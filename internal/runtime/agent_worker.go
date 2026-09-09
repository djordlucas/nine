package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/protocol"
	"nine/internal/toolvm"
)

type turnReq struct {
	ctx        context.Context
	text       string
	respCh     chan turnResp
	trigger    string // journal trigger label; "" defaults to "user"
	forceThink bool   // /think: force native thinking for this turn
}

// replayBuffer is a bounded ring of progress events and the last completed
// response, kept per session so reattaching clients can see what happened
// while they were disconnected.
type replayBuffer struct {
	mu           sync.RWMutex
	events       []protocol.Msg
	lastResponse string
}

const replayBufCap = 200 // max events stored
const replayMaxSend = 50 // max events sent on attach

func (b *replayBuffer) push(msg protocol.Msg) {
	b.mu.Lock()
	if len(b.events) >= replayBufCap {
		b.events = b.events[1:]
	}
	b.events = append(b.events, msg)
	b.mu.Unlock()
}

func (b *replayBuffer) setResponse(text string) {
	b.mu.Lock()
	b.lastResponse = text
	b.mu.Unlock()
}

func (b *replayBuffer) clearResponse() {
	b.mu.Lock()
	b.lastResponse = ""
	b.mu.Unlock()
}

// snapshot returns up to replayMaxSend recent events and the last response.
func (b *replayBuffer) snapshot() (events []protocol.Msg, response string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	n := len(b.events)
	if n > replayMaxSend {
		n = replayMaxSend
	}
	if n > 0 {
		out := make([]protocol.Msg, n)
		copy(out, b.events[len(b.events)-n:])
		return out, b.lastResponse
	}
	return nil, b.lastResponse
}

type turnResp struct {
	text string
	err  error
}

// StallConfig controls when a AgentWorker posts a stall event.
// A stall event fires when stallCount consecutive outer turns complete with
// no tool calls. Set Limit to 0 to disable stall detection.
type StallConfig struct {
	Limit   int                  // consecutive no-tool turns before stall fires
	OnStall func(agentID string) // called on the worker goroutine when stalled
}

// AgentWorker wraps an agent loop and processes user turns sequentially.
type AgentWorker struct {
	id      string
	loop    *agent.Loop
	inbox   chan turnReq
	inspect chan inspectReq // context-inspection requests, served on run()'s goroutine
	// wake carries a condition trigger's finding: a predicate said there is
	// something to look at, so run a turn now rather than at the next clock
	// (docs/scheduling.md). Buffered at 1 and never blocked on — see Wake.
	wake       chan string
	saveCkpt   func(id string, data []byte) error
	getNotif   func(id string) ([]string, error)
	stall      StallConfig
	stallN     int                  // consecutive no-tool turns so far
	quit       chan struct{}        // closed by stop() to signal shutdown to run() and senders
	stopOnce   sync.Once            // guards quit against concurrent/repeated stop()
	stopped    chan struct{}        // closed when run() has exited
	onComplete func(agentID string) // called after each successful turn
	turnN      int                  // 1-based count of turns processed
	replay     replayBuffer
	sink       EventSink // durable session-event journal (adr/event-log.md); nil = disabled

	// Per-turn span cursors for the journal, touched only on the worker
	// goroutine during loop.Run: the current inner-LLM-call number and the
	// running tool-call count within the in-flight turn.
	llmCallN int
	toolN    int

	plan      *sessionPlanState
	idleTimer *time.Timer
	idleSince map[string]time.Time // stage Name -> last idle-check time

	mu         sync.Mutex
	progressFn func(protocol.Msg) // called from worker goroutine on each tool event
	busy       bool               // true while processTurn is running; guarded by mu
	// drainQueuedFn, when set, is called after each turn completes. If it
	// returns a non-empty string, the worker immediately starts another turn
	// with that text. Used to auto-process queued messages left unconsumed by
	// the model (docs/queued-messages.md).
	drainQueuedFn func() (string, error)
}

// IsBusy reports whether the worker is currently processing a turn.
// Thread-safe; checked by the daemon to decide whether to queue a message.
func (w *AgentWorker) IsBusy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy
}

// setProgress registers fn to be called with a tool event protocol.Msg.
// Pass nil to clear. Thread-safe; may be called from any goroutine.
func (w *AgentWorker) setProgress(fn func(protocol.Msg)) {
	w.mu.Lock()
	w.progressFn = fn
	w.mu.Unlock()
}

// SetDrainQueued installs fn to be called after each turn. If fn returns a
// non-empty string, the worker auto-starts another turn with that text. Used
// to drain queued messages the model left unconsumed.
func (w *AgentWorker) SetDrainQueued(fn func() (string, error)) { w.drainQueuedFn = fn }

func (w *AgentWorker) emitEvent(msg protocol.Msg) {
	w.mu.Lock()
	fn := w.progressFn
	w.mu.Unlock()
	if fn != nil {
		fn(msg)
	}
	// Buffer tool and sub-agent events for replay on reattach.
	switch msg.Type {
	case "tool_start", "tool_end", "sub_agent_start", "sub_agent_end":
		w.replay.push(msg)
	}
	// Journal the sub-agent tree. These may arrive from a background sub-agent's
	// own goroutine, so read the turn number under the lock (loop-driven events
	// journal on the worker goroutine and pass turn directly). The sub-agent is
	// its own span, parented to the current turn root.
	switch msg.Type {
	case protocol.TypeSubAgentStart, protocol.TypeSubAgentEnd:
		w.mu.Lock()
		turn := w.turnN
		w.mu.Unlock()
		// Crossing namespaces: a wire message type is reused verbatim as the
		// journal event type. The two vocabularies are separate and only
		// coincide on these names, so the conversion is written out rather than
		// implied.
		w.journal(turn, string(msg.Type), msg.SubAgentID, turnSpan(turn), subAgentPayload{
			SubID:  msg.SubAgentID,
			Task:   msg.Text,
			Status: msg.Status,
			Role:   msg.Role,
		})
	}
}

// errString renders err for a journal payload, "" when nil.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func newAgentWorker(
	id string,
	loop *agent.Loop,
	saveCkpt func(string, []byte) error,
	getNotif func(string) ([]string, error),
	stall StallConfig,
	plan *sessionPlanState,
	sink EventSink,
) *AgentWorker {
	if plan == nil {
		plan = defaultPlanState()
	}
	now := time.Now()
	idleSince := make(map[string]time.Time, len(plan.plan.Routines))
	for _, st := range plan.plan.Routines {
		idleSince[st.Name] = now
	}
	w := &AgentWorker{
		id:        id,
		loop:      loop,
		inbox:     make(chan turnReq, 1),
		inspect:   make(chan inspectReq),
		wake:      make(chan string, 1),
		saveCkpt:  saveCkpt,
		getNotif:  getNotif,
		stall:     stall,
		quit:      make(chan struct{}),
		stopped:   make(chan struct{}),
		plan:      plan,
		idleSince: idleSince,
		sink:      sink,
	}
	go w.run()
	return w
}

// defaultPlanState returns an unpersisted [active]-profile plan, used when no
// PlanStore is configured (e.g. in tests).
func defaultPlanState() *sessionPlanState {
	now := time.Now().UTC().Format(time.RFC3339)
	plan := &memory.SessionPlan{
		Status: "active",
		Routines: []memory.SessionRoutine{
			{Name: "active", Kind: "active", Status: "active", UpdatedAt: now},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	handlers, _ := initRoutines(context.Background(), "", plan.Routines) //nolint:errcheck // "active" is always registered
	return &sessionPlanState{plan: plan, handlers: handlers, persisted: true}
}

func (w *AgentWorker) run() {
	defer close(w.stopped)
	defer func() {
		if w.idleTimer != nil {
			w.idleTimer.Stop()
		}
	}()
	w.armIdleTimer()
	for {
		var timerC <-chan time.Time
		if w.idleTimer != nil {
			timerC = w.idleTimer.C
		}
		select {
		case req := <-w.inbox:
			w.processTurn(req)
			w.drainQueued()
		case ir := <-w.inspect:
			ir.respCh <- w.loop.InspectContext(ir.ctx)
		case text := <-w.wake:
			w.processTurn(turnReq{ctx: context.Background(), text: text, trigger: "condition"})
			w.drainQueued()
		case <-timerC:
			w.handleIdle()
		case <-w.quit:
			return
		}
	}
}

// drainQueued processes any queued messages the model left unconsumed after a
// turn. It loops: each remaining unconsumed message becomes its own turn,
// until none are left. This runs on the worker goroutine, so there is no
// concurrency with the turn itself.
func (w *AgentWorker) drainQueued() {
	if w.drainQueuedFn == nil {
		return
	}
	for {
		text, err := w.drainQueuedFn()
		if err != nil || text == "" {
			return
		}
		slog.Info("auto-processing queued message", "agent_id", w.id)
		w.processTurn(turnReq{
			ctx:    context.Background(),
			text:   text,
			respCh: make(chan turnResp, 1),
		})
	}
}

// processTurn runs one turn through the agent loop, then runs the
// end-of-turn hooks: stall detection, stage OnTurnEnd, checkpointing, and
// rearming the idle scheduler.
func (w *AgentWorker) processTurn(req turnReq) {
	w.mu.Lock()
	w.turnN++
	turn := w.turnN
	w.busy = true
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		w.busy = false
		w.mu.Unlock()
	}()
	w.llmCallN = 0
	w.toolN = 0
	w.replay.clearResponse() // new turn supersedes any buffered response
	turnStart := time.Now()
	text := w.prependNotifications(req.ctx, req.text)
	trigger := req.trigger
	if trigger == "" {
		trigger = "user"
	}
	w.journal(turn, "turn_start", turnSpan(turn), "", turnStartPayload{Input: text, Trigger: trigger})
	w.loop.SetHooks(w.turnHooks(turn))
	slog.Debug("turn_start", "agent_id", w.id, "turn_n", turn)
	w.loop.SetForceThinkNextTurn(req.forceThink)
	// Both of these travel with the turn rather than being configured at boot,
	// because the sandboxed-tool host is daemon-wide: the journal destination for
	// a tool's outbound HTTP, and the conversation a conversation-scoped state
	// grant resolves against.
	toolCtx := toolvm.WithHTTPAudit(req.ctx, w.httpAuditor(turn))
	toolCtx = toolvm.WithStateScope(toolCtx, w.id)
	result, err := w.loop.Run(toolCtx, text)
	w.loop.ClearHooks()
	w.journal(turn, "turn_end", turnSpan(turn), "", turnEndPayload{
		Result:     result,
		Error:      errString(err),
		ToolCount:  w.loop.LastRunToolCount(),
		DurationMs: time.Since(turnStart).Milliseconds(),
	})
	if err != nil {
		slog.Warn("turn_error", "agent_id", w.id, "turn_n", w.turnN, "err", err)
	} else {
		w.replay.setResponse(result)
		slog.Debug("turn_done",
			"agent_id", w.id,
			"turn_n", w.turnN,
			"tools_used", w.loop.LastRunToolCount(),
			"duration_ms", time.Since(turnStart).Milliseconds(),
		)
	}
	w.notifyRoutines(req.ctx, result, err)
	w.checkStall(req.ctx)
	w.checkpoint()
	w.armIdleTimer()
	req.respCh <- turnResp{text: result, err: err}
	if w.onComplete != nil {
		w.onComplete(w.id)
	}
}

// turnHooks assembles the full observer set for one turn: the progress events a
// client sees live, and the journal events that outlive the session. Both are
// built here because both close over `turn` — the number every span id derives
// from — and the Loop takes them as one set.
//
// These run on the worker goroutine (the loop calls them synchronously), so
// llmCallN/toolN need no locking. The one exception is OnStage, which the Loop
// guards itself because a queue wait is reported from the queue's goroutine.
func (w *AgentWorker) turnHooks(turn int) agent.Hooks {
	root := turnSpan(turn)
	return agent.Hooks{
		OnContextUpdate: func(used, budget int) {
			w.emitEvent(protocol.NewContextUpdateMsg(w.id, used, budget))
			w.journal(turn, "context_update", root, "", contextPayload{Used: used, Budget: budget})
		},
		OnToolStart: func(name, displayName, backend string, input json.RawMessage) {
			w.emitEvent(protocol.NewToolStartMsg(w.id, name, displayName, backend, input))
			w.journalToolStart(turn, name, input)
		},
		OnToolEnd: func(name, displayName, backend string, input json.RawMessage, out agent.ToolOutcome) {
			w.emitEvent(protocol.NewToolEndMsg(w.id, name, displayName, backend, input, out.Output))
			w.journalToolEnd(turn, name, input, out)
		},
		OnChunk: func(chunk string) {
			w.emitEvent(protocol.NewResponseChunkMsg(w.id, chunk))
		},
		OnThinkingChunk: func(chunk string) {
			w.emitEvent(protocol.NewThinkingChunkMsg(w.id, chunk))
		},
		OnThinking: func(n int, think bool) {
			w.emitEvent(protocol.NewThinkingMsg(w.id, n, think))
			w.journal(turn, "thinking", llmSpan(turn, n), root, thinkingPayload{LLMCallN: n, Think: think})
		},
		OnStage:     func(label string) { w.emitEvent(protocol.NewStageMsg(w.id, label)) },
		OnPlanStart: func() { w.emitEvent(protocol.NewPlanStartMsg(w.id)) },
		OnPlanEnd:   func() { w.emitEvent(protocol.NewPlanEndMsg(w.id)) },
		OnNotice:    func(text string) { w.emitEvent(protocol.NewNoticeMsg(w.id, text)) },

		OnLLMRequest: func(req *llm.Request, tokensUsed, budget, llmCallN int) {
			w.llmCallN = llmCallN
			names := make([]string, len(req.Tools))
			for i, t := range req.Tools {
				names[i] = t.Name
			}
			w.journal(turn, "llm_request", llmSpan(turn, llmCallN), root, llmRequestPayload{
				System:     req.System,
				Messages:   req.Messages,
				ToolNames:  names,
				MaxTokens:  req.MaxTokens,
				TokensUsed: tokensUsed,
				Budget:     budget,
				LLMCallN:   llmCallN,
			})
		},
		OnLLMResponse: func(resp *llm.Response, llmCallN int) {
			w.journal(turn, "llm_response", llmSpan(turn, llmCallN), root, llmResponsePayload{
				Text:         resp.Text,
				ToolCalls:    resp.ToolCalls,
				StopReason:   resp.StopReason,
				LLMCallN:     llmCallN,
				InputTokens:  resp.Usage.InputTokens,
				OutputTokens: resp.Usage.OutputTokens,
			})
		},
	}
}

func (w *AgentWorker) checkStall(ctx context.Context) {
	if w.stall.Limit <= 0 {
		return
	}
	if w.loop.LastRunToolCount() == 0 {
		w.stallN++
		if w.stallN >= w.stall.Limit {
			slog.Warn("agent stalled", "agent_id", w.id, "consecutive_no_tool_turns", w.stallN)
			w.stallN = 0
			w.notifyRoutines(ctx, "", ErrStall)
			if w.stall.OnStall != nil {
				w.stall.OnStall(w.id)
			}
		}
	} else {
		w.stallN = 0
	}
}

// notifyRoutines calls OnTurnEnd on every stage with Status == "active",
// then persists/refreshes the plan. Stages that mutate their own
// Status/Result do so by writing their session_plans row directly; the
// refresh picks up those changes so the idle scheduler sees them.
func (w *AgentWorker) notifyRoutines(ctx context.Context, result string, err error) {
	if w.plan == nil {
		return
	}
	for _, st := range w.plan.plan.Routines {
		if st.Status != "active" {
			continue
		}
		h, ok := w.plan.handlers[st.Name]
		if !ok {
			continue
		}
		if hErr := h.OnTurnEnd(ctx, w.id, result, err); hErr != nil {
			slog.Warn("stage OnTurnEnd failed", "agent_id", w.id, "stage", st.Name, "err", hErr)
		}
	}
	if perr := w.persistPlan(); perr != nil {
		slog.Warn("session plan persist failed", "agent_id", w.id, "err", perr)
	}
}

// persistPlan writes a freshly-created plan on first use, or refreshes the
// cached plan from the store on subsequent calls (picking up any
// stage-driven Status/Result/Config changes).
func (w *AgentWorker) persistPlan() error {
	if w.plan == nil || w.plan.save == nil {
		return nil
	}
	if !w.plan.persisted {
		if err := w.plan.save(w.plan.plan); err != nil {
			return err
		}
		w.plan.persisted = true
		return nil
	}
	if w.plan.load == nil {
		return nil
	}
	refreshed, err := w.plan.load(w.id)
	if err != nil {
		return err
	}
	if refreshed != nil {
		w.plan.plan = refreshed
	}
	return nil
}

// armIdleTimer (re)computes the worker's idle timer from the minimum
// remaining idle_interval across this session's active, idle-capable routines.
// It stops any existing timer first; if the plan is paused/archived or no
// stage qualifies, no timer is armed.
func (w *AgentWorker) armIdleTimer() {
	if w.idleTimer != nil {
		w.idleTimer.Stop()
		w.idleTimer = nil
	}
	if w.plan == nil || w.plan.plan.Status != "active" {
		return
	}
	now := time.Now()
	var next time.Duration
	have := false
	for _, st := range w.plan.plan.Routines {
		if st.Status != "active" {
			continue
		}
		remaining, ok := routineNextWake(st.Config, w.idleSince[st.Name], now)
		if !ok {
			continue
		}
		if !have || remaining < next {
			next = remaining
			have = true
		}
	}
	if have {
		w.idleTimer = time.NewTimer(next)
	}
}

// handleIdle is called when the idle timer fires. It runs at most one turn, from
// the due stage that has been waiting longest, and rearms either way.
//
// Order is by overdueness, not by position in the Stages array. I1 allows only
// one turn at a time, so when several stages are due one must be chosen, and
// choosing by array order starves the others: a 60s routine listed before a 3600s
// one comes due again long before the slow stage is ever reached, so the slow
// stage can wait indefinitely. Whether a session makes progress on all its
// routines would otherwise depend on the order its stages happened to be
// serialized in.
//
// A stage that is due but has no work still yields to the next-most-overdue one,
// which is why this is a sorted walk rather than a single pick.
func (w *AgentWorker) handleIdle() {
	if w.plan == nil {
		return
	}
	ctx := context.Background()
	now := time.Now()

	type dueRoutine struct {
		name    string
		overdue time.Duration
	}
	var due []dueRoutine
	for _, st := range w.plan.plan.Routines {
		if st.Status != "active" {
			continue
		}
		overdue, ok := routineOverdueBy(st.Config, w.idleSince[st.Name], now)
		if !ok {
			continue
		}
		due = append(due, dueRoutine{name: st.Name, overdue: overdue})
	}
	// Longest-waiting first. Equal overdueness keeps the array order, so a
	// single-routine plan and simultaneous wakes behave exactly as before.
	sort.SliceStable(due, func(i, j int) bool { return due[i].overdue > due[j].overdue })

	for _, d := range due {
		// Mark every stage considered as fired, not just the one that runs.
		// Otherwise a stage whose OnIdle declines stays overdue and keeps
		// winning the sort, and the stages behind it never run.
		w.idleSince[d.name] = now
		h, ok := w.plan.handlers[d.name]
		if !ok {
			continue
		}
		text, ok := h.OnIdle(ctx, w.id)
		if !ok {
			continue
		}
		w.processTurn(turnReq{ctx: ctx, text: text, respCh: make(chan turnResp, 1), trigger: "idle"})
		return
	}
	// No stage produced idle work. Refresh the cached plan so any stage-status
	// change an OnIdle handler wrote directly (e.g. the pursue routine retiring
	// itself once its goal is no longer active) is reflected before we decide
	// whether to re-arm — otherwise a since-retired stage keeps arming the timer
	// and keeps counting against MaxGoalSessions (activeGoalSessionCount reads
	// this cached plan).
	if err := w.persistPlan(); err != nil {
		slog.Warn("session plan refresh failed", "agent_id", w.id, "err", err)
	}
	w.armIdleTimer()
}

// stop signals shutdown and waits for the run goroutine to exit. It never
// closes the inbox — senders may be racing a send against shutdown — so
// closing quit (once, even under concurrent calls) is the sole signal. Both
// run() and any blocked sender select on it.
// Wake asks the worker to run a turn now, with text as its input.
//
// Non-blocking and lossy on purpose. The buffer holds one pending wake: if the
// agent is mid-turn, or a wake is already queued, this drops. That is the right
// failure — a condition that fires twice while the agent is still reading the
// first finding does not want two turns, it wants the agent to look, and the
// second finding will still be there when it does. Blocking here would instead
// let a chatty predicate stall the evaluator that produced it.
func (w *AgentWorker) Wake(text string) bool {
	select {
	case w.wake <- text:
		return true
	default:
		return false
	}
}

func (w *AgentWorker) stop() {
	w.stopOnce.Do(func() { close(w.quit) })
	<-w.stopped
}

// turnAsync submits a user message and returns a channel that receives the
// response when the worker finishes. The caller must not call turnAsync again
// until it has read from the returned channel.
func (w *AgentWorker) turnAsync(ctx context.Context, text string, forceThink bool) (<-chan turnResp, error) {
	ch := make(chan turnResp, 1)
	select {
	case w.inbox <- turnReq{ctx: ctx, text: text, respCh: ch, forceThink: forceThink}:
		return ch, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-w.quit:
		return nil, context.Canceled
	}
}

// turn submits a user message and blocks until the response is ready.
func (w *AgentWorker) turn(ctx context.Context, text string) (string, error) {
	ch := make(chan turnResp, 1)
	select {
	case w.inbox <- turnReq{ctx: ctx, text: text, respCh: ch}:
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.quit:
		return "", context.Canceled
	}
	select {
	case res := <-ch:
		return res.text, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.quit:
		// Shutdown began before the enqueued turn was picked up; the worker
		// won't deliver a response. Prefer a result that already landed.
		select {
		case res := <-ch:
			return res.text, res.err
		default:
			return "", context.Canceled
		}
	}
}

// inspectReq asks the run goroutine to snapshot the session's assembled context.
type inspectReq struct {
	ctx    context.Context
	respCh chan ninectx.Report
}

// InspectContext returns a breakdown of the session's currently-assembled
// context. It is served on the worker's run goroutine, so it is race-free with
// turns and never overlaps a loop.Run. It performs no LLM call. Returns an error
// only if the worker is shutting down.
func (w *AgentWorker) InspectContext(ctx context.Context) (ninectx.Report, error) {
	ch := make(chan ninectx.Report, 1)
	select {
	case w.inspect <- inspectReq{ctx: ctx, respCh: ch}:
	case <-ctx.Done():
		return ninectx.Report{}, ctx.Err()
	case <-w.quit:
		return ninectx.Report{}, context.Canceled
	}
	select {
	case rep := <-ch:
		return rep, nil
	case <-ctx.Done():
		return ninectx.Report{}, ctx.Err()
	case <-w.quit:
		return ninectx.Report{}, context.Canceled
	}
}

func (w *AgentWorker) prependNotifications(_ context.Context, text string) string {
	if w.getNotif == nil {
		return text
	}
	notifs, err := w.getNotif(w.id)
	if err != nil || len(notifs) == 0 {
		return text
	}
	var sb strings.Builder
	sb.WriteString("[Notifications]\n")
	for _, n := range notifs {
		sb.WriteString("- ")
		sb.WriteString(n)
		sb.WriteString("\n")
	}
	sb.WriteString("\n")
	sb.WriteString(text)
	return sb.String()
}

func (w *AgentWorker) checkpoint() {
	if w.saveCkpt == nil {
		return
	}
	data, err := w.loop.SaveState()
	if err != nil {
		slog.Warn("checkpoint marshal failed", "agent_id", w.id, "err", err)
		return
	}
	if err := w.saveCkpt(w.id, data); err != nil {
		slog.Warn("checkpoint save failed", "agent_id", w.id, "err", err)
	}
}

// setPlanMode changes the loop's reasoning mode live. Safe to call concurrently
// with a running turn (the loop guards the mode); it takes effect next turn.
func (w *AgentWorker) setPlanMode(mode string) { w.loop.SetPlanMode(mode) }

// planMode reports the loop's current reasoning mode (surfaced in status).
func (w *AgentWorker) planMode() string { return w.loop.PlanMode() }

// role reports the resolved role the session runs (surfaced in status and the
// TUI header).
func (w *AgentWorker) role() string { return w.loop.Role() }
