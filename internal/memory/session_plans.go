package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// SessionRoutine is one stage in a session plan's lifecycle. See
// docs/session-plans.md for the full design.
type SessionRoutine struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"` // pending | active | paused | done | skipped
	Config    json.RawMessage `json:"config,omitempty"`
	Result    string          `json:"result,omitempty"`
	UpdatedAt string          `json:"updated_at,omitempty"`
}

// SessionPlan is one row from the session_plans table.
type SessionPlan struct {
	ID        string           `json:"id"`
	Status    string           `json:"status"` // active | paused | archived
	Routines  []SessionRoutine `json:"routines"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
}

func scanSessionPlan(row interface{ Scan(...any) error }) (SessionPlan, error) {
	var p SessionPlan
	var routinesJSON string
	if err := row.Scan(&p.ID, &p.Status, &routinesJSON, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return SessionPlan{}, err
	}
	if err := json.Unmarshal([]byte(routinesJSON), &p.Routines); err != nil {
		return SessionPlan{}, fmt.Errorf("unmarshal routines: %w", err)
	}
	return p, nil
}

// SessionPlanGet returns a session plan by id. Returns (nil, nil) if not found.
func (s *Store) SessionPlanGet(id string) (*SessionPlan, error) {
	row := s.db.QueryRow(
		`SELECT id, status, routines, created_at, updated_at FROM session_plans WHERE id = ?`, id)
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
	routinesJSON, err := json.Marshal(plan.Routines)
	if err != nil {
		return fmt.Errorf("marshal routines: %w", err)
	}
	status := plan.Status
	if status == "" {
		status = "active"
	}
	_, err = s.db.Exec(
		`INSERT INTO session_plans(id, status, routines, updated_at)
		 VALUES(?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET status=excluded.status, routines=excluded.routines, updated_at=excluded.updated_at`,
		plan.ID, status, string(routinesJSON), nowText())
	return err
}

// SessionPlanListActive returns all session plans with status "active".
func (s *Store) SessionPlanListActive() ([]SessionPlan, error) {
	rows, err := s.db.Query(
		`SELECT id, status, routines, created_at, updated_at FROM session_plans WHERE status = 'active'`)
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
