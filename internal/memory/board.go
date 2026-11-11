package memory

import (
	"database/sql"
	"fmt"
	"strings"
)

// Board tasks (adr/orchestrator-board.md §2.1): the board is the one durable,
// shared work queue in the store. A task is posted by a session, claimed
// atomically by one worker, and finished with a result or a failure. Reads are
// open to every session; writes go through the daemon's role-gated handlers,
// each transition journaled as a task_* event.

// Board task statuses: queued → running → done / failed; cancelled from queued
// or running.
const (
	BoardQueued    = "queued"
	BoardRunning   = "running"
	BoardDone      = "done"
	BoardFailed    = "failed"
	BoardCancelled = "cancelled"
)

// BoardOpenStatuses are the statuses an alike-duplicate check treats as open.
var BoardOpenStatuses = []string{BoardQueued, BoardRunning}

// BoardTask is one row of board_tasks.
type BoardTask struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Spec       string `json:"spec,omitempty"`
	PostedBy   string `json:"posted_by"`
	ClaimedBy  string `json:"claimed_by,omitempty"`
	Status     string `json:"status"`
	Result     string `json:"result,omitempty"`
	// Source is how the task entered the board: "post" (board_post),
	// "run_agent" (a waited delegation), "detached" (run_agent wait:false).
	// Only non-waited sources deliver their result to the poster on finish.
	Source     string `json:"source"`
	Depth      int    `json:"depth"`
	CreatedAt  string `json:"created_at"`
	ClaimedAt  string `json:"claimed_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

// BoardTaskPatch is an operator or poster update (§2.6, §2.1): nil fields are
// left unchanged.
type BoardTaskPatch struct {
	Title   *string
	Spec    *string
	Status  *string
	Result  *string
	Claimed *string // clears the claim when set to ""
}

func validBoardStatus(s string) bool {
	switch s {
	case BoardQueued, BoardRunning, BoardDone, BoardFailed, BoardCancelled:
		return true
	}
	return false
}

func scanBoardTask(s interface{ Scan(...any) error }) (BoardTask, error) {
	var t BoardTask
	err := s.Scan(&t.ID, &t.Title, &t.Spec, &t.PostedBy, &t.ClaimedBy, &t.Status,
		&t.Result, &t.Source, &t.Depth, &t.CreatedAt, &t.ClaimedAt, &t.FinishedAt, &t.UpdatedAt)
	return t, err
}

const boardTaskCols = `id, title, spec, posted_by, claimed_by, status, result, source, depth,
	created_at, claimed_at, finished_at, updated_at`

// Board task sources.
const (
	BoardSourcePost     = "post"
	BoardSourceRunAgent = "run_agent"
	BoardSourceDetached = "detached"
)

// BoardTaskCreate appends a queued task. The title must be non-empty; depth is
// the poster's lineage depth, recorded so posting chains can end at max_depth.
func (s *Store) BoardTaskCreate(title, spec, postedBy, source string, depth int) (BoardTask, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return BoardTask{}, fmt.Errorf("board task: title is required")
	}
	id := "task_" + NewID()
	if source == "" {
		source = BoardSourcePost
	}
	_, err := s.db.Exec(
		`INSERT INTO board_tasks (id, title, spec, posted_by, status, source, depth)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, title, spec, postedBy, BoardQueued, source, depth)
	if err != nil {
		return BoardTask{}, err
	}
	t, ok, err := s.BoardTaskGet(id)
	if err != nil || !ok {
		return BoardTask{}, fmt.Errorf("board task: read back %s: %w", id, err)
	}
	return t, nil
}

// BoardTaskList returns tasks newest-first (by created_at, id), optionally
// filtered to one status ("" is all). limit <= 0 means no limit.
func (s *Store) BoardTaskList(status string, limit, offset int) ([]BoardTask, error) {
	q := `SELECT ` + boardTaskCols + ` FROM board_tasks`
	args := []any{}
	if status != "" {
		if !validBoardStatus(status) {
			return nil, fmt.Errorf("board task: unknown status %q", status)
		}
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY rowid DESC`
	if limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []BoardTask
	for rows.Next() {
		t, err := scanBoardTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// BoardTaskGet returns one task by id.
func (s *Store) BoardTaskGet(id string) (BoardTask, bool, error) {
	t, err := scanBoardTask(s.db.QueryRow(
		`SELECT `+boardTaskCols+` FROM board_tasks WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return BoardTask{}, false, nil
	}
	if err != nil {
		return BoardTask{}, false, err
	}
	return t, true, nil
}

// BoardTaskClaim atomically moves a queued task to running under claimant. It
// reports false when the task is missing or already taken — two workers cannot
// claim one task.
func (s *Store) BoardTaskClaim(id, claimant string) (BoardTask, bool, error) {
	res, err := s.db.Exec(
		`UPDATE board_tasks SET status = ?, claimed_by = ?, claimed_at = strftime('%Y-%m-%dT%H:%M:%f000Z','now'),
			updated_at = strftime('%Y-%m-%dT%H:%M:%f000Z','now')
		 WHERE id = ? AND status = ?`,
		BoardRunning, claimant, id, BoardQueued)
	if err != nil {
		return BoardTask{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return BoardTask{}, false, nil
	}
	t, ok, err := s.BoardTaskGet(id)
	if err != nil || !ok {
		return BoardTask{}, false, fmt.Errorf("board task: read back %s: %w", id, err)
	}
	return t, true, nil
}

// BoardTaskFinish sets a running task to a terminal state, only for its
// claimant: done with a result, or failed with a reason.
func (s *Store) BoardTaskFinish(id, claimant, status, result string) (BoardTask, bool, error) {
	if status != BoardDone && status != BoardFailed {
		return BoardTask{}, false, fmt.Errorf("board task: %q is not a terminal status", status)
	}
	res, err := s.db.Exec(
		`UPDATE board_tasks SET status = ?, result = ?, finished_at = strftime('%Y-%m-%dT%H:%M:%f000Z','now'),
			updated_at = strftime('%Y-%m-%dT%H:%M:%f000Z','now')
		 WHERE id = ? AND claimed_by = ? AND status = ?`,
		status, result, id, claimant, BoardRunning)
	if err != nil {
		return BoardTask{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return BoardTask{}, false, nil
	}
	t, ok, err := s.BoardTaskGet(id)
	if err != nil || !ok {
		return BoardTask{}, false, fmt.Errorf("board task: read back %s: %w", id, err)
	}
	return t, true, nil
}

// BoardTaskCancel moves a queued or running task to cancelled.
func (s *Store) BoardTaskCancel(id string) (BoardTask, bool, error) {
	res, err := s.db.Exec(
		`UPDATE board_tasks SET status = ?, finished_at = strftime('%Y-%m-%dT%H:%M:%f000Z','now'),
			updated_at = strftime('%Y-%m-%dT%H:%M:%f000Z','now')
		 WHERE id = ? AND status IN (?, ?)`,
		BoardCancelled, id, BoardQueued, BoardRunning)
	if err != nil {
		return BoardTask{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return BoardTask{}, false, nil
	}
	t, ok, err := s.BoardTaskGet(id)
	if err != nil || !ok {
		return BoardTask{}, false, fmt.Errorf("board task: read back %s: %w", id, err)
	}
	return t, true, nil
}

// BoardTaskUpdate applies a patch to a task (operator CRUD, §2.6). It reports
// false when the id is unknown.
func (s *Store) BoardTaskUpdate(id string, p BoardTaskPatch) (BoardTask, bool, error) {
	if p.Status != nil && !validBoardStatus(*p.Status) {
		return BoardTask{}, false, fmt.Errorf("board task: unknown status %q", *p.Status)
	}
	res, err := s.db.Exec(
		`UPDATE board_tasks SET
			title = COALESCE(?, title),
			spec = COALESCE(?, spec),
			status = COALESCE(?, status),
			result = COALESCE(?, result),
			claimed_by = CASE WHEN ? THEN '' ELSE claimed_by END,
			updated_at = strftime('%Y-%m-%dT%H:%M:%f000Z','now')
		 WHERE id = ?`,
		p.Title, p.Spec, p.Status, p.Result, p.Claimed != nil, id)
	if err != nil {
		return BoardTask{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return BoardTask{}, false, nil
	}
	t, ok, err := s.BoardTaskGet(id)
	if err != nil || !ok {
		return BoardTask{}, false, fmt.Errorf("board task: read back %s: %w", id, err)
	}
	return t, true, nil
}

// BoardTaskDelete removes a row outright (operator delete, §2.6).
func (s *Store) BoardTaskDelete(id string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM board_tasks WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// BoardTaskCountOpen counts a poster's queued+running tasks, for the
// [board] max_queued admission guard (§2.4).
func (s *Store) BoardTaskCountOpen(poster string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM board_tasks WHERE posted_by = ? AND status IN (?, ?)`,
		poster, BoardQueued, BoardRunning).Scan(&n)
	return n, err
}

// BoardTaskFindAlike returns an open task with the same title and spec from the
// same poster, for the duplicate admission guard (§2.4).
func (s *Store) BoardTaskFindAlike(poster, title, spec string) (BoardTask, bool, error) {
	t, err := scanBoardTask(s.db.QueryRow(
		`SELECT `+boardTaskCols+` FROM board_tasks
		 WHERE posted_by = ? AND title = ? AND spec = ? AND status IN (?, ?)
		 ORDER BY rowid DESC LIMIT 1`,
		poster, title, spec, BoardQueued, BoardRunning))
	if err == sql.ErrNoRows {
		return BoardTask{}, false, nil
	}
	if err != nil {
		return BoardTask{}, false, err
	}
	return t, true, nil
}
