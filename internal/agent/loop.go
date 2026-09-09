package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	ninectx "nine/internal/context"
	"nine/internal/embed"
	"nine/internal/llm"
)

const analysisMaxTokens = 512

// contextWarnFraction is the assembled-context usage fraction of the budget at
// or above which the loop emits a notice that oldest history is being trimmed
// to fit. The warning re-arms once usage falls back below the threshold, so a
// session that repeatedly brushes the ceiling is warned each time it crosses up
// rather than only once.
const contextWarnFraction = 0.9

// Config holds static configuration for an agent loop.
type Config struct {
	// Role is the resolved role name this loop runs (orchestrator, executor,
	// pursue, …). Descriptive only — the tool boundary and persona are already
	// baked into the loop at build time; this is carried so the daemon can
	// surface which role a session or sub-agent is running (status, TUI header).
	Role string
	// SessionID identifies the session this loop runs — a conversation,
	// goal-pursue, reflection, or sub-agent (see docs/glossary.md § Work Units).
	// When set it is stamped into the system core every turn, so the model can
	// answer a user who asks which session they are talking to. Empty omits the
	// line entirely, which is what replay and most tests want.
	SessionID    string
	SystemCore   string
	SystemExtras string
	Priority     int // llm.PrioritySupervisor / PriorityConversation / PriorityBackground
	MaxTokens    int // per-completion token limit; 0 → 4096
	Tools        []ninectx.ToolWithVector
	Embedder     embed.Embedder                                       // nil = no query embedding / tool ranking
	SelfModelFn  func(ctx context.Context, queryVec []float32) string // nil = no self-model
	// EnrichmentFn optionally returns pull-surfaced enrichment for the current
	// query — e.g. a related prior session recorded out-of-band by the reactive
	// subscriber (adr/reactive-events.md §5) and/or stored memories relevant to
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

	// QueuedMessagesFn, when non-nil, returns the number of unconsumed queued
	// messages for this session. The context builder surfaces this count to the
	// model so it knows to call queued_messages_get. nil = no queued messages.
	QueuedMessagesFn func() int
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

// Stage labels reported via Hooks.OnStage. These name the phases *within one
// turn* that can make a user wait, and are unrelated to a session's **routines**
// (internal/runtime), which are the concurrent behaviors a session carries across
// turns. Both were called "stage" until the routine rename; each word now means
// exactly one thing.
//
// They name the phases of a turn that run
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
	contextWarnLatched bool // re-arming latch: a context-pressure notice is active until usage drops back below contextWarnFraction
	onLLMRequest       func(req *llm.Request, tokensUsed, budget, llmCallN int)
	onLLMResponse      func(resp *llm.Response, llmCallN int)
	onStage            func(string) // guarded by stageMu
}

// ToolOutcome carries the full result of a dispatched tool call for observers
// (the durable event journal, adr/event-log.md §7.3). Output is exactly what
// the model saw as the observation — the tool's text on success, or the
// instructive failure notice when Err is set.
type ToolOutcome struct {
	Output    string
	Truncated bool
	// SpillPath is where the full output was stored when it exceeded the cap,
	// and OutputChars how long that output was. The journal records the pointer
	// rather than the payload: the bytes already live in the file store, so
	// duplicating them into the event log would double the cost of every large
	// result (adr/tool-output-spill.md §4).
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

// Hooks are the observer callbacks a caller attaches to a turn. Every field is
// optional; a nil field is simply not called.
//
// They are per-turn, not construction-time configuration: the agent worker binds
// a fresh set before each Run because the closures capture that turn's number
// for span ids and its emit destination, then detaches them with ClearHooks
// afterwards. That is why they are a struct passed to SetHooks rather than
// arguments to NewLoop.
//
// Grouping them this way is not cosmetic. Attaching twelve callbacks through
// twelve setters made a forgotten one silently do nothing, and detaching meant
// twelve more calls that had to stay in step with the first twelve. A struct
// makes an unset callback a visible zero field, and ClearHooks detaches all of
// them or none.
type Hooks struct {
	// OnContextUpdate reports the estimated tokens used and the total budget
	// after each context assembly.
	OnContextUpdate func(used, budget int)

	// OnToolStart fires before each tool call is dispatched, OnToolEnd after it
	// completes. displayName is the human-friendly label, falling back to name.
	OnToolStart func(name, displayName string, input json.RawMessage)
	OnToolEnd   func(name, displayName string, input json.RawMessage, out ToolOutcome)

	// OnLLMRequest fires with the fully-assembled request for each inner LLM
	// call just before it is submitted, along with the estimated tokens used,
	// the context budget, and the 1-based call number. OnLLMResponse fires with
	// the raw response of that call.
	OnLLMRequest  func(req *llm.Request, tokensUsed, budget, llmCallN int)
	OnLLMResponse func(resp *llm.Response, llmCallN int)

	// OnChunk receives each streamed text token; OnThinkingChunk each streamed
	// reasoning token, when the provider surfaces extended thinking. Reasoning
	// is a separate channel and is never folded into the reply text.
	OnChunk         func(string)
	OnThinkingChunk func(string)

	// OnThinking fires at the start of each inner LLM call, before the request
	// is submitted. llmCallN is 1-based; think reports whether the call will
	// actually stream reasoning, which is false both when the policy declines to
	// think and when the model cannot.
	OnThinking func(llmCallN int, think bool)

	// OnPlanStart and OnPlanEnd bracket the no-tool request-analysis pass.
	OnPlanStart func()
	OnPlanEnd   func()

	// OnNotice receives a session-level notice, e.g. a capability downgrade or
	// context pressure.
	OnNotice func(text string)

	// OnStage reports entry into a named waiting phase (the Stage* constants).
	// Unlike the others it may fire from the LLM queue's goroutine, so the Loop
	// guards it with a mutex; see SetHooks.
	OnStage func(string)
}

// SetHooks attaches h for subsequent turns, replacing any previously attached
// set wholesale — there is no merging, so a caller assembles the full set it
// wants in one literal.
//
// Safe to call between turns; not safe during Run, with the single exception of
// OnStage, which is guarded because the queue reports a wait from its own
// goroutine.
func (l *Loop) SetHooks(h Hooks) {
	l.onContextUpdate = h.OnContextUpdate
	l.onToolStart = h.OnToolStart
	l.onToolEnd = h.OnToolEnd
	l.onLLMRequest = h.OnLLMRequest
	l.onLLMResponse = h.OnLLMResponse
	l.onChunk = h.OnChunk
	l.onThinkingChunk = h.OnThinkingChunk
	l.onThinking = h.OnThinking
	l.onPlanStart = h.OnPlanStart
	l.onPlanEnd = h.OnPlanEnd
	l.onNotice = h.OnNotice

	// onStage alone is read from the LLM queue's goroutine, so it may be written
	// here while a just-finished turn's queue wait is still reporting.
	l.stageMu.Lock()
	l.onStage = h.OnStage
	l.stageMu.Unlock()
}

// ClearHooks detaches every callback. It is the exact inverse of SetHooks, so a
// caller cannot leave some attached and some not — the failure mode when
// detaching was twelve separate nil assignments.
func (l *Loop) ClearHooks() { l.SetHooks(Hooks{}) }

// maybeWarnContext emits a session notice when assembled context usage crosses
// contextWarnFraction of the budget, re-arming once usage falls back below it.
// By the time usage is this high the builder is already trimming oldest history
// to fit (internal/context.trimFront), so the notice tells the user their
// earliest turns are dropping out of the window rather than reporting an error —
// the turn still fits the budget by construction.
func (l *Loop) maybeWarnContext(used, budget int) {
	if budget <= 0 {
		return
	}
	over := float64(used) >= contextWarnFraction*float64(budget)
	switch {
	case over && !l.contextWarnLatched:
		l.contextWarnLatched = true
		if l.onNotice != nil {
			l.onNotice(fmt.Sprintf(
				"Context is %d%% full (%d/%d tokens) — oldest history is being trimmed to fit.",
				used*100/budget, used, budget))
		}
	case !over:
		l.contextWarnLatched = false
	}
}

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
	emptyAnswers := 0     // no-text, no-tool-call responses so far (see maxEmptyAnswerRetries)
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
			SystemCore:          l.ambientCore(),
			SystemExtras:        l.cfg.SystemExtras,
			SystemSelf:          selfModel,
			SystemEnrichment:    enrichment,
			Tools:               l.cfg.Tools,
			QueryVector:         queryVec,
			History:             l.history,
			Scratchpad:          l.scratchpad,
			SystemPlan:          plan,
			QueuedMessagesCount: l.queuedMessagesCount(),
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
		l.maybeWarnContext(tokensUsed, l.builder.Budget())
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
		// Reconcile the pre-send estimate against what the provider charged.
		// countTokens is a chars/4 approximation and drifts most on exactly the
		// content a turn is made of (JSON tool schemas, code, tool results), so
		// the error is worth surfacing per call rather than inferred later. A
		// provider that reports no counts leaves InputTokens zero; skip it
		// instead of recording a spurious -100%.
		if resp.Usage.InputTokens > 0 {
			slog.Debug("token_reconciliation",
				"llm_call_n", llmCallN,
				"estimated_input", tokensUsed,
				"actual_input", resp.Usage.InputTokens,
				"error_pct", int(math.Round(float64(tokensUsed-resp.Usage.InputTokens)/float64(resp.Usage.InputTokens)*100)),
				"output", resp.Usage.OutputTokens,
				"budget", l.builder.Budget(),
			)
		}
		if l.onLLMResponse != nil {
			l.onLLMResponse(&resp, llmCallN)
		}

		if len(resp.ToolCalls) == 0 {
			answer := resp.Text
			// A turn that produced neither text nor a tool call is usually a
			// sampling accident, not a decision: small local models answering
			// on top of a tool observation quite often emit a single
			// end-of-turn token and stop. Measured on gemma4:e2b replaying one
			// real "list your skills" turn, half of ten identical requests came
			// back that way. Nothing has been appended to history or the
			// scratchpad yet, so re-running the loop rebuilds the same request
			// and simply draws another sample — which is why a retry is worth
			// far more here than the fallback text is.
			if strings.TrimSpace(answer) == "" && emptyAnswers < maxEmptyAnswerRetries {
				emptyAnswers++
				slog.Warn("empty model response; retrying",
					"priority", l.cfg.Priority,
					"llm_call_n", llmCallN,
					"attempt", emptyAnswers,
					"max", maxEmptyAnswerRetries,
				)
				continue
			}
			// Record only what the model actually said. The fallback below is
			// text *about* the turn, written for whoever is reading; appending it
			// to history would put it in the model's own mouth, and on a
			// long-lived session (self-reflection) the model reads it back and
			// learns to imitate it — a loop that feeds itself.
			if strings.TrimSpace(answer) != "" {
				l.history = append(l.history, llm.Message{Role: "assistant", Text: answer})
			} else {
				// Out of retries: the model will not answer this one. Don't
				// return a blank answer — surface whatever went wrong so the
				// turn isn't a silent no-op.
				answer = emptyAnswerFallback(toolErrs)
				slog.Warn("empty final answer",
					"priority", l.cfg.Priority,
					"llm_call_n", llmCallN,
					"retries", emptyAnswers,
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
				// `attempts`, not maxToolRetries+1: the loop returns early for a
				// refused approval and for a non-retryable failure, and claiming
				// three tries when one happened misleads both the model and anyone
				// reading the journal.
				advice := "Try a different approach or tool."
				if retryable, stated := statedRetryable(dispErr); stated && !retryable {
					// Without this the model is told to "try a different approach"
					// by text that reads like generic encouragement, and may simply
					// re-issue the same call. The tool has already ruled that out.
					advice = "The tool reports this cannot succeed on retry; change the arguments or use a different tool."
				}
				observation = fmt.Sprintf(
					"Tool %q failed after %d attempt(s): %v. %s",
					tc.Name, attempts, dispErr, advice,
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
		SystemCore:       l.ambientCore(),
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

// maxEmptyAnswerRetries is the number of times a response carrying neither text
// nor a tool call is re-drawn before the turn gives up and falls back. Two
// retries because the failure is an independent sample each time: at the ~50%
// rate measured on a small local model, three attempts leave roughly one turn
// in eight unanswered instead of one in two. The cost of being wrong is bounded
// — an extra LLM call on a turn that was about to return nothing anyway.
const maxEmptyAnswerRetries = 2

// dispatchWithRetry calls Dispatch up to maxToolRetries+1 times, returning on
// the first success. Each failed attempt is logged. On final failure the last
// error is returned so the caller can build an instructive observation. The
// returned int is the total number of attempts made (1 = succeeded first try).
// statedRetryable asks an error whether trying again could work, and whether it
// said so at all.
//
// Deliberately an interface rather than a concrete type: a sandboxed tool's
// *toolvm.CallError satisfies it today, and a plugin error can opt in later
// without this package learning about either. The two return values are the
// whole point — "did not say" must not collapse into "said no", because only one
// of those is a claim the tool made.
func statedRetryable(err error) (retryable, stated bool) {
	var r interface{ Retryable() (bool, bool) }
	if !errors.As(err, &r) {
		return false, false
	}
	return r.Retryable()
}

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
		// A human's refusal (or an unanswered approval question) is a decision,
		// not a flake — retrying would re-prompt for the same call. Give the
		// model the refusal now and let it choose another route.
		var approvalErr *ApprovalError
		if errors.As(err, &approvalErr) {
			slog.Info("tool call not approved, not retrying",
				"tool", name,
				"err", err,
				"duration_ms", elapsed.Milliseconds(),
			)
			return result, attempt + 1, elapsed, err
		}
		// A tool that says the failure is not retryable is making the same kind of
		// statement: a malformed argument does not become well-formed on the second
		// attempt, so retrying only burns the turn's budget and the operator's
		// tokens. Stated is checked separately from the value because a tool that
		// said nothing has not made this claim, and must keep the old behavior.
		if retryable, stated := statedRetryable(err); stated && !retryable {
			slog.Info("tool reported a non-retryable failure, not retrying",
				"tool", name,
				"err", err,
				"duration_ms", elapsed.Milliseconds(),
			)
			return result, attempt + 1, elapsed, err
		}
		// Nil args (the model sent nothing or invalid JSON that was sanitized
		// to nil) produce a deterministic validation error — retrying with nil
		// again will never succeed. Hand the error back to the model so it can
		// provide proper arguments. An explicit {} from the model is different:
		// the tool may accept it, and the error may be transient (e.g. an
		// approval gate flicker), so that case still retries.
		if len(input) == 0 {
			slog.Info("tool call failed with no args, not retrying",
				"tool", name,
				"err", err,
				"duration_ms", elapsed.Milliseconds(),
			)
			return result, attempt + 1, elapsed, err
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

// AppendUserHistory adds a user message to the in-memory conversation history.
// Used by queued-message consumption to fold consumed messages into the turn
// the model is running, so the end-of-turn checkpoint includes them.
func (l *Loop) AppendUserHistory(text string) {
	l.history = append(l.history, llm.Message{Role: "user", Text: text})
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

func (l *Loop) queuedMessagesCount() int {
	if l.cfg.QueuedMessagesFn == nil {
		return 0
	}
	return l.cfg.QueuedMessagesFn()
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

// ambientCore prepends the facts the model cannot derive for itself — the
// current time (R-LOOP.1) and, when the runtime supplied one, this session's ID
// — to the configured system core.
//
// Both places that assemble a turn go through here, so the request the model
// actually sees and the breakdown `/context` reports can never drift apart.
// With no SessionID the output is byte-identical to the plain time preamble,
// which keeps replay and recorded eval journals stable.
func (l *Loop) ambientCore() string {
	var b strings.Builder
	b.WriteString("Current time: ")
	b.WriteString(time.Now().UTC().Format(time.RFC3339))
	if l.cfg.SessionID != "" {
		b.WriteString("\nSession ID: ")
		b.WriteString(l.cfg.SessionID)
	}
	b.WriteString("\n\n")
	b.WriteString(l.cfg.SystemCore)
	return b.String()
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
