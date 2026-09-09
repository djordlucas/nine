package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Conversation is one row from the conversations table.
type Conversation struct {
	ID           string          `json:"id"`
	History      json.RawMessage `json:"history,omitempty"`
	Scratchpad   json.RawMessage `json:"scratchpad,omitempty"`
	Status       string          `json:"status"`
	CreatedAt    string          `json:"created_at"`
	UpdatedAt    string          `json:"updated_at"`
	QueuedMsgs   json.RawMessage `json:"queued_messages,omitempty"`
}

// QueuedMessage represents a single queued user message with its consumption status.
type QueuedMessage struct {
	Text      string `json:"text"`
	Consumed bool   `json:"consumed"`
}

// GetQueuedMessages returns all queued messages for a conversation.
func (s *Store) GetQueuedMessages(agentID string) ([]QueuedMessage, error) {
	c, err := s.ConversationGet(agentID)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil
	}
	if len(c.QueuedMsgs) == 0 || string(c.QueuedMsgs) == "" {
		return nil, nil
	}
	var msgs []QueuedMessage
	if err := json.Unmarshal(c.QueuedMsgs, &msgs); err != nil {
		return nil, fmt.Errorf("unmarshal queued messages: %w", err)
	}
	return msgs, nil
}

// QueueMessage adds a message to the queue for the given agent ID.
func (s *Store) QueueMessage(agentID, message string) error {
	if err := s.ConversationCreate(agentID); err != nil {
		return err
	}
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return err
	}
	msgs = append(msgs, QueuedMessage{Text: message, Consumed: false})
	data, err := json.Marshal(msgs)
	if err != nil {
		return err
	}
	return s.ConversationUpdateQueuedMsgs(agentID, data)
}

// MarkConsumed marks the message at the given index as consumed and returns
// its text. The caller is responsible for appending the text to conversation
// history — the store no longer writes to history directly, because the
// end-of-turn checkpoint would clobber it.
func (s *Store) MarkConsumed(ctx context.Context, agentID string, index int) (string, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return "", err
	}
	if index < 0 || index >= len(msgs) {
		return "", fmt.Errorf("index %d out of range (have %d messages)", index, len(msgs))
	}
	msgs[index].Consumed = true
	data, err := json.Marshal(msgs)
	if err != nil {
		return "", err
	}
	if err := s.ConversationUpdateQueuedMsgs(agentID, data); err != nil {
		return "", err
	}
	return msgs[index].Text, nil
}

// MarkAllConsumed marks all unconsumed messages as consumed and returns their
// texts. The caller is responsible for appending them to conversation history.
func (s *Store) MarkAllConsumed(ctx context.Context, agentID string) ([]string, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return nil, err
	}
	var consumed []string
	for i := range msgs {
		if !msgs[i].Consumed {
			msgs[i].Consumed = true
			consumed = append(consumed, msgs[i].Text)
		}
	}
	if len(consumed) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(msgs)
	if err != nil {
		return consumed, err
	}
	return consumed, s.ConversationUpdateQueuedMsgs(agentID, data)
}

// QueuedMessagesCount returns the total number of queued messages.
func (s *Store) QueuedMessagesCount(agentID string) (int, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return 0, err
	}
	return len(msgs), nil
}

// UnconsumedMessagesCount returns the number of unconsumed queued messages.
func (s *Store) UnconsumedMessagesCount(agentID string) (int, error) {
	msgs, err := s.GetQueuedMessages(agentID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, m := range msgs {
		if !m.Consumed {
			count++
		}
	}
	return count, nil
}

// ConversationCreate creates the conversation row if it doesn't already exist.
func (s *Store) ConversationCreate(id string) error {
	_, err := s.db.Exec(`INSERT INTO conversations(id) VALUES(?) ON CONFLICT (id) DO NOTHING`, id)
	return err
}

// ConversationGet returns the conversation by id. Returns (nil, nil) if not found.
func (s *Store) ConversationGet(id string) (*Conversation, error) {
	var c Conversation
	var historyStr, scratchpadStr, queuedMsgsStr string
	err := s.db.QueryRow(
		`SELECT id, history, scratchpad, status, created_at, updated_at, queued_messages
		 FROM conversations WHERE id = ?`, id).
		Scan(&c.ID, &historyStr, &scratchpadStr, &c.Status, &c.CreatedAt, &c.UpdatedAt, &queuedMsgsStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.History = json.RawMessage(historyStr)
	c.Scratchpad = json.RawMessage(scratchpadStr)
	c.QueuedMsgs = json.RawMessage(queuedMsgsStr)
	return &c, nil
}

// ConversationUpdateHistory updates the history blob for a conversation.
func (s *Store) ConversationUpdateHistory(id string, history json.RawMessage) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET history=?, updated_at=? WHERE id=?`,
		string(history), nowText(), id)
	return err
}

// ConversationUpdateScratchpad updates the scratchpad blob for a conversation.
func (s *Store) ConversationUpdateScratchpad(id string, scratchpad json.RawMessage) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET scratchpad=?, updated_at=? WHERE id=?`,
		string(scratchpad), nowText(), id)
	return err
}
func (s *Store) ConversationUpdateQueuedMsgs(id string, queuedMsgs json.RawMessage) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET queued_messages=?, updated_at=? WHERE id=?`,
		string(queuedMsgs), nowText(), id)
	return err
}

// ConversationSetStatus updates the status field for a conversation.
func (s *Store) ConversationSetStatus(id, status string) error {
	_, err := s.db.Exec(
		`UPDATE conversations SET status=?, updated_at=? WHERE id=?`,
		status, nowText(), id)
	return err
}

// ConversationLoad returns (data, true, nil) where data is the JSON-encoded
// {history, scratchpad, queued_messages} blob used by the checkpoint store. Returns (nil, false, nil)
// if not found.
func (s *Store) ConversationLoad(id string) ([]byte, bool, error) {
	c, err := s.ConversationGet(id)
	if err != nil {
		return nil, false, err
	}
	if c == nil {
		return nil, false, nil
	}
	data, err := json.Marshal(struct {
		History      json.RawMessage `json:"history,omitempty"`
		Scratchpad   json.RawMessage `json:"scratchpad,omitempty"`
		QueuedMsgs   json.RawMessage `json:"queued_messages,omitempty"`
	}{c.History, c.Scratchpad, c.QueuedMsgs})
	return data, err == nil, err
}

// ConversationSave upserts a conversation's history, scratchpad, and queued messages from a
// JSON blob of the form {history: [...], scratchpad: [...], queued_messages: [...]}.
// Queued messages are only overwritten if the blob includes the field — the
// checkpoint (loop.SaveState) does not, so preserving the queue avoids clobbering
// messages added while a turn was in flight.
func (s *Store) ConversationSave(id string, data []byte) error {
	// Check if the queued_messages field is explicitly present in the blob.
	// A checkpoint from loop.SaveState omits it entirely; we must not clobber
	// the queue in that case. Use a raw decode to detect field presence.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	_, hasQueued := raw["queued_messages"]

	var cp struct {
		History      json.RawMessage `json:"history,omitempty"`
		Scratchpad   json.RawMessage `json:"scratchpad,omitempty"`
		QueuedMsgs   json.RawMessage `json:"queued_messages,omitempty"`
	}
	if err := json.Unmarshal(data, &cp); err != nil {
		return err
	}
	if err := s.ConversationCreate(id); err != nil {
		return err
	}
	if err := s.ConversationUpdateHistory(id, cp.History); err != nil {
		return err
	}
	if err := s.ConversationUpdateScratchpad(id, cp.Scratchpad); err != nil {
		return err
	}
	// Only overwrite queued messages if the blob explicitly includes the field.
	// A checkpoint from loop.SaveState omits it, and overwriting with empty
	// would erase messages queued by the daemon while a turn was in flight.
	if hasQueued {
		return s.ConversationUpdateQueuedMsgs(id, cp.QueuedMsgs)
	}
	return nil
}

// ConversationDelete removes a conversation row (its checkpointed history and
// scratchpad). It is a no-op if no such row exists, so terminating an
// already-gone session is not an error.
func (s *Store) ConversationDelete(id string) error {
	_, err := s.db.Exec(`DELETE FROM conversations WHERE id = ?`, id)
	return err
}

// ConversationNameSave stores the display name for a conversation in kv.
func (s *Store) ConversationNameSave(id, name string) error {
	return s.Set("name:"+id, name)
}

// ConversationNameLoad returns the display name for a conversation, or "".
func (s *Store) ConversationNameLoad(id string) string {
	v, _, _ := s.Get("name:" + id)
	if strings.HasPrefix(v, "") { // always true, just for clarity
		return v
	}
	return v
}
