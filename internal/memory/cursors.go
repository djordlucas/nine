package memory

import "database/sql"

// SessionEventsAfter returns up to limit events with seq > afterSeq, in seq
// (causal) order across all agents. It is the forward read path for cursor-based
// journal subscribers (adr/reactive-events.md §3); pass the last processed seq
// to get the next batch.
func (s *Store) SessionEventsAfter(afterSeq int64, limit int) ([]SessionEvent, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.db.Query(
		`SELECT seq, agent_id, turn, span_id, parent_span_id, type, ts, payload
		 FROM session_events WHERE seq > ? ORDER BY seq LIMIT ?`,
		afterSeq, limit)
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

// EventCursorGet returns the last seq subscriberID has processed, or 0 if it has
// no recorded cursor yet.
func (s *Store) EventCursorGet(subscriberID string) (int64, error) {
	var seq int64
	err := s.db.QueryRow(
		`SELECT seq FROM event_cursors WHERE subscriber_id = ?`, subscriberID).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return seq, err
}

// EventCursorSet persists subscriberID's cursor position (upsert).
func (s *Store) EventCursorSet(subscriberID string, seq int64) error {
	_, err := s.db.Exec(
		`INSERT INTO event_cursors(subscriber_id, seq) VALUES(?, ?)
		 ON CONFLICT (subscriber_id) DO UPDATE SET seq = EXCLUDED.seq`,
		subscriberID, seq)
	return err
}
