package memory

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// SessionEvent is one row of the append-only session execution journal
// (adr/event-log.md §6). It captures a single step of a session's execution —
// a turn boundary, an LLM request/response, a tool call, a context update, or a
// sub-agent lifecycle event — as a durable, causally-ordered record. Seq and TS
// are assigned by the database on insert.
type SessionEvent struct {
	Seq          int64           `json:"seq"`
	AgentID      string          `json:"agent_id"`
	Turn         int             `json:"turn"`
	SpanID       string          `json:"span_id"`
	ParentSpanID string          `json:"parent_span_id,omitempty"`
	Type         string          `json:"type"`
	TS           time.Time       `json:"ts"`
	Payload      json.RawMessage `json:"payload"`
}

// SessionEventsAppend inserts a batch of events in one round-trip. Seq is
// database-assigned and TS is stamped here; the passed values for those fields
// are ignored. An empty batch is a no-op.
func (s *Store) SessionEventsAppend(evs []SessionEvent) error {
	if len(evs) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO session_events(agent_id, turn, span_id, parent_span_id, type, payload, ts) VALUES `)
	args := make([]any, 0, len(evs)*7)
	// One timestamp for the whole batch rather than one per event: the previous
	// backend's now() was transaction time and constant across the multi-row
	// INSERT, and preserving that keeps within-batch ordering carried purely by
	// seq, which is what replay already assumes.
	ts := nowText()
	for i, e := range evs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("(?,?,?,?,?,?,?)")
		parent := sql.NullString{String: e.ParentSpanID, Valid: e.ParentSpanID != ""}
		payload := e.Payload
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		args = append(args, e.AgentID, e.Turn, e.SpanID, parent, e.Type, string(payload), ts)
	}
	_, err := s.db.Exec(b.String(), args...)
	return err
}

// SessionEventsScrub bounds journal growth (adr/event-log.md §9, v4). It prunes,
// per agent, every event that falls outside the most recent keepTurns turns, and
// (independently) every event older than maxAge. A row is removed if it violates
// either bound. keepTurns <= 0 disables turn-based pruning; maxAge <= 0 disables
// age-based pruning. Returns the total number of rows deleted. Intended to run at
// boot, like WorkflowScrub.
func (s *Store) SessionEventsScrub(keepTurns int, maxAge time.Duration) (int, error) {
	total := 0
	if keepTurns > 0 {
		// Keep the last keepTurns turns per agent; drop everything below that.
		res, err := s.db.Exec(
			`DELETE FROM session_events
			 WHERE seq IN (
			   SELECT e.seq FROM session_events e
			   JOIN (SELECT agent_id, max(turn) AS mx FROM session_events GROUP BY agent_id) m
			     ON m.agent_id = e.agent_id
			   WHERE e.turn <= m.mx - ?)`,
			keepTurns)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	if maxAge > 0 {
		res, err := s.db.Exec(
			`DELETE FROM session_events WHERE ts < ?`,
			writeTime(time.Now().Add(-maxAge)))
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// LatestTurnResult returns the answer text of the most recent completed turn
// (the last turn_end event) for agentID, or "" if it has none. It backs the
// related-session enrichment the context builder surfaces on a later turn
// (adr/reactive-events.md §5): a one-line gist of what a linked prior session
// concluded, so the surfaced link is meaningful rather than an opaque id.
func (s *Store) LatestTurnResult(agentID string) (string, error) {
	var payload []byte
	err := s.db.QueryRow(
		`SELECT payload FROM session_events
		 WHERE agent_id = ? AND type = 'turn_end'
		 ORDER BY seq DESC LIMIT 1`, agentID).Scan(&payload)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var p struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return "", nil // malformed payload is not fatal to a turn
	}
	return strings.TrimSpace(p.Result), nil
}

// SessionEventsByAgent returns every journaled event for agentID in causal
// (seq) order. It is the read path behind observational replay (adr/event-log.md
// §3, §10).
func (s *Store) SessionEventsByAgent(agentID string) ([]SessionEvent, error) {
	rows, err := s.db.Query(
		`SELECT seq, agent_id, turn, span_id, parent_span_id, type, ts, payload
		 FROM session_events WHERE agent_id = ? ORDER BY seq`,
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var evs []SessionEvent
	for rows.Next() {
		var (
			e       SessionEvent
			parent  sql.NullString
			payload []byte
			ts      string
		)
		if err := rows.Scan(&e.Seq, &e.AgentID, &e.Turn, &e.SpanID, &parent, &e.Type, &ts, &payload); err != nil {
			return nil, err
		}
		// ts is parsed explicitly rather than scanned straight into a time.Time.
		// The driver can be asked to convert TEXT columns automatically, but that
		// would make the store's behaviour depend on a DSN flag that differs
		// between the writer and the read-only CLI pool — two explicit parses are
		// better than that kind of action at a distance.
		e.TS = parseStoredTime(ts)
		e.ParentSpanID = parent.String
		e.Payload = payload
		evs = append(evs, e)
	}
	return evs, rows.Err()
}
