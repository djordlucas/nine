package memory

// UserNotification is one row from the user_notifications table — a
// human-facing message posted by a background agent that has no interactive
// conversation (docs/predefined-agents.md §5 piece 2). Distinct from the
// per-session `notifications` table, which feeds an agent's own next turn.
type UserNotification struct {
	ID        string `json:"id"`
	AgentID   string `json:"agent_id,omitempty"`
	Message   string `json:"message"`
	Seen      bool   `json:"seen"`
	CreatedAt string `json:"created_at"`
}

// UserNotificationCreate posts a message to the human-facing feed, attributed
// to agentID. Silently ignores duplicate ids.
func (s *Store) UserNotificationCreate(id, agentID, message string) error {
	_, err := s.db.Exec(
		`INSERT INTO user_notifications(id, agent_id, message)
		 VALUES(?,?,?)
		 ON CONFLICT (id) DO NOTHING`,
		id, agentID, message)
	return err
}

// UserNotificationList returns feed entries oldest-first. When unseenOnly is
// true only entries not yet marked seen are returned.
func (s *Store) UserNotificationList(unseenOnly bool) ([]UserNotification, error) {
	query := `SELECT id, agent_id, message, seen, created_at
	          FROM user_notifications`
	if unseenOnly {
		query += ` WHERE seen = 0`
	}
	query += ` ORDER BY created_at, id`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ns []UserNotification
	for rows.Next() {
		var n UserNotification
		var seen int
		if err := rows.Scan(&n.ID, &n.AgentID, &n.Message, &seen, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Seen = seen == 1
		ns = append(ns, n)
	}
	return ns, rows.Err()
}

// UserNotificationMarkSeen marks a single feed entry as seen.
func (s *Store) UserNotificationMarkSeen(id string) error {
	_, err := s.db.Exec(`UPDATE user_notifications SET seen=1 WHERE id=?`, id)
	return err
}

// UserNotificationCountUnseen returns how many feed entries are not yet seen —
// the number a status badge would surface.
func (s *Store) UserNotificationCountUnseen() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM user_notifications WHERE seen = 0`).Scan(&n)
	return n, err
}
