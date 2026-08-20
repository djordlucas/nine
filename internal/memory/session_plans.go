package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// SessionAspect is one stage in a session plan's lifecycle. See
// docs/session-plans.md for the full design.
type SessionAspect struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"` // pending | active | paused | done | skipped
	Config    json.RawMessage `json:"config,omitempty"`
	Result    string          `json:"result,omitempty"`
	UpdatedAt string          `json:"updated_at,omitempty"`
}

// SessionPlan is one row from the session_plans table.
type SessionPlan struct {
	ID        string          `json:"id"`
	Status    string          `json:"status"` // active | paused | archived
	Aspects   []SessionAspect `json:"aspects"`
	CreatedAt string          `json:"created_at"`
	UpdatedAt string          `json:"updated_at"`
}

func scanSessionPlan(row interface{ Scan(...any) error }) (SessionPlan, error) {
	var p SessionPlan
	var aspectsJSON string
	if err := row.Scan(&p.ID, &p.Status, &aspectsJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return SessionPlan{}, err
	}
	if err := json.Unmarshal([]byte(aspectsJSON), &p.Aspects); err != nil {
		return SessionPlan{}, fmt.Errorf("unmarshal aspects: %w", err)
	}
	return p, nil
}

// SessionPlanGet returns a session plan by id. Returns (nil, nil) if not found.
func (s *Store) SessionPlanGet(id string) (*SessionPlan, error) {
	row := s.db.QueryRow(
		`SELECT id, status, aspects, created_at, updated_at FROM session_plans WHERE id = ?`, id)
	p, err := scanSessionPlan(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SessionPlanSave creates or updates a session plan row, keyed by plan.ID.
func (s *Store) SessionPlanSave(plan *SessionPlan) error {
	aspectsJSON, err := json.Marshal(plan.Aspects)
	if err != nil {
		return fmt.Errorf("marshal aspects: %w", err)
	}
	status := plan.Status
	if status == "" {
		status = "active"
	}
	_, err = s.db.Exec(
		`INSERT INTO session_plans(id, status, aspects, updated_at)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET status=excluded.status, aspects=excluded.aspects, updated_at=excluded.updated_at`,
		plan.ID, status, string(aspectsJSON), nowText())
	return err
}

// SessionPlanListActive returns all session plans with status "active".
func (s *Store) SessionPlanListActive() ([]SessionPlan, error) {
	rows, err := s.db.Query(
		`SELECT id, status, aspects, created_at, updated_at FROM session_plans WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var plans []SessionPlan
	for rows.Next() {
		p, err := scanSessionPlan(rows)
		if err != nil {
			return nil, err
		}
		plans = append(plans, p)
	}
	return plans, rows.Err()
}
