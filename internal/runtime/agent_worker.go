package runtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"nine/internal/agent"
	ninectx "nine/internal/context"
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
	id         string
	loop       *agent.Loop
	inbox      chan turnReq
	inspect    chan inspectReq // context-inspection requests, served on run()'s goroutine
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
	sink       EventSink // durable session-event journal (docs/event-log.md); nil = disabled

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
}

// setProgress registers fn to be called with a tool event protocol.Msg.
// Pass nil to clear. Thread-safe; may be called from any goroutine.
func (w *AgentWorker) setProgress(fn func(protocol.Msg)) {
	w.mu.Lock()
	w.progressFn = fn
	w.mu.Unlock()
}

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
	case "sub_agent_start", "sub_agent_end":
		w.mu.Lock()
		turn := w.turnN
		w.mu.Unlock()
		w.journal(turn, msg.Type, msg.SubAgentID, turnSpan(turn), subAgentPayload{
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
	idleSince := make(map[string]time.Time, len(plan.plan.Stages))
	for _, st := range plan.plan.Stages {
		idleSince[st.Name] = now
	}
	w := &AgentWorker{
		id:        id,
		loop:      loop,
		inbox:     make(chan turnReq, 1),
		inspect:   make(chan inspectReq),
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
		Stages: []memory.SessionStage{
			{Name: "active", Kind: "active", Status: "active", UpdatedAt: now},
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	handlers, _ := initStages(context.Background(), "", plan.Stages) //nolint:errcheck // "active" is always registered
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
		case ir := <-w.inspect:
			ir.respCh <- w.loop.InspectContext(ir.ctx)
		case <-timerC:
			w.handleIdle()
		case <-w.quit:
			return
		}
	}
}

// processTurn runs one turn through the agent loop, then runs the
// end-of-turn hooks: stall detection, stage OnTurnEnd, checkpointing, and
// rearming the idle scheduler.
func (w *AgentWorker) processTurn(req turnReq) {
	w.mu.Lock()
	w.turnN++
	turn := w.turnN
	w.mu.Unlock()
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
	w.loop.SetOnContextUpdate(func(used, budget int) {
		w.emitEvent(protocol.NewContextUpdateMsg(w.id, used, budget))
		w.journal(turn, "context_update", turnSpan(turn), "", contextPayload{Used: used, Budget: budget})
	})
	w.loop.SetOnToolStart(func(name, displayName string, input json.RawMessage) {
		w.emitEvent(protocol.NewToolStartMsg(w.id, name, displayName, input))
		w.journalToolStart(turn, name, input)
	})
	w.loop.SetOnToolEnd(func(name, displayName string, input json.RawMessage, out agent.ToolOutcome) {
		w.emitEvent(protocol.NewToolEndMsg(w.id, name, displayName, input, out.Output))
		w.journalToolEnd(turn, name, input, out)
	})
	w.loop.SetOnChunk(func(chunk string) {
		w.emitEvent(protocol.NewResponseChunkMsg(w.id, chunk))
	})
	w.loop.SetOnThinkingChunk(func(chunk string) {
		w.emitEvent(protocol.NewThinkingChunkMsg(w.id, chunk))
	})
	w.loop.SetOnThinking(func(n int, think bool) {
		w.emitEvent(protocol.NewThinkingMsg(w.id, n, think))
		w.journal(turn, "thinking", llmSpan(turn, n), turnSpan(turn), thinkingPayload{LLMCallN: n, Think: think})
	})
	w.loop.SetOnStage(func(label string) { w.emitEvent(protocol.NewStageMsg(w.id, label)) })
	w.loop.SetOnPlanStart(func() { w.emitEvent(protocol.NewPlanStartMsg(w.id)) })
	w.loop.SetOnPlanEnd(func() { w.emitEvent(protocol.NewPlanEndMsg(w.id)) })
	w.loop.SetOnNotice(func(text string) { w.emitEvent(protocol.NewNoticeMsg(w.id, text)) })
	w.wireJournalHooks(turn)
	slog.Debug("turn_start", "agent_id", w.id, "turn_n", turn)
	w.loop.SetForceThinkNextTurn(req.forceThink)
	// The sandboxed-tool host is daemon-wide, so the journal destination for a
	// tool's outbound HTTP has to travel with the turn rather than be configured
	// once at boot.
	result, err := w.loop.Run(toolvm.WithHTTPAudit(req.ctx, w.httpAuditor(turn)), text)
	w.loop.SetOnContextUpdate(nil)
	w.loop.SetOnToolStart(nil)
	w.loop.SetOnToolEnd(nil)
	w.loop.SetOnChunk(nil)
	w.loop.SetOnThinkingChunk(nil)
	w.loop.SetOnThinking(nil)
	w.loop.SetOnPlanStart(nil)
	w.loop.SetOnPlanEnd(nil)
	w.loop.SetOnNotice(nil)
	w.loop.SetOnStage(nil)
	w.clearJournalHooks()
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
	w.notifyStages(req.ctx, result, err)
	w.checkStall(req.ctx)
	w.checkpoint()
	w.armIdleTimer()
	req.respCh <- turnResp{text: result, err: err}
	if w.onComplete != nil {
		w.onComplete(w.id)
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
			w.notifyStages(ctx, "", ErrStall)
			if w.stall.OnStall != nil {
				w.stall.OnStall(w.id)
			}
		}
	} else {
		w.stallN = 0
	}
}

// notifyStages calls OnTurnEnd on every stage with Status == "active",
// then persists/refreshes the plan. Stages that mutate their own
// Status/Result do so by writing their session_plans row directly; the
// refresh picks up those changes so the idle scheduler sees them.
func (w *AgentWorker) notifyStages(ctx context.Context, result string, err error) {
	if w.plan == nil {
		return
	}
	for _, st := range w.plan.plan.Stages {
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
// remaining idle_interval across this session's active, idle-capable stages.
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
	for _, st := range w.plan.plan.Stages {
		if st.Status != "active" {
			continue
		}
		remaining, ok := stageNextWake(st.Config, w.idleSince[st.Name], now)
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

// handleIdle is called when the idle timer fires. It finds the first active,
// idle-capable stage whose interval has elapsed, calls its OnIdle, and (if
// it has work to do) runs the returned text as the session's next turn.
// Either way, the scheduler is rearmed for the next cycle.
func (w *AgentWorker) handleIdle() {
	if w.plan == nil {
		return
	}
	ctx := context.Background()
	now := time.Now()
	for _, st := range w.plan.plan.Stages {
		if st.Status != "active" {
			continue
		}
		remaining, ok := stageNextWake(st.Config, w.idleSince[st.Name], now)
		if !ok || remaining > 0 {
			continue
		}
		w.idleSince[st.Name] = now
		h, ok := w.plan.handlers[st.Name]
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
	// change an OnIdle handler wrote directly (e.g. the pursue stage retiring
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
