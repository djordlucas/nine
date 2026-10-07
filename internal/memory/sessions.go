package memory

import (
	"database/sql"
	"fmt"
	"time"
)

// SessionSummary is one session as `nine sessions` lists it.
//
// Protected marks a session the retention reaper must never take: one that is
// still someone's job to run. See SessionsReapable for what that means and why
// it is decided here rather than by the caller.
type SessionSummary struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	AgeSeconds int    `json:"age_seconds"`
	Protected  bool   `json:"protected,omitempty"`
	// Events is how many journal rows the session still has — a rough measure of
	// how much would be lost, shown so `nine session delete` is an informed
	// decision rather than a blind one.
	Events int `json:"events,omitempty"`
}

// SessionDeleteCounts reports what a cascade removed, per table. It exists so a
// deletion can be logged and printed in full: "deleted session X" is not an
// auditable statement, and this is the one operation in Nine that destroys
// history.
type SessionDeleteCounts struct {
	Conversations      int `json:"conversations"`
	Events             int `json:"events"`
	Notifications      int `json:"notifications"`
	UserNotifications  int `json:"user_notifications"`
	Related            int `json:"related"`
	HumanRequests      int `json:"human_requests"`
	InteractiveMarkers int `json:"interactive"`
	ToolState          int `json:"tool_state"`
	Jobs               int `json:"jobs"`
}

// Total is every row the cascade removed.
func (c SessionDeleteCounts) Total() int {
	return c.Conversations + c.Events + c.Notifications + c.UserNotifications +
		c.Related + c.HumanRequests + c.InteractiveMarkers +
		c.ToolState + c.Jobs
}

// SessionList returns every session, most recently active first.
func (s *Store) SessionList() ([]SessionSummary, error) {
	rows, err := s.db.Query(
		`SELECT c.id, c.status, c.created_at, c.updated_at,
		        (SELECT count(*) FROM session_events e WHERE e.agent_id = c.id),
		        (SELECT count(*) FROM goals g
		           WHERE g.id = c.id AND g.status = 'active'),
		        (SELECT count(*) FROM processes p WHERE p.session_id = c.id)
		   FROM conversations c
		  ORDER BY c.updated_at DESC, c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// One `now` for the whole result set, so two sessions touched a microsecond
	// apart cannot report ages that disagree about their ordering.
	now := time.Now()
	var out []SessionSummary
	for rows.Next() {
		var (
			sum                   SessionSummary
			activeGoal, processes int
		)
		if err := rows.Scan(&sum.ID, &sum.Status, &sum.CreatedAt, &sum.UpdatedAt,
			&sum.Events, &activeGoal, &processes); err != nil {
			return nil, err
		}
		sum.Protected = activeGoal > 0 || processes > 0
		if t := parseStoredTime(sum.UpdatedAt); !t.IsZero() {
			if age := int(now.Sub(t).Seconds()); age > 0 {
				sum.AgeSeconds = age
			}
		}
		sum.Name = s.ConversationNameLoad(sum.ID)
		out = append(out, sum)
	}
	return out, rows.Err()
}

// SessionGet returns one session's summary, or (…, false, nil).
func (s *Store) SessionGet(id string) (SessionSummary, bool, error) {
	all, err := s.SessionList()
	if err != nil {
		return SessionSummary{}, false, err
	}
	for _, sum := range all {
		if sum.ID == id {
			return sum, true, nil
		}
	}
	return SessionSummary{}, false, nil
}

// SessionsReapable returns the sessions older than maxAge that the retention
// sweep may delete. maxAge <= 0 returns nothing (retention disabled).
//
// **Age is measured from last activity, not creation.** A session that has been
// running for a year and was used this morning is not stale; one created this
// morning and abandoned is not old enough. `updated_at` is what moves on a turn.
//
// Two kinds of session are **never** returned, and this is the part that makes
// automatic deletion safe to switch on:
//
//   - one whose id matches an **active goal**. A pursue session's id *is* its
//     goal id, so this is an exact test, not a heuristic.
//   - one a **process drives** (adr/process-sessions.md). That covers standing
//     agents declared in nine.toml, self-reflection, and every other process
//     session, stopped ones included: a stopped process can be started again.
//
// Both are idle by design: a standing agent that wakes weekly looks stale after
// ten days precisely because it is working correctly. Reaping either would
// silently dismantle configured behaviour, which is the failure this exclusion
// exists to prevent.
func (s *Store) SessionsReapable(maxAge time.Duration) ([]SessionSummary, error) {
	if maxAge <= 0 {
		return nil, nil
	}
	all, err := s.SessionList()
	if err != nil {
		return nil, err
	}
	cutoff := int(maxAge.Seconds())
	var out []SessionSummary
	for _, sum := range all {
		if sum.Protected || sum.AgeSeconds < cutoff {
			continue
		}
		out = append(out, sum)
	}
	return out, nil
}

// SessionDelete removes a session and everything keyed to it, in one
// transaction, and reports what it took.
//
// This is the only operation in Nine that destroys history rather than bounding
// it, so two properties are deliberate. It is **all or nothing** — a partial
// cascade would leave journal rows pointing at a conversation that no longer
// exists, which is worse than either outcome. And it **reports every count**, so
// the deletion can be logged in full rather than as a bare "deleted".
//
// Tool state is removed for the *conversation* scope only: a `scope_key` of the
// session id. Tool-scoped state (`scope_key = ”`) is shared across every caller
// and is nobody's session to delete (spec/contracts/toolvm.md R-TVM.18).
//
// A non-terminal job owned by this session is removed with it, which for the
// tool backend *is* the cancellation: the sweeper finds no row and makes no
// further call. A plugin job's goroutine lives in another process and cannot be
// stopped from here — the runtime attempts that before calling this.
func (s *Store) SessionDelete(id string) (SessionDeleteCounts, error) {
	var c SessionDeleteCounts
	if id == "" {
		return c, fmt.Errorf("session id is required")
	}

	tx, err := s.db.BeginWrite()
	if err != nil {
		return c, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	steps := []struct {
		into  *int
		query string
		args  []any
	}{
		{&c.Events, `DELETE FROM session_events WHERE agent_id = ?`, []any{id}},
		{&c.Notifications, `DELETE FROM notifications WHERE conversation_id = ?`, []any{id}},
		{&c.UserNotifications, `DELETE FROM user_notifications WHERE agent_id = ?`, []any{id}},
		// Both directions: a link is symmetric in meaning even though the primary
		// key is ordered, so leaving the mirror row would dangle.
		{&c.Related, `DELETE FROM related_sessions WHERE agent_id = ? OR related_agent_id = ?`, []any{id, id}},
		{&c.HumanRequests, `DELETE FROM human_requests WHERE agent_id = ?`, []any{id}},
		{&c.InteractiveMarkers, `DELETE FROM interactive_sessions WHERE id = ?`, []any{id}},
		{&c.ToolState, `DELETE FROM tool_state WHERE scope_key = ?`, []any{id}},
		{&c.Jobs, `DELETE FROM jobs WHERE owner_id = ?`, []any{id}},
		{&c.Conversations, `DELETE FROM conversations WHERE id = ?`, []any{id}},
	}
	for _, st := range steps {
		res, err := tx.Exec(st.query, st.args...)
		if err != nil {
			return c, fmt.Errorf("session delete %s: %w", id, err)
		}
		n, _ := res.RowsAffected()
		*st.into = int(n)
	}

	// The display name lives in kv under a per-session key rather than on the
	// conversation row, so it needs removing by hand or it outlives its session.
	if _, err := tx.Exec(`DELETE FROM kv WHERE key = ?`, "name:"+id); err != nil {
		return c, fmt.Errorf("session delete %s: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return c, err
	}
	return c, nil
}

// SessionExists reports whether a conversation row exists for id.
func (s *Store) SessionExists(id string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM conversations WHERE id = ?`, id).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
