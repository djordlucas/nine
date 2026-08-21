package runtime

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"nine/internal/agent"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/toolvm"
)

// This file holds the AgentWorker → EventSink journaling path: the typed event
// payloads (adr/event-log.md §6) and the helpers that build session_events rows
// from loop callbacks. Span ids are derived from the turn number so a turn's
// tree (turn root → llm calls → tool calls) is reconstructable from the flat
// log via parent_span_id.

type turnStartPayload struct {
	Input   string `json:"input"`
	Trigger string `json:"trigger"` // "user" | "idle"
}

type turnEndPayload struct {
	Result     string `json:"result,omitempty"`
	Error      string `json:"error,omitempty"`
	ToolCount  int    `json:"tool_count"`
	DurationMs int64  `json:"duration_ms"`
}

type llmRequestPayload struct {
	System     string        `json:"system"`
	Messages   []llm.Message `json:"messages"`
	ToolNames  []string      `json:"tool_names"`
	MaxTokens  int           `json:"max_tokens"`
	TokensUsed int           `json:"tokens_used"`
	Budget     int           `json:"budget"`
	LLMCallN   int           `json:"llm_call_n"`
}

type llmResponsePayload struct {
	Text       string         `json:"text"`
	ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
	StopReason string         `json:"stop_reason"`
	LLMCallN   int            `json:"llm_call_n"`
	// Provider-reported token accounting, omitted when the provider reports
	// none. The estimate it reconciles against — tokens_used and budget — is on
	// the llm_request event sharing this event's span id, so estimate-vs-actual
	// is a join on span rather than a duplicated field.
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

type toolStartPayload struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type toolEndPayload struct {
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    string          `json:"output"`
	Truncated bool            `json:"truncated,omitempty"`
	// SpillPath / OutputChars describe an over-cap result: Output holds the
	// preview the model saw, and these say where the full text lives and how
	// long it was (adr/tool-output-spill.md §4).
	SpillPath   string `json:"spill_path,omitempty"`
	OutputChars int    `json:"output_chars,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error,omitempty"`
}

type contextPayload struct {
	Used   int `json:"used"`
	Budget int `json:"budget"`
}

type thinkingPayload struct {
	LLMCallN int  `json:"llm_call_n"`
	Think    bool `json:"think,omitempty"`
}

type subAgentPayload struct {
	SubID  string `json:"sub_id"`
	Task   string `json:"task,omitempty"`
	Status string `json:"status,omitempty"`
	Role   string `json:"role,omitempty"`
}

// toolHTTPPayload records one outbound request a sandboxed tool made
// (docs/sandboxed-tools.md §8). The URL is already redacted of credentials by
// the host; the host allowlist and SSRF checks that permitted it are not
// repeated here, because this records what happened rather than why it was
// allowed.
type toolHTTPPayload struct {
	Tool       string `json:"tool"`
	Method     string `json:"method"`
	Host       string `json:"host"`
	URL        string `json:"url"`
	Status     int    `json:"status,omitempty"`
	Bytes      int    `json:"bytes"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	// Error carries the refusal reason. A blocked request is the most
	// audit-worthy thing a tool does, so it is journaled rather than dropped.
	Error string `json:"error,omitempty"`
}

// turnSpan returns the span id of a turn's root.
func turnSpan(turn int) string { return fmt.Sprintf("t%d", turn) }

// llmSpan returns the span id of the nth inner LLM call within a turn.
func llmSpan(turn, n int) string { return fmt.Sprintf("t%d.llm%d", turn, n) }

// toolSpan returns the span id of the nth tool call within a turn.
func toolSpan(turn, n int) string { return fmt.Sprintf("t%d.tool%d", turn, n) }

// journal appends a typed event to the sink, marshaling payload to JSON. It is
// a no-op when no sink is configured. turn is passed explicitly so callers on
// the worker goroutine avoid locking; concurrent callers (emitEvent) pass a
// lock-guarded snapshot.
func (w *AgentWorker) journal(turn int, typ, span, parent string, payload any) {
	if w.sink == nil {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("journal marshal failed", "type", typ, "agent_id", w.id, "err", err)
		return
	}
	w.sink.Append(memory.SessionEvent{
		AgentID:      w.id,
		Turn:         turn,
		SpanID:       span,
		ParentSpanID: parent,
		Type:         typ,
		Payload:      b,
	})
}

// journalToolStart records a tool_start under the current LLM call and advances
// the per-turn tool counter. Runs on the worker goroutine.
func (w *AgentWorker) journalToolStart(turn int, name string, input json.RawMessage) {
	w.toolN++
	w.journal(turn, "tool_start", toolSpan(turn, w.toolN), llmSpan(turn, w.llmCallN), toolStartPayload{
		Name:  name,
		Input: input,
	})
}

// httpAuditor returns the hook a turn installs on its context so a sandboxed
// tool's outbound requests land in the journal.
//
// It parents each request to the span of the tool call in flight — w.toolN is
// the tool most recently started, and an HTTP call can only happen inside one —
// so `nine trace` nests the request under the call that made it rather than
// listing it beside the turn.
//
// Called from the guest's goroutine while the worker is blocked in loop.Run, so
// it reads w.toolN without a lock for the same reason the other journal hooks do.
func (w *AgentWorker) httpAuditor(turn int) toolvm.HTTPAuditFn {
	return func(c toolvm.HTTPCall) {
		w.journal(turn, "tool_http", toolSpan(turn, w.toolN)+".http", toolSpan(turn, w.toolN),
			toolHTTPPayload{
				Tool:       c.Tool,
				Method:     c.Method,
				Host:       c.Host,
				URL:        c.URL,
				Status:     c.Status,
				Bytes:      c.Bytes,
				Truncated:  c.Truncated,
				DurationMs: c.Duration.Milliseconds(),
				Error:      c.Error,
			})
	}
}

// journalToolEnd records a tool_end for the tool most recently started this turn.
func (w *AgentWorker) journalToolEnd(turn int, name string, input json.RawMessage, out agent.ToolOutcome) {
	w.journal(turn, "tool_end", toolSpan(turn, w.toolN), llmSpan(turn, w.llmCallN), toolEndPayload{
		Name:        name,
		Input:       input,
		Output:      out.Output,
		Truncated:   out.Truncated,
		SpillPath:   out.SpillPath,
		OutputChars: out.OutputChars,
		DurationMs:  out.DurationMs,
		Attempts:    out.Attempts,
		Error:       out.Err,
	})
}
