package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	ninectx "nine/internal/context"
	"nine/internal/embed"
	"nine/internal/llm"
)

const analysisMaxTokens = 512

// Config holds static configuration for an agent loop.
type Config struct {
	// Role is the resolved role name this loop runs (orchestrator, executor,
	// pursue, …). Descriptive only — the tool boundary and persona are already
	// baked into the loop at build time; this is carried so the daemon can
	// surface which role a session or sub-agent is running (status, TUI header).
	Role         string
	SystemCore   string
	SystemExtras string
	Priority     int // llm.PrioritySupervisor / PriorityConversation / PriorityBackground
	MaxTokens    int // per-completion token limit; 0 → 4096
	Tools        []ninectx.ToolWithVector
	Embedder     embed.Embedder                                       // nil = no query embedding / tool ranking
	SelfModelFn  func(ctx context.Context, queryVec []float32) string // nil = no self-model
	// EnrichmentFn optionally returns pull-surfaced enrichment for the current
	// query — e.g. a related prior session recorded out-of-band by the reactive
	// subscriber (docs/reactive-events.md §5) and/or stored memories relevant to
	// the turn, composed by the runtime. It returns "" when nothing is relevant.
	// nil = no enrichment. Computed once per turn, off no extra path.
	EnrichmentFn func(ctx context.Context, queryVec []float32) string

	// ThinkPolicy decices, by inner LLM call, whether to request native extended reasoning ("thinking") from the model.
	// It receives the 1-based inner LLM call index and a boolean indicating whether the user explicitly requested thinking for this turn.
	// Nil leaves the decision to the provider's default behavior (the Provider.thinking field).
	// See DefaultThinkPolicy for the "plan then execute" default
	ThinkPolicy func(llmCallN int, forced bool) bool

	// Analysis prompt is the persona for the no-tool request analysis pass
	// run before the main loop when the model lacks native thinking.
	// Empty disabled the pass entirely (the mecanism is off). The runtime
	// sources from skills/roles/analysis.md
	AnalysisPrompt string

	// PlanMode is the initial reasoning mode (off | plan-only | always). Empty
	// defaults to plan-only. Live-switchable per session via SetPlanMode.
	PlanMode string

	// PlanReviewFn, when non-nil, is a function that's invoked between the analysis pass and
	// the execution loop with the raw plan text. The bool Proceed flag decides to run the plan as-is;
	// a rejection with a clarification folds that text in as the new user message and re-runs the analysis.
	// Nil disables the checkpoint (the low-prority SystemPlan block is then the only guard)
	// The runtime wires this to HITL.Ask for interactive sessions only.
	PlanReviewFn func(ctx context.Context, plan string) (PlanDecision, error)
}

// PlanDecision is the outcome of a PlanReviewFn checkpoint.
// Proceed=true runs the plan;
// Proceed=false with a non-empty clarification re-runs the analysis pass with the guidance folded
// in a user message
type PlanDecision struct {
	Proceed       bool
	Clarification string
}

// DefaultThinkPolicy is the default inner-call thinking policy: the first inner call (the "plan" phase) requests thinking, subsequent calls (the "execute" phase) do not.
func DefaultThinkPolicy(llmCallN int, forced bool) bool {
	return forced || llmCallN == 1 // first inner call is the "plan" phase; subsequent calls are "execute"
}

const (
	PlanModeOff      = "off"
	PlanModePlanOnly = "plan-only"
	PlanModeAlways   = "always"
)

// Stage labels reported via SetOnStage. They name the phases of a turn that run
// before the model produces anything, so the UI isn't blank while they happen.
// An empty label means no phase is active and the client falls back to its own
// status text — runAnalysisPass relies on this to surface a queue wait without
// masking "planning".
const (
	StageRecall  = "recalling memory"
	StageContext = "building context"
	StageQueued  = "queued"
	StageModel   = "waiting for the model"
)

// Loop is a stateful ReAct agent loop. It is not safe for concurrent use.
type Loop struct {
	cfg Config

	planModeMu sync.RWMutex
	planMode   string // off | plan-only | always; guarded by planModeMu (live-switchable)

	// stageMu guards onStage alone. Every other callback fires from inside Run,
	// but a queue wait is reported from the LLM queue's goroutine, which can
	// outlive the Run that registered it (see wireQueueStages).
	stageMu sync.Mutex

	builder    *ninectx.Builder
	queue      *llm.Queue
	dispatcher *Dispatcher

	history            []llm.Message
	scratchpad         []ninectx.ScratchpadEntry
	lastToolCount      int  // tool calls dispatched in the most recent Run()
	forceThinkNextTurn bool // set by SetForceThinkNextTurn; consumed + reset each Run (/think)
	displayNames       map[string]string
	onToolStart        func(name, displayName string, input json.RawMessage)
	onToolEnd          func(name, displayName string, input json.RawMessage, out ToolOutcome)
	onContextUpdate    func(used, budget int)
	onChunk            func(string)
	onThinkingChunk    func(string)
	onThinking         func(llmCallN int, think bool)
	onPlanStart        func()
	onPlanEnd          func()
	onNotice           func(text string)
	downgradeNoticed   bool // one-shot per session: a thinking-downgrade notice was emitted
	onLLMRequest       func(req *llm.Request, tokensUsed, budget, llmCallN int)
	onLLMResponse      func(resp *llm.Response, llmCallN int)
	onStage            func(string) // guarded by stageMu
}

// ToolOutcome carries the full result of a dispatched tool call for observers
// (the durable event journal, docs/event-log.md §7.3). Output is exactly what
// the model saw as the observation — the tool's text on success, or the
// instructive failure notice when Err is set.
type ToolOutcome struct {
	Output    string
	Truncated bool
	// SpillPath is where the full output was stored when it exceeded the cap,
	// and OutputChars how long that output was. The journal records the pointer
	// rather than the payload: the bytes already live in the file store, so
	// duplicating them into the event log would double the cost of every large
	// result (docs/tool-output-spill.md §4).
	SpillPath   string
	OutputChars int
	DurationMs  int64
	Attempts    int    // total dispatch attempts (1 = succeeded on first try)
	Err         string // "" when the call ultimately succeeded
}

// SetForceThinkNextTurn forces native thinking for the whole of the next Run
// (the /think command), consumed and reset after that turn. Safe to call between
// turns; not safe during Run.
func (l *Loop) SetForceThinkNextTurn(v bool) { l.forceThinkNextTurn = v }

// SetQueue replaces the LLM queue used for subsequent Run calls.
// Safe to call between turns; not safe during Run.
func (l *Loop) SetQueue(q *llm.Queue) { l.queue = q }

// SetOnContextUpdate registers a callback invoked after each context assembly,
// reporting the estimated tokens used and the total budget.
// Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnContextUpdate(fn func(used, budget int)) { l.onContextUpdate = fn }

// SetOnToolStart registers a callback invoked before each tool call is dispatched.
// displayName is the human-friendly label for the tool (falls back to name if unset).
// Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnToolStart(fn func(name, displayName string, input json.RawMessage)) {
	l.onToolStart = fn
}

// SetOnToolEnd registers a callback invoked after each tool call completes.
// displayName is the human-friendly label for the tool (falls back to name if unset).
// Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnToolEnd(fn func(name, displayName string, input json.RawMessage, out ToolOutcome)) {
	l.onToolEnd = fn
}

// SetOnLLMRequest registers a callback invoked with the fully-assembled request
// for each inner LLM call, just before it is submitted, along with the estimated
// tokens used, the context budget, and the 1-based call number. Pass nil to
// clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnLLMRequest(fn func(req *llm.Request, tokensUsed, budget, llmCallN int)) {
	l.onLLMRequest = fn
}

// SetOnLLMResponse registers a callback invoked with the raw response of each
// inner LLM call. Pass nil to clear. Safe to call between turns; not safe during
// Run.
func (l *Loop) SetOnLLMResponse(fn func(resp *llm.Response, llmCallN int)) {
	l.onLLMResponse = fn
}

// SetOnChunk registers a callback invoked with each streamed text token from
// the LLM. Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnChunk(fn func(string)) { l.onChunk = fn }

// SetOnThinkingChunk registers a callback invoked with each streamed reasoning
// ("thinking") token from the LLM, when the provider surfaces extended thinking.
// Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnThinkingChunk(fn func(string)) { l.onThinkingChunk = fn }

// SetOnThinking registers a callback invoked at the start of each inner-loop
// LLM call, before the request is submitted. llmCallN is 1-based; think reports
// whether the call will stream reasoning, which is false both when the policy
// declines to think and when the model cannot. Pass nil to clear. Safe to call
// between turns; not safe during Run.
func (l *Loop) SetOnThinking(fn func(llmCallN int, think bool)) { l.onThinking = fn }

// SetOnPlanStart registers a callback invoked when the no-tool request-analysis
// (planning) pass begins. Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnPlanStart(fn func()) { l.onPlanStart = fn }

// SetOnPlanEnd registers a callback invoked when the request-analysis pass ends.
// Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnPlanEnd(fn func()) { l.onPlanEnd = fn }

// SetOnNotice registers a callback invoked with a session-level notice, e.g. a
// capability downgrade. Pass nil to clear. Safe to call between turns; not safe during Run.
func (l *Loop) SetOnNotice(fn func(string)) { l.onNotice = fn }

// LastRunToolCount returns the number of tool calls dispatched during the
// most recent Run(). Zero means the agent answered without using any tools.
func (l *Loop) LastRunToolCount() int { return l.lastToolCount }

// ConversationState is a serialisable snapshot of loop state for persistence and
// resumption across process restarts.
type ConversationState struct {
	History    []llm.Message             `json:"history"`
	Scratchpad []ninectx.ScratchpadEntry `json:"scratchpad"`
}

// NewLoop returns an agent loop ready to accept user turns.
func NewLoop(cfg Config, builder *ninectx.Builder, queue *llm.Queue, dispatcher *Dispatcher) *Loop {
	dn := make(map[string]string, len(cfg.Tools))
	for _, tw := range cfg.Tools {
		if tw.Tool.DisplayName != "" {
			dn[tw.Tool.Name] = tw.Tool.DisplayName
		}
	}

	mode := cfg.PlanMode
	if mode == "" {
		mode = PlanModePlanOnly
	}
	return &Loop{
		cfg:          cfg,
		builder:      builder,
		queue:        queue,
		dispatcher:   dispatcher,
		displayNames: dn,
		planMode:     mode,
	}
}

// Run processes one user turn: it runs the ReAct inner loop until the LLM
// produces a final answer, then returns the answer text. History is updated
// and the scratchpad is cleared on success.
//
// When the LLM returns multiple tool calls in a single response they are
// dispatched sequentially, each recorded as its own scratchpad entry. This
// is the uncommon case in ReAct; single-tool responses are the norm.
func (l *Loop) Run(ctx context.Context, userText string) (string, error) {
	l.history = append(l.history, llm.Message{Role: "user", Text: userText})
	l.scratchpad = l.scratchpad[:0]
	l.lastToolCount = 0
	forced := l.forceThinkNextTurn // take the current value and reset for next run
	l.forceThinkNextTurn = false
	mode := l.PlanMode()

	l.stage(StageRecall)

	// Embed user text once per turn; reused across all inner Build() calls.
	queryVec := embedText(ctx, l.cfg.Embedder, userText)
	selfModel := ""
	if l.cfg.SelfModelFn != nil {
		selfModel = l.cfg.SelfModelFn(ctx, queryVec)
	}
	enrichment := ""
	if l.cfg.EnrichmentFn != nil {
		enrichment = l.cfg.EnrichmentFn(ctx, queryVec)
	}

	plan := ""
	if mode != PlanModeOff && l.cfg.AnalysisPrompt != "" && l.queue.ThinkingUnsupported(ctx) {
		if !l.downgradeNoticed {
			l.downgradeNoticed = true
			if l.onNotice != nil {
				l.onNotice("This model does not support native thinking — running a planning pass instead.")
			}
		}
		if l.onPlanStart != nil {
			l.onPlanStart()
		}
		for {
			p := l.runAnalysisPass(ctx, queryVec)
			if p == "" {
				break
			}

			plan = "Request analysis (planning pass, no tools were used): \n" + p
			if l.cfg.PlanReviewFn == nil {
				break
			}

			decision, err := l.cfg.PlanReviewFn(ctx, p)
			if err != nil {
				if l.onPlanEnd != nil {
					l.onPlanEnd()
				}
				return "", fmt.Errorf("plan review: %w", err)
			}

			if decision.Proceed || decision.Clarification == "" {
				break
			}

			l.history = append(l.history, llm.Message{Role: "user", Text: decision.Clarification})
		}
		if l.onPlanEnd != nil {
			l.onPlanEnd()
		}
	}

	llmCallN := 0
	var toolErrs []string // tool failures seen this turn, surfaced if the answer is empty
	for {
		llmCallN++
		think := l.thinkFor(mode, llmCallN, forced)
		// Report what the user will actually see: only a capable model asked to
		// think streams reasoning. Ollama gates think on capability, so a policy
		// "yes" on a model that can't think produces no trace at all.
		if l.onThinking != nil {
			l.onThinking(llmCallN, think != nil && *think && l.queue.SupportsThinking(ctx))
		}

		l.stage(StageContext)

		req, tokensUsed := l.builder.BuildWithUsage(ninectx.BuildInput{
			SystemCore:       "Current time: " + time.Now().UTC().Format(time.RFC3339) + "\n\n" + l.cfg.SystemCore,
			SystemExtras:     l.cfg.SystemExtras,
			SystemSelf:       selfModel,
			SystemEnrichment: enrichment,
			Tools:            l.cfg.Tools,
			QueryVector:      queryVec,
			History:          l.history,
			Scratchpad:       l.scratchpad,
			SystemPlan:       plan,
		})
		req.MaxTokens = l.maxTokens()
		req.OnChunk = l.onChunk
		req.OnThinkingChunk = l.onThinkingChunk

		if think != nil {
			req.Think = think
		}
		l.wireQueueStages(&req, StageModel)

		if l.onContextUpdate != nil {
			l.onContextUpdate(tokensUsed, l.builder.Budget())
		}
		if l.onLLMRequest != nil {
			l.onLLMRequest(&req, tokensUsed, l.builder.Budget(), llmCallN)
		}
		slog.Debug("context_built",
			"priority", l.cfg.Priority,
			"llm_call_n", llmCallN,
			"tokens_used", tokensUsed,
			"budget", l.builder.Budget(),
			"history_len", len(l.history),
			"scratchpad_entries", len(l.scratchpad),
		)

		l.stage(StageModel)
		slog.Debug("llm_call", "priority", l.cfg.Priority, "history_len", len(l.history))
		resp, err := l.queue.Submit(ctx, l.cfg.Priority, req)
		if err != nil {
			return "", fmt.Errorf("llm: %w", err)
		}
		slog.Debug("llm_response",
			"priority", l.cfg.Priority,
			"llm_call_n", llmCallN,
			"n_tool_calls", len(resp.ToolCalls),
			"has_text", resp.Text != "",
		)
		if l.onLLMResponse != nil {
			l.onLLMResponse(&resp, llmCallN)
		}

		if len(resp.ToolCalls) == 0 {
			answer := resp.Text
			// Record only what the model actually said. The fallback below is
			// text *about* the turn, written for whoever is reading; appending it
			// to history would put it in the model's own mouth, and on a
			// long-lived session (self-reflection) the model reads it back and
			// learns to imitate it — a loop that feeds itself.
			if strings.TrimSpace(answer) != "" {
				l.history = append(l.history, llm.Message{Role: "assistant", Text: answer})
			} else {
				// The model ended the turn without text. Don't return a blank
				// answer — surface whatever went wrong so the turn isn't a
				// silent no-op.
				answer = emptyAnswerFallback(toolErrs)
				slog.Warn("empty final answer",
					"priority", l.cfg.Priority,
					"llm_call_n", llmCallN,
					"tool_errors", len(toolErrs),
				)
			}
			l.scratchpad = l.scratchpad[:0]
			return answer, nil
		}

		l.lastToolCount += len(resp.ToolCalls)

		// Dispatch tool calls and record observations. The first entry gets the
		// assistant's thought text; subsequent entries (parallel calls) get none.
		for i, tc := range resp.ToolCalls {
			dn := l.displayNames[tc.Name]
			if l.onToolStart != nil {
				l.onToolStart(tc.Name, dn, tc.Input)
			}
			result, attempts, elapsed, dispErr := l.dispatchWithRetry(ctx, tc.Name, tc.Input)
			observation := result.Output
			errStr := ""
			if dispErr != nil {
				observation = fmt.Sprintf(
					"Tool %q failed after %d attempt(s): %v. Try a different approach or tool.",
					tc.Name, maxToolRetries+1, dispErr,
				)
				errStr = dispErr.Error()
				toolErrs = append(toolErrs, fmt.Sprintf("%s: %v", tc.Name, dispErr))
			}
			if l.onToolEnd != nil {
				l.onToolEnd(tc.Name, dn, tc.Input, ToolOutcome{
					Output:      observation,
					Truncated:   result.Truncated,
					SpillPath:   result.SpillPath,
					OutputChars: result.OutputChars,
					DurationMs:  elapsed.Milliseconds(),
					Attempts:    attempts,
					Err:         errStr,
				})
			}

			thought := ""
			if i == 0 {
				thought = resp.Text
			}
			l.scratchpad = append(l.scratchpad, ninectx.ScratchpadEntry{
				Thought:     thought,
				ToolName:    tc.Name,
				ToolCallID:  tc.ID,
				ToolArgs:    tc.Input,
				Observation: observation,
			})
		}
	}
}

// InspectContext assembles the context for the session's current state and
// returns a per-section token breakdown — the same deterministic assembly Run
// performs before each inner LLM call, but WITHOUT calling the LLM. It embeds the
// most recent user message (embedder call, not an LLM call) so tool ranking,
// self-model, and enrichment reflect what the next turn would actually see; with
// no prior user message it builds with no query vector. Must be called on the
// worker goroutine — the loop is not safe for concurrent use.
func (l *Loop) InspectContext(ctx context.Context) ninectx.Report {
	var queryVec []float32
	if last := lastUserText(l.history); last != "" {
		queryVec = embedText(ctx, l.cfg.Embedder, last)
	}
	selfModel := ""
	if l.cfg.SelfModelFn != nil {
		selfModel = l.cfg.SelfModelFn(ctx, queryVec)
	}
	enrichment := ""
	if l.cfg.EnrichmentFn != nil {
		enrichment = l.cfg.EnrichmentFn(ctx, queryVec)
	}
	return l.builder.BuildReport(ninectx.BuildInput{
		SystemCore:       "Current time: " + time.Now().UTC().Format(time.RFC3339) + "\n\n" + l.cfg.SystemCore,
		SystemExtras:     l.cfg.SystemExtras,
		SystemSelf:       selfModel,
		SystemEnrichment: enrichment,
		Tools:            l.cfg.Tools,
		QueryVector:      queryVec,
		History:          l.history,
		Scratchpad:       l.scratchpad,
	})
}

// lastUserText returns the text of the most recent user message, or "" if none.
func lastUserText(history []llm.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Role == "user" && history[i].Text != "" {
			return history[i].Text
		}
	}
	return ""
}

// emptyAnswerFallback builds a user-facing message for a turn that ended with
// no text. If tools failed during the turn, it surfaces them so the failure
// isn't swallowed; otherwise it returns a generic notice.
func emptyAnswerFallback(toolErrs []string) string {
	if len(toolErrs) == 0 {
		return "(I wasn't able to produce a response for that.)"
	}
	return "I couldn't complete that. The following tool call(s) failed:\n- " +
		strings.Join(toolErrs, "\n- ")
}

// maxToolRetries is the number of times a failing tool call is retried before
// the loop gives up and instructs the LLM to try a different approach.
const maxToolRetries = 2

// dispatchWithRetry calls Dispatch up to maxToolRetries+1 times, returning on
// the first success. Each failed attempt is logged. On final failure the last
// error is returned so the caller can build an instructive observation. The
// returned int is the total number of attempts made (1 = succeeded first try).
func (l *Loop) dispatchWithRetry(ctx context.Context, name string, input json.RawMessage) (CallResult, int, time.Duration, error) {
	var (
		result  CallResult
		err     error
		elapsed time.Duration
	)
	for attempt := range maxToolRetries + 1 {
		t0 := time.Now()
		result, err = l.dispatcher.Dispatch(ctx, name, input)
		elapsed = time.Since(t0)
		if err == nil {
			slog.Info("tool call",
				"tool", name,
				"input", string(input),
				"output_len", len(result.Output),
				"truncated", result.Truncated,
				"duration_ms", elapsed.Milliseconds(),
			)
			return result, attempt + 1, elapsed, nil
		}
		if attempt < maxToolRetries {
			slog.Warn("tool call failed, retrying",
				"tool", name,
				"attempt", attempt+1,
				"err", err,
				"duration_ms", elapsed.Milliseconds(),
			)
		} else {
			slog.Warn("tool call failed, giving up",
				"tool", name,
				"input", string(input),
				"err", err,
				"attempts", maxToolRetries+1,
				"duration_ms", elapsed.Milliseconds(),
			)
		}
	}
	return result, maxToolRetries + 1, elapsed, err
}

// ClearHistory discards conversation history and scratchpad, making the loop
// stateless for the next turn. Used by the reflection loop.
func (l *Loop) ClearHistory() {
	l.history = l.history[:0]
	l.scratchpad = l.scratchpad[:0]
}

// SaveState serialises the loop's current state to JSON.
func (l *Loop) SaveState() ([]byte, error) {
	return json.Marshal(ConversationState{
		History:    l.history,
		Scratchpad: l.scratchpad,
	})
}

// LoadState restores loop state from a JSON checkpoint.
func (l *Loop) LoadState(data []byte) error {
	var cp ConversationState
	if err := json.Unmarshal(data, &cp); err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	l.history = cp.History
	l.scratchpad = cp.Scratchpad
	return nil
}

func (l *Loop) maxTokens() int {
	if l.cfg.MaxTokens > 0 {
		return l.cfg.MaxTokens
	}
	return 4096
}

func embedText(ctx context.Context, e embed.Embedder, text string) []float32 {
	if e == nil || text == "" {
		return nil
	}
	vec, err := e.Embed(ctx, text)
	if err != nil {
		slog.Debug("embed failed", "err", err)
		return nil
	}
	return vec
}

// runAnalysisPass performs one tool-free completion that reasons about the
// request, returning the plan text ("" on failure). It is the graceful-
// degradation substitute for native thinking. Tools are stripped so the
// model must reason in text; it is not streamed to the user (internal planning).
func (l *Loop) runAnalysisPass(ctx context.Context, queryVec []float32) string {
	req, _ := l.builder.BuildWithUsage(ninectx.BuildInput{
		SystemCore:  l.cfg.AnalysisPrompt,
		Tools:       nil,
		QueryVector: queryVec,
		History:     l.history,
	})
	req.MaxTokens = analysisMaxTokens

	l.wireQueueStages(&req, "")

	resp, err := l.queue.Submit(ctx, l.cfg.Priority, req)
	if err != nil {
		slog.Warn("analysis pass failed", "priority", l.cfg.Priority, "err", err)
		return ""
	}
	return strings.TrimSpace(resp.Text)
}

// SetPlanMode changes the reasoning mode live (the set_plan_mode command).
// Concurrency-safe: may be called from another goroutine while a turn runs; the
// new mode takes effect from the next turn.
func (l *Loop) SetPlanMode(mode string) {
	l.planModeMu.Lock()
	defer l.planModeMu.Unlock()
	l.planMode = mode
}

// PlanMode returns the current reasoning mode. Concurrency-safe.
func (l *Loop) PlanMode() string {
	l.planModeMu.RLock()
	defer l.planModeMu.RUnlock()
	return l.planMode
}

// Role returns the resolved role name this loop runs. Fixed at build time.
func (l *Loop) Role() string { return l.cfg.Role }

// SetOnStage registers a callback invoked when the turn enters a named waiting
// phase (see the Stage* constants). Pass nil to clear. Unlike the other
// callbacks this one is concurrency-safe: a queue wait is reported from the LLM
// queue's goroutine, so it may be read while the worker clears it after Run.
func (l *Loop) SetOnStage(fn func(string)) {
	l.stageMu.Lock()
	defer l.stageMu.Unlock()
	l.onStage = fn
}

func (l *Loop) stage(label string) {
	l.stageMu.Lock()
	fn := l.onStage
	l.stageMu.Unlock()
	// Called outside the lock: it reaches the event stream and must not run
	// under a loop mutex.
	if fn != nil {
		fn(label)
	}
}

// wireQueueStages reports a queue wait on req. An interactive turn can sit
// behind reflection, standing agents, or sub-agents before it reaches the
// provider, which is indistinguishable from the model hanging. resumed is the
// phase to report once a slot opens.
func (l *Loop) wireQueueStages(req *llm.Request, resumed string) {
	req.OnQueued = func() { l.stage(StageQueued) }
	req.OnDequeued = func() { l.stage(resumed) }
}

// thinkFor resolves whether this inner call requests native thinking. nil means
// no policy is configured and the provider's own default applies.
func (l *Loop) thinkFor(mode string, llmCallN int, forced bool) *bool {
	if l.cfg.ThinkPolicy == nil {
		return nil
	}
	var think bool
	switch mode {
	case PlanModeOff:
		think = false
	case PlanModeAlways:
		think = true
	default:
		think = l.cfg.ThinkPolicy(llmCallN, forced)
	}
	return &think
}
