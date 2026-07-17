package runtime

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"nine/internal/agent"
	"nine/internal/llm"
	"nine/internal/memory"
)

// This file holds the AgentWorker → EventSink journaling path: the typed event
// payloads (docs/event-log.md §6) and the helpers that build session_events rows
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
}

type toolStartPayload struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
}

type toolEndPayload struct {
	Name       string          `json:"name"`
	Input      json.RawMessage `json:"input,omitempty"`
	Output     string          `json:"output"`
	Truncated  bool            `json:"truncated,omitempty"`
	DurationMs int64           `json:"duration_ms"`
	Attempts   int             `json:"attempts"`
	Error      string          `json:"error,omitempty"`
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

// wireJournalHooks registers the loop callbacks that feed the journal for the
// in-flight turn. It returns the loop's existing progress callbacks untouched;
// callers set both the progress emit and these journal hooks. turn is the
// stable turn number for the duration of the loop run.
//
// The llm/tool/context/thinking journal hooks run on the worker goroutine (the
// loop calls them synchronously), so llmCallN/toolN need no locking.
func (w *AgentWorker) wireJournalHooks(turn int) {
	root := turnSpan(turn)
	w.loop.SetOnLLMRequest(func(req *llm.Request, tokensUsed, budget, llmCallN int) {
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
	})
	w.loop.SetOnLLMResponse(func(resp *llm.Response, llmCallN int) {
		w.journal(turn, "llm_response", llmSpan(turn, llmCallN), root, llmResponsePayload{
			Text:       resp.Text,
			ToolCalls:  resp.ToolCalls,
			StopReason: resp.StopReason,
			LLMCallN:   llmCallN,
		})
	})
}

// clearJournalHooks detaches the loop's journal callbacks after a turn.
func (w *AgentWorker) clearJournalHooks() {
	w.loop.SetOnLLMRequest(nil)
	w.loop.SetOnLLMResponse(nil)
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

// journalToolEnd records a tool_end for the tool most recently started this turn.
func (w *AgentWorker) journalToolEnd(turn int, name string, input json.RawMessage, out agent.ToolOutcome) {
	w.journal(turn, "tool_end", toolSpan(turn, w.toolN), llmSpan(turn, w.llmCallN), toolEndPayload{
		Name:       name,
		Input:      input,
		Output:     out.Output,
		Truncated:  out.Truncated,
		DurationMs: out.DurationMs,
		Attempts:   out.Attempts,
		Error:      out.Err,
	})
}
