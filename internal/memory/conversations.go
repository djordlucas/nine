package memory

import (
	"database/sql"
	"encoding/json"
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

// ConversationUpdateQueuedMsgs updates the queued messages blob for a conversation.
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
func (s *Store) ConversationSave(id string, data []byte) error {
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
	return s.ConversationUpdateQueuedMsgs(id, cp.QueuedMsgs)
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
