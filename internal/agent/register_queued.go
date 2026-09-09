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
	// MarkConsumed marks a specific message as consumed and moves it to history.
	MarkConsumed(ctx context.Context, agentID string, index int) (string, error)
	// MarkAllConsumed marks all messages as consumed and moves them to history.
	MarkAllConsumed(ctx context.Context, agentID string) (int, error)
	// QueueMessage adds a message to the queue for the given agent ID.
	QueueMessage(agentID, message string) error
	// QueuedMessagesCount returns the total number of queued messages.
	QueuedMessagesCount(agentID string) (int, error)
	// UnconsumedMessagesCount returns the number of unconsumed queued messages.
	UnconsumedMessagesCount(agentID string) (int, error)
}

// RegisterQueuedTools registers the queued messages tools into d.
func RegisterQueuedTools(d *Dispatcher, store QueuedMessagesStore, getAgentID func() string) {
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
		return msg, nil
	}

	d.handlers["queued_messages_mark_all_consumed"] = func(ctx context.Context, _ json.RawMessage) (string, error) {
		agentID := getAgentID()
		if agentID == "" {
			return "", fmt.Errorf("no agent ID available")
		}
		count, err := store.MarkAllConsumed(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("mark all consumed: %w", err)
		}
		return fmt.Sprintf("marked %d messages as consumed", count), nil
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

// MemoryQueuedMessagesStore adapts a memory.Store to QueuedMessagesStore.
type MemoryQueuedMessagesStore struct {
	store *memory.Store
}

// GetQueuedMessages returns all queued messages with their consumption status.
func (s *MemoryQueuedMessagesStore) GetQueuedMessages(agentID string) ([]QueuedMessage, error) {
	c, err := s.store.ConversationGet(agentID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	if len(c.QueuedMsgs) == 0 {
		return nil, nil
	}

	// Parse queued messages - they can be either []string (old format) or []QueuedMessage (new format)
	var rawMsgs []json.RawMessage
	if err := json.Unmarshal(c.QueuedMsgs, &rawMsgs); err != nil {
		// Try parsing as array of strings (legacy format)
		var stringMsgs []string
		if err2 := json.Unmarshal(c.QueuedMsgs, &stringMsgs); err2 != nil {
			return nil, fmt.Errorf("unmarshal queued messages: %w", err)
		}
		// Convert to QueuedMessage format
		msgs := make([]QueuedMessage, len(stringMsgs))
		for i, txt := range stringMsgs {
			msgs[i] = QueuedMessage{Text: txt, Consumed: false}
		}
		return msgs, nil
	}

	// Parse each message - try QueuedMessage first, fall back to string
	msgs := make([]QueuedMessage, len(rawMsgs))
	for i, raw := range rawMsgs {
		var qm QueuedMessage
		if err := json.Unmarshal(raw, &qm); err == nil {
			msgs[i] = qm
		} else {
			// It's a plain string
			var txt string
			if err := json.Unmarshal(raw, &txt); err != nil {
				return nil, fmt.Errorf("unmarshal message %d: %w", i, err)
			}
			msgs[i] = QueuedMessage{Text: txt, Consumed: false}
		}
	}
	return msgs, nil
}

// saveQueuedMessages saves the queued messages back to the store.
func (s *MemoryQueuedMessagesStore) saveQueuedMessages(agentID string, msgs []QueuedMessage) error {
	// Marshal each message individually to preserve the structure
	rawMsgs := make([]json.RawMessage, len(msgs))
	for i, msg := range msgs {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal message %d: %w", i, err)
		}
		rawMsgs[i] = json.RawMessage(data)
	}
	data, err := json.Marshal(rawMsgs)
	if err != nil {
		return err
	}
	return s.store.ConversationUpdateQueuedMsgs(agentID, json.RawMessage(data))
}

// MarkConsumed marks a specific message as consumed and moves it to history.
func (s *MemoryQueuedMessagesStore) MarkConsumed(ctx context.Context, agentID string, index int) (string, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return "", err
	}
	if index < 0 || index >= len(msgs) {
		return "", fmt.Errorf("index %d out of range (0-%d)", index, len(msgs)-1)
	}
	if msgs[index].Consumed {
		return "", fmt.Errorf("message %d already consumed", index)
	}

	// Get the message text
	msgText := msgs[index].Text

	// Mark as consumed
	msgs[index].Consumed = true

	// Save updated queue
	if err := s.saveQueuedMessages(agentID, msgs); err != nil {
		return "", err
	}

	// Move consumed message to history
	c, err := s.store.ConversationGet(agentID)
	if err != nil {
		return "", err
	}

	var history []llm.Message
	if len(c.History) > 0 {
		if err := json.Unmarshal(c.History, &history); err != nil {
			return "", fmt.Errorf("unmarshal history: %w", err)
		}
	}

	// Append the consumed message to history
	history = append(history, llm.Message{Role: "user", Text: msgText})

	historyData, err := json.Marshal(history)
	if err != nil {
		return "", fmt.Errorf("marshal history: %w", err)
	}
	if err := s.store.ConversationUpdateHistory(agentID, json.RawMessage(historyData)); err != nil {
		return "", fmt.Errorf("update history: %w", err)
	}

	return fmt.Sprintf("message %d consumed: %s", index, truncateText(msgText, 50)), nil
}

// MarkAllConsumed marks all unconsumed messages as consumed and moves them to history.
func (s *MemoryQueuedMessagesStore) MarkAllConsumed(ctx context.Context, agentID string) (int, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return 0, err
	}

	consumedCount := 0
	for i := range msgs {
		if !msgs[i].Consumed {
			if _, err := s.MarkConsumed(ctx, agentID, i); err != nil {
				return consumedCount, fmt.Errorf("mark message %d consumed: %w", i, err)
			}
			consumedCount++
			// Re-fetch after each mark since the list changes
			msgs, err = s.GetQueuedMessages(agentID)
			if err != nil {
				return consumedCount, err
			}
		}
	}
	return consumedCount, nil
}

// QueueMessage adds a message to the queue for the given agent ID.
func (s *MemoryQueuedMessagesStore) QueueMessage(agentID, message string) error {
	c, err := s.store.ConversationGet(agentID)
	if err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("conversation %s not found", agentID)
	}

	var msgs []QueuedMessage
	if len(c.QueuedMsgs) > 0 {
		// Try to parse existing messages
		var rawMsgs []json.RawMessage
		if err := json.Unmarshal(c.QueuedMsgs, &rawMsgs); err == nil {
			// Parse as QueuedMessage or string
			for _, raw := range rawMsgs {
				var qm QueuedMessage
				if err := json.Unmarshal(raw, &qm); err == nil {
					msgs = append(msgs, qm)
				} else {
					var txt string
					if err := json.Unmarshal(raw, &txt); err == nil {
						msgs = append(msgs, QueuedMessage{Text: txt, Consumed: false})
					}
				}
			}
		} else {
			// Try parsing as array of strings
			var stringMsgs []string
			if err := json.Unmarshal(c.QueuedMsgs, &stringMsgs); err == nil {
				for _, txt := range stringMsgs {
					msgs = append(msgs, QueuedMessage{Text: txt, Consumed: false})
				}
			}
		}
	}
	msgs = append(msgs, QueuedMessage{Text: message, Consumed: false})

	return s.saveQueuedMessages(agentID, msgs)
}

// QueuedMessagesCount returns the total number of queued messages.
func (s *MemoryQueuedMessagesStore) QueuedMessagesCount(agentID string) (int, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

// UnconsumedMessagesCount returns the number of unconsumed queued messages.
func (s *MemoryQueuedMessagesStore) UnconsumedMessagesCount(agentID string) (int, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, msg := range msgs {
		if !msg.Consumed {
			count++
		}
	}
	return count, nil
}

// truncateText truncates text to maxLen characters.
func truncateText(text string, maxLen int) string {
	if len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "..."
}

// CleanupConsumedMessages removes all consumed messages from the queue.
// This is called automatically to keep the queue clean.
func (s *MemoryQueuedMessagesStore) CleanupConsumedMessages(agentID string) error {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return err
	}

	// Filter out consumed messages
	var remaining []QueuedMessage
	for _, msg := range msgs {
		if !msg.Consumed {
			remaining = append(remaining, msg)
		}
	}

	if len(remaining) == len(msgs) {
		return nil // Nothing to clean up
	}

	return s.saveQueuedMessages(agentID, remaining)
}


