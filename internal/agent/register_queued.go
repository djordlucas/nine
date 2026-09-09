package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/llm"
	"nine/internal/memory"
)

// QueuedMessage represents a single queued message with its consumption status.
type QueuedMessage = memory.QueuedMessage

var queuedToolDefs = []llm.ToolDef{
	{
		Name:        "queued_messages_get",
		DisplayName: "Get Queued Messages",
		Description: "Read all messages currently queued for this session. Returns messages with their consumption status. Queued messages are user replies that were added while the session was busy, and are NOT automatically included in the model's context. Each message has a 'consumed' field indicating whether it has been processed. Use queued_message_mark_consumed to mark individual messages as consumed after processing them.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	},
	{
		Name:        "queued_message_mark_consumed",
		DisplayName: "Mark Message Consumed",
		Description: "Mark a specific queued message as consumed by its index. Once marked as consumed, the message will be moved to conversation history and appear in future context. This allows fine-grained control: you can process some messages and leave others in the queue.",
		InputSchema: json.RawMessage(`{"type":"object","required":["index"],"properties":{"index":{"type":"integer","description":"0-based index of the message to mark as consumed"}}}`),
	},
	{
		Name:        "queued_messages_mark_all_consumed",
		DisplayName: "Mark All Messages Consumed",
		Description: "Mark all queued messages as consumed. This moves all messages to conversation history so they will appear in future context as normal user messages.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	},
	{
		Name:        "queued_messages_count",
		DisplayName: "Queued Messages Count",
		Description: "Get the number of messages currently queued for this session (including both consumed and unconsumed).",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	},
	{
		Name:        "queued_messages_unconsumed_count",
		DisplayName: "Unconsumed Messages Count",
		Description: "Get the number of unconsumed messages currently queued for this session.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	},
}

// QueuedMessagesStore is the persistence interface for queued messages.
type QueuedMessagesStore interface {
	// GetQueuedMessages returns all queued messages with their consumption status.
	GetQueuedMessages(agentID string) ([]QueuedMessage, error)
	// MarkConsumed marks a specific message as consumed and returns its text.
	// The caller is responsible for appending it to the conversation history.
	MarkConsumed(ctx context.Context, agentID string, index int) (string, error)
	// MarkAllConsumed marks all unconsumed messages as consumed and returns
	// their texts. The caller is responsible for appending them to history.
	MarkAllConsumed(ctx context.Context, agentID string) ([]string, error)
	// QueueMessage adds a message to the queue for the given agent ID.
	QueueMessage(agentID, message string) error
	// QueuedMessagesCount returns the total number of queued messages.
	QueuedMessagesCount(agentID string) (int, error)
	// UnconsumedMessagesCount returns the number of unconsumed queued messages.
	UnconsumedMessagesCount(agentID string) (int, error)
}

// RegisterQueuedTools registers the queued messages tools into d.
// onConsumed, when non-nil, is called for each consumed message so the loop
// can append it to in-memory history — avoiding a clobber by the end-of-turn
// checkpoint.
func RegisterQueuedTools(d *Dispatcher, store QueuedMessagesStore, getAgentID func() string, onConsumed func(text string)) {
	if store == nil {
		return
	}

	d.handlers["queued_messages_get"] = func(_ context.Context, _ json.RawMessage) (string, error) {
		agentID := getAgentID()
		if agentID == "" {
			return "", fmt.Errorf("no agent ID available")
		}
		msgs, err := store.GetQueuedMessages(agentID)
		if err != nil {
			return "", fmt.Errorf("get queued messages: %w", err)
		}
		data, err := json.Marshal(map[string]any{"messages": msgs})
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["queued_message_mark_consumed"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		agentID := getAgentID()
		if agentID == "" {
			return "", fmt.Errorf("no agent ID available")
		}
		var req struct {
			Index int `json:"index"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if req.Index < 0 {
			return "", fmt.Errorf("index must be non-negative")
		}
		msg, err := store.MarkConsumed(ctx, agentID, req.Index)
		if err != nil {
			return "", fmt.Errorf("mark consumed: %w", err)
		}
		if onConsumed != nil {
			onConsumed(msg)
		}
		return msg, nil
	}

	d.handlers["queued_messages_mark_all_consumed"] = func(ctx context.Context, _ json.RawMessage) (string, error) {
		agentID := getAgentID()
		if agentID == "" {
			return "", fmt.Errorf("no agent ID available")
		}
		texts, err := store.MarkAllConsumed(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("mark all consumed: %w", err)
		}
		if onConsumed != nil {
			for _, t := range texts {
				onConsumed(t)
			}
		}
		return fmt.Sprintf("marked %d messages as consumed", len(texts)), nil
	}

	d.handlers["queued_messages_count"] = func(_ context.Context, _ json.RawMessage) (string, error) {
		agentID := getAgentID()
		if agentID == "" {
			return "", fmt.Errorf("no agent ID available")
		}
		count, err := store.QueuedMessagesCount(agentID)
		if err != nil {
			return "", fmt.Errorf("get queued messages count: %w", err)
		}
		return fmt.Sprintf("%d", count), nil
	}

	d.handlers["queued_messages_unconsumed_count"] = func(_ context.Context, _ json.RawMessage) (string, error) {
		agentID := getAgentID()
		if agentID == "" {
			return "", fmt.Errorf("no agent ID available")
		}
		count, err := store.UnconsumedMessagesCount(agentID)
		if err != nil {
			return "", fmt.Errorf("get unconsumed messages count: %w", err)
		}
		return fmt.Sprintf("%d", count), nil
	}
}

