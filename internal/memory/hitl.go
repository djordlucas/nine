package memory

import (
	"database/sql"
	"encoding/json"
	"time"
)

// HumanRequest is one row from the human_requests table — a pending,
// answered, or timed-out request for human input raised by an interactive
// session's ask_human tool or an approval gate.
type HumanRequest struct {
	ID         string    `json:"id"`
	AgentID    string    `json:"agent_id"`
	Question   string    `json:"question"`
	Options    []string  `json:"options,omitempty"`
	Status     string    `json:"status"` // pending | answered | timed_out
	Answer     string    `json:"answer,omitempty"`
	CreatedAt  string    `json:"created_at"`
	AnsweredAt string    `json:"answered_at,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// HumanRequestCreate inserts a new pending request. options is stored as a
// JSON array (NULL when empty). Times are stored as RFC3339 strings.
func (s *Store) HumanRequestCreate(id, agentID, question string, options []string, expiresAt time.Time) error {
	var opts sql.NullString
	if len(options) > 0 {
		b, err := json.Marshal(options)
		if err != nil {
			return err
		}
		opts = sql.NullString{String: string(b), Valid: true}
	}
	_, err := s.db.Exec(
		`INSERT INTO human_requests(id, agent_id, question, options, status, created_at, expires_at)
		 VALUES(?,?,?,?,'pending',?,?)`,
		id, agentID, question, opts,
		time.Now().UTC().Format(time.RFC3339),
		expiresAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return err
	}
	s.journalState(agentID, EvHumanInputRequested, map[string]any{
		"id": id, "question": question, "options": options,
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
	})
	return nil
}

// HumanRequestGetPending returns the most recent pending request for agentID,
// or nil if none is pending.
func (s *Store) HumanRequestGetPending(agentID string) (*HumanRequest, error) {
	row := s.db.QueryRow(
		`SELECT id, agent_id, question, options, status, answer, created_at, answered_at, expires_at
		 FROM human_requests
		 WHERE agent_id = ? AND status = 'pending'
		 ORDER BY created_at DESC
		 LIMIT 1`,
		agentID)
	return scanHumanRequest(row)
}

// HumanRequestListPending returns every request still waiting for an answer,
// oldest first. One past its deadline is left out: its session has stopped
// waiting, and the row is marked timed_out at the next boot.
func (s *Store) HumanRequestListPending() ([]HumanRequest, error) {
	rows, err := s.db.Query(
		`SELECT id, agent_id, question, options, status, answer, created_at, answered_at, expires_at
		 FROM human_requests
		 WHERE status = 'pending' AND expires_at > ?
		 ORDER BY created_at ASC, id ASC`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := []HumanRequest{}
	for rows.Next() {
		hr, err := scanHumanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *hr)
	}
	return out, rows.Err()
}

// HumanRequestAnswer marks a request answered, recording the answer and time.
func (s *Store) HumanRequestAnswer(id, answer string) error {
	var agentID string
	err := s.db.QueryRow(
		`UPDATE human_requests
		 SET status = 'answered', answer = ?, answered_at = ?
		 WHERE id = ?
		 RETURNING agent_id`,
		answer, time.Now().UTC().Format(time.RFC3339), id).Scan(&agentID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	s.journalState(agentID, EvHumanInputAnswered, map[string]any{"id": id, "answer": answer})
	return nil
}

// HumanRequestMarkTimedOut marks a request timed_out. Used on expiry,
// cancellation, or daemon shutdown.
func (s *Store) HumanRequestMarkTimedOut(id string) error {
	var agentID string
	err := s.db.QueryRow(
		`UPDATE human_requests SET status = 'timed_out' WHERE id = ? AND status = 'pending'
		 RETURNING agent_id`,
		id).Scan(&agentID)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	s.journalState(agentID, EvHumanInputTimedOut, map[string]any{"id": id})
	return nil
}

// HumanRequestExpireStale marks every pending request whose expires_at is at or
// before now as timed_out. Called once at daemon startup.
func (s *Store) HumanRequestExpireStale(now time.Time) error {
	rows, err := s.db.Query(
		`UPDATE human_requests SET status = 'timed_out'
		 WHERE status = 'pending' AND expires_at <= ?
		 RETURNING id, agent_id`,
		now.UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	type expired struct{ id, agentID string }
	var done []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.id, &e.agentID); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		done = append(done, e)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range done {
		s.journalState(e.agentID, EvHumanInputTimedOut, map[string]any{"id": e.id})
	}
	return nil
}

// InteractiveSessionAdd records agentID as an interactive session so the flag
// survives a daemon restart. Idempotent.
func (s *Store) InteractiveSessionAdd(id string) error {
	_, err := s.db.Exec(`INSERT INTO interactive_sessions(id) VALUES(?) ON CONFLICT (id) DO NOTHING`, id)
	return err
}

// InteractiveSessionExists reports whether agentID was started interactively.
func (s *Store) InteractiveSessionExists(id string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM interactive_sessions WHERE id = ?`, id).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func scanHumanRequest(row interface{ Scan(...any) error }) (*HumanRequest, error) {
	var (
		hr         HumanRequest
		opts       sql.NullString
		answer     sql.NullString
		answeredAt sql.NullString
		expiresAt  string
	)
	err := row.Scan(&hr.ID, &hr.AgentID, &hr.Question, &opts, &hr.Status, &answer, &hr.CreatedAt, &answeredAt, &expiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if opts.Valid && opts.String != "" {
		json.Unmarshal([]byte(opts.String), &hr.Options) //nolint:errcheck // best-effort; absent options just render as none
	}
	if answer.Valid {
		hr.Answer = answer.String
	}
	if answeredAt.Valid {
		hr.AnsweredAt = answeredAt.String
	}
	if t, perr := time.Parse(time.RFC3339, expiresAt); perr == nil {
		hr.ExpiresAt = t
	}
	return &hr, nil
}
