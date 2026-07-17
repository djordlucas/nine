// Package llm defines the provider-agnostic LLM interface and request queue.
package llm

import (
	"context"
	"encoding/json"
)

// Priority levels for the LLM request queue.
const (
	PrioritySupervisor   = 1 // supervisor agent (highest)
	PriorityConversation = 2 // active conversation (user is waiting)
	PriorityBackground   = 3 // background task or goal (lowest)
)

// ToolDef describes a tool available to the LLM during a completion.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	DisplayName string          `json:"-"` // human-friendly label for TUI display; never sent to the LLM
}

// ToolCall is a tool invocation requested by the LLM.
type ToolCall struct {
	ID    string          // unique per-turn call ID
	Name  string          // tool name
	Input json.RawMessage // tool arguments as JSON
}

// ToolResult is the result of a tool call, included in the next user turn.
type ToolResult struct {
	ToolCallID string // matches ToolCall.ID
	Content    string // tool output text
	IsError    bool
}

// Message is a single turn in a conversation.
// For user turns: set Text or ToolResults (not both).
// For assistant turns: Text and ToolCalls may both be set.
type Message struct {
	Role        string       // "user" | "assistant"
	Text        string       // plain text content
	ToolCalls   []ToolCall   // tool invocations (assistant turns)
	ToolResults []ToolResult // tool results (user turns following tool calls)
}

// Request is a single LLM completion request.
type Request struct {
	System    string
	Messages  []Message
	Tools     []ToolDef
	MaxTokens int
	OnChunk   func(string) // optional; called with each streamed text token
	// OnThinkingChunk is called with each streamed reasoning ("thinking") token,
	// when the provider is configured to surface extended thinking. It is a
	// distinct channel from OnChunk: thinking tokens are trace-only and are never
	// folded into Response.Text. Optional; nil disables thinking surfacing.
	OnThinkingChunk func(string)

	// Think overrides the provider's default thinking behavior for this request.
	// Non-nil true forces extended thinking on capable models, non-nil false suppresses it entirely,
	// and nil defers to the provider's default behavior.
	// Note that some providers may not support thinking at all, in which case this field is ignored.
	Think *bool

	// OnQueued is called if the request has to wait for a concurrency slot
	// rather than starting immediately; OnDequeued when a slot opens and it
	// starts. A request that starts immediately gets neither. Both are optional
	// and run on the queue's goroutine.
	OnQueued   func()
	OnDequeued func()
}

// Response is the LLM's completion result.
type Response struct {
	Text       string     // text reply (may be empty when stop_reason is tool_use)
	ToolCalls  []ToolCall // tool invocations requested
	StopReason string     // "end_turn" | "tool_use" | "max_tokens"

	// ThinkingUsed reflects if the thinking mode was used in the response.
	// It is true if the provider supports thinking and it was enabled for this request, false otherwise.
	ThinkingUsed bool
}

// Provider is the interface for LLM completion backends.
type Provider interface {
	Complete(ctx context.Context, req Request) (Response, error)
}

// ProviderFunc adapts a plain function to the Provider interface.
type ProviderFunc func(ctx context.Context, req Request) (Response, error)

func (f ProviderFunc) Complete(ctx context.Context, req Request) (Response, error) {
	return f(ctx, req)
}

// ThinkingAware is an optional interface implemented by providers that can
// report if the active model supports native thinking mode.
// It exists so the runtime can gate the per-request Think flag (and the plan-mode fallback)
// without bloating the core Provider interface.
// A provider that does not implement the ThinkingAware interface is assumed to not support model-native thinking mode.
// Implementations MUST be safe for concurrent use by multiple goroutines and SHOULD cache the result since the model cannot change during a session.
type ThinkingAware interface {
	SupportsThinking(ctx context.Context) bool
}
