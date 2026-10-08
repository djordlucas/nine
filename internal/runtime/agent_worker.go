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
	"nine/internal/llm"
	"nine/internal/protocol"
	"nine/internal/toolvm"
)

type turnReq struct {
	ctx        context.Context
	text       string
	respCh     chan turnResp
	trigger    string // journal trigger label; "" defaults to "user"
	forceThink bool   // /think: force native thinking for this turn
	// allow, when set, restricts the turn to these tools (PipedTurnTools for
	// a turn a pipe caused).
	allow []string
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
	// tokens is what the turn's model calls spent, input and output: what a
	// process's budget counts.
	tokens int
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
	// turnTokens sums the in-flight turn's model usage, input and output.
	turnTokens int
	// workspaceRoot is [workspace].root, for a piped turn's write guard.
	workspaceRoot string

	mu         sync.Mutex
	progressFn func(protocol.Msg) // called from worker goroutine on each tool event
	// watchers follow every turn the session runs, whoever started it — the
	// feed behind the "watch" message (and so the HTTP API's event stream).
	// Unlike progressFn, which belongs to the one connection driving the
	// current turn, there may be any number, and they also receive each turn's
	// outcome. Guarded by mu.
	watchers map[uint64]func(protocol.Msg)
	watcherN uint64
	busy     bool // true from turn start until its reply is sent; guarded by mu
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

// addWatcher registers fn to receive every progress event and every turn's
// outcome ("response" or "error", then "done") until the returned func is
// called. fn runs on the emitting goroutine, so it must not block.
func (w *AgentWorker) addWatcher(fn func(protocol.Msg)) (remove func()) {
	w.mu.Lock()
	if w.watchers == nil {
		w.watchers = make(map[uint64]func(protocol.Msg))
	}
	w.watcherN++
	key := w.watcherN
	w.watchers[key] = fn
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		delete(w.watchers, key)
		w.mu.Unlock()
	}
}

// notifyWatchers delivers msg to every registered watcher.
func (w *AgentWorker) notifyWatchers(msg protocol.Msg) {
	w.mu.Lock()
	fns := make([]func(protocol.Msg), 0, len(w.watchers))
	for _, fn := range w.watchers {
		fns = append(fns, fn)
	}
	w.mu.Unlock()
	for _, fn := range fns {
		fn(msg)
	}
}

// notifyTurnOutcome tells watchers how a turn ended. The connection that drove
// the turn gets its outcome from the daemon's turn handler instead; this is for
// everyone else, including turns no client started (idle wakes, queued drains).
func (w *AgentWorker) notifyTurnOutcome(result string, err error) {
	if err != nil {
		failed := protocol.NewAgentErrorMsg(w.id, err.Error())
		failed.Status = protocol.StatusTurnFailed
		w.notifyWatchers(failed)
	} else {
		w.notifyWatchers(protocol.NewResponseMsg(w.id, result))
	}
	done := protocol.NewDoneMsg(w.id)
	done.Timestamp = time.Now().UnixMilli()
	w.notifyWatchers(done)
}

func (w *AgentWorker) emitEvent(msg protocol.Msg) {
	w.mu.Lock()
	fn := w.progressFn
	w.mu.Unlock()
	if fn != nil {
		fn(msg)
	}
	w.notifyWatchers(msg)
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
	sink EventSink,
) *AgentWorker {
	w := &AgentWorker{
		id:       id,
		loop:     loop,
		inbox:    make(chan turnReq, 1),
		inspect:  make(chan inspectReq),
		wake:     make(chan string, 1),
		saveCkpt: saveCkpt,
		getNotif: getNotif,
		stall:    stall,
		quit:     make(chan struct{}),
		stopped:  make(chan struct{}),
		sink:     sink,
	}
	go w.run()
	return w
}

// run serves the worker's turns until it is stopped. A worker never starts a
// turn of its own: a person, a process (adr/process-sessions.md) or a wake
// submits each one.
func (w *AgentWorker) run() {
	defer close(w.stopped)
	for {
		select {
		case req := <-w.inbox:
			w.processTurn(req)
			w.drainQueued()
		case ir := <-w.inspect:
			ir.respCh <- w.loop.InspectContext(ir.ctx)
		case text := <-w.wake:
			// Nobody waits on a woken turn's reply, but processTurn delivers one; a
			// buffered channel takes it so the worker is free for the next turn.
			// A woken turn is a pipe's report reaching this session, so it runs
			// restricted (PipedTurnTools).
			w.processTurn(turnReq{ctx: context.Background(), text: text, respCh: make(chan turnResp, 1),
				trigger: "condition", allow: PipedTurnTools})
			w.drainQueued()
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
// end-of-turn hooks: stall detection and checkpointing.
func (w *AgentWorker) processTurn(req turnReq) {
	w.mu.Lock()
	w.turnN++
	turn := w.turnN
	w.busy = true
	w.mu.Unlock()
	w.llmCallN = 0
	w.toolN = 0
	w.turnTokens = 0
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
	if req.allow != nil {
		w.loop.RestrictNextTurn(pipedTurn(req.allow, w.workspaceRoot))
	}
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
	w.checkStall(req.ctx)
	w.checkpoint()
	// Clear busy before delivering the reply, not after: a client that sends its
	// next message the moment this reply lands must get a turn of its own. Were
	// busy still set, the daemon would queue that message and drainQueued would
	// run it with no one listening for the reply. A turn arriving now waits in
	// the inbox until this goroutine is free, so nothing runs concurrently.
	w.mu.Lock()
	w.busy = false
	w.mu.Unlock()
	w.notifyTurnOutcome(result, err)
	req.respCh <- turnResp{text: result, err: err, tokens: w.turnTokens}
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
			w.turnTokens += resp.Usage.InputTokens + resp.Usage.OutputTokens
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
			if w.stall.OnStall != nil {
				w.stall.OnStall(w.id)
			}
		}
	} else {
		w.stallN = 0
	}
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
	return w.turnAs(ctx, text, "")
}

// turnAs is turn with the journal's trigger label: "" for a user's turn, or
// what drove it ("idle", "condition") for a process's.
func (w *AgentWorker) turnAs(ctx context.Context, text, trigger string) (string, error) {
	res := w.turnCounted(ctx, text, trigger, nil)
	return res.text, res.err
}

// turnCounted is turnAs with the tokens the turn spent; allow, when set,
// restricts the turn's tools.
func (w *AgentWorker) turnCounted(ctx context.Context, text, trigger string, allow []string) turnResp {
	ch := make(chan turnResp, 1)
	select {
	case w.inbox <- turnReq{ctx: ctx, text: text, respCh: ch, trigger: trigger, allow: allow}:
	case <-ctx.Done():
		return turnResp{err: ctx.Err()}
	case <-w.quit:
		return turnResp{err: context.Canceled}
	}
	select {
	case res := <-ch:
		return res
	case <-ctx.Done():
		return turnResp{err: ctx.Err()}
	case <-w.quit:
		// Shutdown began before the enqueued turn was picked up; the worker
		// won't deliver a response. Prefer a result that already landed.
		select {
		case res := <-ch:
			return res
		default:
			return turnResp{err: context.Canceled}
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
