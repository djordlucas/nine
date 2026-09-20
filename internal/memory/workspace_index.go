package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The workspace index makes files searchable that Nine did not write: a bind
// mount that arrived full, a `git pull`, a file an operator dropped in. It is
// derived state — delete it and the next scan rebuilds it — which is why it may
// live outside the "single database gateway" reasoning that governs primary
// state (invariant I3): the workspace is already primary state on disk, and
// this is a cache over it.
//
// The FTS table is **contentless**. `files_fts` is external-content over
// `files`, which is right there because the store *is* the text's home. Doing
// that here would put a second copy of every indexed file inside nine.db,
// doubling the disk a repository costs. Postings only; snippets are read back
// from the file itself, so they also reflect the file as it is now rather than
// as it was indexed.
//
// Deleting postings without the original text needs contentless_delete=1
// (SQLite 3.43+); the bundled SQLite is 3.53.

// WorkspaceFile is one indexed path.
type WorkspaceFile struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	Indexed     bool   `json:"indexed"`
	Reason      string `json:"reason,omitempty"` // why it is not indexed
	FirstSeen   string `json:"first_seen"`
	LastChanged string `json:"last_changed"`
}

// WorkspaceHit is one search result.
type WorkspaceHit struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

// WorkspaceIndexUpsert records a file's metadata and, when content is non-nil,
// its searchable text. A nil content means "known but not indexed" — an
// oversized file, a binary, an unreadable one — which still lists, with the
// reason, because a file missing from a listing reads as a file that is not
// there.
func (s *Store) WorkspaceIndexUpsert(path string, size int64, mtimeMS int64, content *string, reason string) error {
	tx, err := s.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rolled back only if Commit did not run

	var rowid int64
	err = tx.QueryRow(`SELECT rowid FROM workspace_files WHERE path = ?`, path).Scan(&rowid)
	switch {
	case err == sql.ErrNoRows:
		res, ierr := tx.Exec(
			`INSERT INTO workspace_files(path, size, mtime_ms, indexed, reason, first_seen, last_changed)
			 VALUES(?,?,?,?,?,?,?)`,
			path, size, mtimeMS, content != nil, reason, nowText(), nowText())
		if ierr != nil {
			return ierr
		}
		rowid, err = res.LastInsertId()
		if err != nil {
			return err
		}
	case err != nil:
		return err
	default:
		if _, err := tx.Exec(
			`UPDATE workspace_files SET size=?, mtime_ms=?, indexed=?, reason=?, last_changed=? WHERE rowid=?`,
			size, mtimeMS, content != nil, reason, nowText(), rowid); err != nil {
			return err
		}
		// Retract the old postings before writing new ones; a contentless table
		// cannot reconstruct them from the text it never kept.
		if _, err := tx.Exec(`DELETE FROM workspace_fts WHERE rowid = ?`, rowid); err != nil {
			return err
		}
	}

	if content != nil {
		if _, err := tx.Exec(`INSERT INTO workspace_fts(rowid, content) VALUES(?,?)`, rowid, *content); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// WorkspaceIndexDelete drops a path that no longer exists on disk.
func (s *Store) WorkspaceIndexDelete(path string) error {
	tx, err := s.db.BeginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var rowid int64
	switch err := tx.QueryRow(`SELECT rowid FROM workspace_files WHERE path = ?`, path).Scan(&rowid); {
	case err == sql.ErrNoRows:
		return nil
	case err != nil:
		return err
	}
	if _, err := tx.Exec(`DELETE FROM workspace_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM workspace_files WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	return tx.Commit()
}

// WorkspaceIndexState returns the size and mtime recorded for a path, so a scan
// can skip a file that has not changed. found=false means the path is new.
func (s *Store) WorkspaceIndexState(path string) (size int64, mtimeMS int64, found bool, err error) {
	err = s.db.QueryRow(`SELECT size, mtime_ms FROM workspace_files WHERE path = ?`, path).Scan(&size, &mtimeMS)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return size, mtimeMS, true, nil
}

// WorkspaceIndexPaths lists every indexed path under a prefix, for a scan to
// find the rows whose files have disappeared.
func (s *Store) WorkspaceIndexPaths(prefix string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT path FROM workspace_files WHERE ? = '' OR path LIKE ? || '%' ORDER BY path`, prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// WorkspaceList lists indexed files, optionally under a path prefix, matching a
// glob, or changed since a timestamp.
//
// The glob is matched in SQL with LIKE after translating `*` and `?`, so a
// pattern never pulls the whole index into memory to filter it.
func (s *Store) WorkspaceList(prefix, pattern, changedSince string, limit int) ([]WorkspaceFile, error) {
	if limit <= 0 {
		limit = 200
	}
	q := strings.Builder{}
	q.WriteString(`SELECT path, size, indexed, reason, first_seen, last_changed FROM workspace_files WHERE 1=1`)
	args := []any{}
	if prefix != "" {
		q.WriteString(` AND path LIKE ? || '%'`)
		args = append(args, prefix)
	}
	if pattern != "" {
		// ESCAPE makes a literal % or _ in a filename safe to match.
		q.WriteString(` AND path LIKE ? ESCAPE '\'`)
		args = append(args, globToLike(pattern))
	}
	if changedSince != "" {
		q.WriteString(` AND last_changed >= ?`)
		args = append(args, changedSince)
	}
	q.WriteString(` ORDER BY path LIMIT ?`)
	args = append(args, limit)

	rows, err := s.db.Query(q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []WorkspaceFile
	for rows.Next() {
		var f WorkspaceFile
		if err := rows.Scan(&f.Path, &f.Size, &f.Indexed, &f.Reason, &f.FirstSeen, &f.LastChanged); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// globToLike translates a shell-style glob into a LIKE pattern. `*` matches any
// run of characters including `/`, so `*_test.go` finds a test file at any
// depth — which is what someone asking for "the test files" means.
func globToLike(pattern string) string {
	var b strings.Builder
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		case '%', '_', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// WorkspaceSearch runs a full-text search over the indexed workspace, ranked by
// bm25 and confined to pathPrefix when one is given.
//
// Snippets are not produced by FTS5 here: a contentless table has no text to
// snippet from. The caller reads the region out of the file, which is also what
// keeps a snippet honest when the file changed after it was indexed.
func (s *Store) WorkspaceSearch(query, pathPrefix string, limit int) ([]WorkspaceHit, error) {
	if limit <= 0 {
		limit = 10
	}
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT w.path
		 FROM workspace_fts
		 JOIN workspace_files w ON w.rowid = workspace_fts.rowid
		 WHERE workspace_fts MATCH ?
		   AND (? = '' OR w.path LIKE ? || '%')
		 ORDER BY bm25(workspace_fts)
		 LIMIT ?`,
		match, pathPrefix, pathPrefix, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var out []WorkspaceHit
	for rows.Next() {
		var h WorkspaceHit
		if err := rows.Scan(&h.Path); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// WorkspaceSkipped counts files under a prefix that are known but not indexed,
// so a search can say how much it could not look at. A search that reports
// "no matches" when it never read half the tree teaches the agent the text is
// not there.
func (s *Store) WorkspaceSkipped(prefix string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM workspace_files WHERE indexed = 0 AND (? = '' OR path LIKE ? || '%')`,
		prefix, prefix).Scan(&n)
	return n, err
}

// WorkspaceIndexedCount reports how many files carry postings, for the scan's
// own logging and for `nine` diagnostics.
func (s *Store) WorkspaceIndexedCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM workspace_files WHERE indexed = 1`).Scan(&n)
	return n, err
}

// WorkspaceListJSON is WorkspaceList ready to hand to a tool boundary.
func (s *Store) WorkspaceListJSON(prefix, pattern, changedSince string, limit int) (string, error) {
	files, err := s.WorkspaceList(prefix, pattern, changedSince, limit)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return `{"files":[]}`, nil
	}
	b, err := json.Marshal(map[string]any{"files": files})
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// WorkspaceScanTime records when a full scan last completed, so a search issued
// before the first one can say so rather than reporting an empty index as an
// empty workspace.
func (s *Store) WorkspaceScanTime() (time.Time, bool, error) {
	v, found, err := s.Get(workspaceScanKey)
	if err != nil || !found {
		return time.Time{}, false, err
	}
	t, err := time.Parse(timeLayout, v)
	if err != nil {
		return time.Time{}, false, nil
	}
	return t, true, nil
}

// SetWorkspaceScanTime stamps the completion of a full scan.
func (s *Store) SetWorkspaceScanTime(t time.Time) error {
	return s.Set(workspaceScanKey, writeTime(t))
}

// workspaceScanKey is daemon-private bookkeeping in the KV table. It shares the
// `self/` protection prefix so memory_delete cannot remove it.
const workspaceScanKey = "self/workspace/last_scan"

// WorkspaceIndexReset drops the whole index. Used when the workspace root
// changes underneath the daemon, where every recorded path describes a
// directory that is no longer the workspace.
func (s *Store) WorkspaceIndexReset() error {
	if _, err := s.db.Exec(`DELETE FROM workspace_fts`); err != nil {
		return fmt.Errorf("clear workspace fts: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM workspace_files`); err != nil {
		return fmt.Errorf("clear workspace index: %w", err)
	}
	return nil
}
