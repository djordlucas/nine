package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Goal is one row from the goals table.
type Goal struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	ParentID    string   `json:"parent_id,omitempty"`
	ParentType  string   `json:"parent_type,omitempty"`
	Subtree     []string `json:"subtree"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

func scanGoal(row interface{ Scan(...any) error }) (Goal, error) {
	var g Goal
	var subtreeJSON string
	var parentID, parentType sql.NullString
	err := row.Scan(&g.ID, &g.Description, &g.Status, &subtreeJSON,
		&parentID, &parentType, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return Goal{}, err
	}
	if err := json.Unmarshal([]byte(subtreeJSON), &g.Subtree); err != nil {
		return Goal{}, fmt.Errorf("unmarshal subtree: %w", err)
	}
	if parentID.Valid {
		g.ParentID = parentID.String
	}
	if parentType.Valid {
		g.ParentType = parentType.String
	}
	return g, nil
}

// GoalCreate creates a new goal. Silently ignores duplicates.
func (s *Store) GoalCreate(id, description, parentID, parentType string) error {
	_, err := s.db.Exec(
		`INSERT INTO goals(id, description, parent_id, parent_type)
		 VALUES(?,?,nullif(?,?),nullif(?,?))
		 ON CONFLICT (id) DO NOTHING`,
		id, description, parentID, "", parentType, "")
	return err
}

// GoalGet returns a goal by id. Returns (nil, nil) if not found.
func (s *Store) GoalGet(id string) (*Goal, error) {
	row := s.db.QueryRow(
		`SELECT id, description, status, subtree, parent_id, parent_type, created_at, updated_at
		 FROM goals WHERE id = ?`, id)
	g, err := scanGoal(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// GoalList returns all goals ordered by creation time.
func (s *Store) GoalList() ([]Goal, error) {
	rows, err := s.db.Query(
		`SELECT id, description, status, subtree, parent_id, parent_type, created_at, updated_at
		 FROM goals ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var goals []Goal
	for rows.Next() {
		g, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		goals = append(goals, g)
	}
	return goals, rows.Err()
}

// GoalUpdateStatus updates the status of a goal.
func (s *Store) GoalUpdateStatus(id, status string) error {
	_, err := s.db.Exec(
		`UPDATE goals SET status=?, updated_at=? WHERE id=?`, status, nowText(), id)
	return err
}

// GoalUpdateDescription updates the description of a goal. Used by standing-agent
// reconciliation to keep a config-owned goal's definition in sync with nine.toml
// (docs/predefined-agents.md §4).
func (s *Store) GoalUpdateDescription(id, description string) error {
	_, err := s.db.Exec(
		`UPDATE goals SET description=?, updated_at=? WHERE id=?`, description, nowText(), id)
	return err
}

// GoalAppendSubtree appends an entry string to the goal's subtree JSON array.
func (s *Store) GoalAppendSubtree(id, entry string) error {
	var subtreeJSON string
	err := s.db.QueryRow(`SELECT subtree FROM goals WHERE id = ?`, id).Scan(&subtreeJSON)
	if err == sql.ErrNoRows {
		return fmt.Errorf("goal not found: %s", id)
	}
	if err != nil {
		return err
	}
	var entries []string
	if err := json.Unmarshal([]byte(subtreeJSON), &entries); err != nil {
		return fmt.Errorf("unmarshal subtree: %w", err)
	}
	entries = append(entries, entry)
	updated, _ := json.Marshal(entries)
	_, err = s.db.Exec(
		`UPDATE goals SET subtree=?, updated_at=? WHERE id=?`,
		string(updated), nowText(), id)
	return err
}
