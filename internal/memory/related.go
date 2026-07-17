package memory

// RelatedSession is a link from one session to a topically-similar prior
// session, with the similarity score that produced it.
type RelatedSession struct {
	RelatedAgentID string  `json:"related_agent_id"`
	Score          float32 `json:"score"`
	UpdatedAt      string  `json:"updated_at"`
}

// RelatedSessionAdd records (or refreshes) a link agentID → relatedAgentID with
// score. Self-links are ignored. The latest write wins per pair (upsert), so an
// at-least-once redelivery of the same event is idempotent.
func (s *Store) RelatedSessionAdd(agentID, relatedAgentID string, score float32) error {
	if agentID == "" || relatedAgentID == "" || agentID == relatedAgentID {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO related_sessions(agent_id, related_agent_id, score, updated_at)
		 VALUES(?, ?, ?, now())
		 ON CONFLICT (agent_id, related_agent_id)
		 DO UPDATE SET score = EXCLUDED.score, updated_at = now()`,
		agentID, relatedAgentID, score)
	return err
}

// RelatedSessions returns the sessions linked to agentID, most similar first.
func (s *Store) RelatedSessions(agentID string) ([]RelatedSession, error) {
	rows, err := s.db.Query(
		`SELECT related_agent_id, score, updated_at
		 FROM related_sessions WHERE agent_id = ? ORDER BY score DESC`,
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RelatedSession
	for rows.Next() {
		var r RelatedSession
		if err := rows.Scan(&r.RelatedAgentID, &r.Score, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
