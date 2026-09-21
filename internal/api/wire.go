package api

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The daemon answers list queries with a single-key object wrapping the rows —
// `{"goals": [...]}`, `{"workflows": [...]}`, `{"notifications": [...]}` — and
// falls back to a bare `[]` for goals and workflows when it has no store. The
// types below match those payloads exactly so the API layer decodes what the
// daemon actually sends rather than what its own response schema wishes it sent.
//
// Decoding straight into the public response types silently failed on every
// non-empty list: the envelope is an object, the target was an array, and the
// error path returned the raw JSON as a string under `data`. Empty lists
// decoded fine, so the endpoints looked correct until they had something to
// report.

// wireGoal is one row of the daemon's `list_goals` reply (memory.Goal).
type wireGoal struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	ParentID    string   `json:"parent_id,omitempty"`
	ParentType  string   `json:"parent_type,omitempty"`
	Subtree     []string `json:"subtree"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// wireWorkflow is one row of the daemon's `list_workflows` reply
// (workflow.Workflow). Steps is the full step list, not a count.
type wireWorkflow struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	AgentID   string     `json:"agent_id"`
	Steps     []wireStep `json:"steps"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
}

// wireStep is one step of a workflow (workflow.Step).
type wireStep struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"`
}

// wireNotification is one row of the daemon's `list_notifications` reply
// (memory.UserNotification).
type wireNotification struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id,omitempty"`
	Message   string `json:"message"`
	Seen      bool   `json:"seen"`
	CreatedAt string `json:"created_at"`
}

// decodeList unwraps the daemon's single-key list envelope. It also accepts a
// bare array, which the daemon emits for goals and workflows when it is running
// without a store.
func decodeList[T any](raw, key string) ([]T, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}

	if trimmed[0] == '[' {
		var rows []T
		if err := json.Unmarshal([]byte(trimmed), &rows); err != nil {
			return nil, fmt.Errorf("decode %s array: %w", key, err)
		}
		return rows, nil
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &envelope); err != nil {
		return nil, fmt.Errorf("decode %s envelope: %w", key, err)
	}
	body, ok := envelope[key]
	if !ok {
		return nil, fmt.Errorf("decode %s envelope: no %q key", key, key)
	}
	var rows []T
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("decode %s rows: %w", key, err)
	}
	return rows, nil
}

// parseStoredTime reads the fixed-width RFC3339 UTC timestamp every nine store
// writes (see memory's stored-timestamp invariant). An unparseable value yields
// the zero time rather than an error: a malformed timestamp on one row should
// not fail the whole list.
func parseStoredTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// toGoalInfo maps a daemon goal row to the API representation.
func toGoalInfo(g wireGoal) GoalInfo {
	return GoalInfo{
		ID:          g.ID,
		Description: g.Description,
		Status:      g.Status,
		ParentID:    g.ParentID,
		ParentType:  g.ParentType,
		Subtree:     g.Subtree,
		CreatedAt:   parseStoredTime(g.CreatedAt),
		UpdatedAt:   parseStoredTime(g.UpdatedAt),
	}
}

// toWorkflowInfo maps a daemon workflow row to the API representation,
// summarising the step list as a total and a completed count.
func toWorkflowInfo(w wireWorkflow) WorkflowInfo {
	completed := 0
	for _, step := range w.Steps {
		switch step.Status {
		case "done", "skipped":
			completed++
		}
	}

	return WorkflowInfo{
		ID:             w.ID,
		Name:           w.Name,
		Status:         w.Status,
		AgentID:        w.AgentID,
		Steps:          len(w.Steps),
		CompletedSteps: completed,
		CreatedAt:      parseStoredTime(w.CreatedAt),
		UpdatedAt:      parseStoredTime(w.UpdatedAt),
	}
}

// toNotification maps a daemon notification row to the API representation.
func toNotification(n wireNotification) Notification {
	return Notification{
		ID:        n.ID,
		AgentID:   n.AgentID,
		Message:   n.Message,
		Seen:      n.Seen,
		CreatedAt: parseStoredTime(n.CreatedAt),
	}
}
