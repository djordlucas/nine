package memory

import (
	"database/sql"
	"fmt"
)

// Goal is one row from the goals table.
type Goal struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	ParentID    string `json:"parent_id,omitempty"`
	ParentType  string `json:"parent_type,omitempty"`

	// Subtree is the goal's child goal ids, derived from their parent_id at read
	// time. It is not stored: the edge lives on the child, and a second copy on
	// the parent was a denormalized index the model was asked to maintain by
	// hand — wrong at some rate, and unverifiable
	// (adr/concept-consolidation.md C6). The JSON key is unchanged so the
	// model-facing shape of goal_get is what it always was.
	Subtree   []string `json:"subtree"`
	CreatedAt string   `json:"created_at"`
	UpdatedAt string   `json:"updated_at"`
}

func scanGoal(row interface{ Scan(...any) error }) (Goal, error) {
	var g Goal
	var parentID, parentType sql.NullString
	err := row.Scan(&g.ID, &g.Description, &g.Status,
		&parentID, &parentType, &g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return Goal{}, err
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
		`SELECT id, description, status, parent_id, parent_type, created_at, updated_at
		 FROM goals WHERE id = ?`, id)
	g, err := scanGoal(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Derived, not stored — see Goal.Subtree. Only GoalGet fills it: it is the
	// read a model uses to inspect one goal, and doing it per row in GoalList
	// would be a query per goal for a field that listing does not show.
	if g.Subtree, err = s.GoalListChildren(id); err != nil {
		return nil, fmt.Errorf("list children of %s: %w", id, err)
	}
	return &g, nil
}

// GoalList returns all goals ordered by creation time.
func (s *Store) GoalList() ([]Goal, error) {
	rows, err := s.db.Query(
		`SELECT id, description, status, parent_id, parent_type, created_at, updated_at
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
// (adr/predefined-agents-design.md §4).
func (s *Store) GoalUpdateDescription(id, description string) error {
	_, err := s.db.Exec(
		`UPDATE goals SET description=?, updated_at=? WHERE id=?`, description, nowText(), id)
	return err
}

// GoalListChildren returns the ids of goals whose parent is id, oldest first.
//
// This replaces the stored `subtree` column: parent_id is the authoritative
// edge, written by goal_create, so the children can simply be asked for. The
// column it replaces was a free-text copy of the same relation that nothing read
// and the model was told to keep up to date by hand.
func (s *Store) GoalListChildren(id string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT id FROM goals WHERE parent_id = ? ORDER BY created_at ASC, id ASC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var childID string
		if err := rows.Scan(&childID); err != nil {
			return nil, err
		}
		out = append(out, childID)
	}
	return out, rows.Err()
}

// GoalDelete removes a goal and every goal beneath it, returning the ids
// removed, the goal itself first. A missing id removes nothing and returns an
// empty list rather than an error, so the caller decides whether that is a 404.
//
// Sub-goals go with their parent because nothing else would reach them: a
// sub-goal has no session of its own and is worked on by the session pursuing
// its top-level ancestor (docs/goal-sessions.md), so once that ancestor is gone
// the subtree is orphaned rows. The walk and the delete share one transaction,
// so a goal created under the subtree mid-delete is either seen or not parented
// to anything deleted.
func (s *Store) GoalDelete(id string) ([]string, error) {
	tx, err := s.db.BeginWrite()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.Query(
		`WITH RECURSIVE tree(id, depth) AS (
		   SELECT id, 0 FROM goals WHERE id = ?
		   UNION
		   SELECT g.id, t.depth + 1 FROM goals g JOIN tree t ON g.parent_id = t.id
		 )
		 SELECT id FROM tree ORDER BY depth, id`, id)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var gid string
		if err := rows.Scan(&gid); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		ids = append(ids, gid)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, gid := range ids {
		if _, err := tx.Exec(`DELETE FROM goals WHERE id = ?`, gid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}
