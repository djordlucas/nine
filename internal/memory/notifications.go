package memory

import "database/sql"

// Notification is one row from the notifications table.
type Notification struct {
	ID               string `json:"id"`
	ConversationID   string `json:"conversation_id,omitempty"`
	Message          string `json:"message"`
	RequiresApproval bool   `json:"requires_approval"`
	Delivered        bool   `json:"delivered"`
	CreatedAt        string `json:"created_at"`
}

// NotificationCreate creates a notification. Silently ignores duplicates.
func (s *Store) NotificationCreate(id, conversationID, message string, requiresApproval bool) error {
	convID := sql.NullString{String: conversationID, Valid: conversationID != ""}
	ra := 0
	if requiresApproval {
		ra = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO notifications(id, conversation_id, message, requires_approval)
		 VALUES(?,?,?,?)
		 ON CONFLICT (id) DO NOTHING`,
		id, convID, message, ra)
	return err
}

// NotificationListPending returns undelivered notifications for conversationID.
// Pass "" to return all undelivered notifications.
func (s *Store) NotificationListPending(conversationID string) ([]Notification, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if conversationID == "" {
		rows, err = s.db.Query(
			`SELECT id, conversation_id, message, requires_approval, delivered, created_at
			 FROM notifications WHERE delivered = 0 ORDER BY created_at`)
	} else {
		rows, err = s.db.Query(
			`SELECT id, conversation_id, message, requires_approval, delivered, created_at
			 FROM notifications
			 WHERE delivered = 0
			   AND (conversation_id IS NULL OR conversation_id = ?)
			 ORDER BY created_at`,
			conversationID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ns []Notification
	for rows.Next() {
		var n Notification
		var convID sql.NullString
		var ra, del int
		if err := rows.Scan(&n.ID, &convID, &n.Message, &ra, &del, &n.CreatedAt); err != nil {
			return nil, err
		}
		if convID.Valid {
			n.ConversationID = convID.String
		}
		n.RequiresApproval = ra == 1
		n.Delivered = del == 1
		ns = append(ns, n)
	}
	return ns, rows.Err()
}

// NotificationMarkDelivered marks a notification as delivered.
func (s *Store) NotificationMarkDelivered(id string) error {
	_, err := s.db.Exec(`UPDATE notifications SET delivered=1 WHERE id=?`, id)
	return err
}
