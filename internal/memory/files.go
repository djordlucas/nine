package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// FileStore saves or replaces the file at path with content.
//
// NUL bytes are replaced with U+FFFD. SQLite's length() and substr() treat a
// NUL as the end of a TEXT value, so storing one verbatim would silently
// truncate every windowed read past it — FileFetchRange would report a Total
// shorter than the file and a paging loop would terminate early, with no error
// anywhere.
func (s *Store) FileStore(path, content string) error {
	content = strings.ReplaceAll(content, "\x00", "�")
	_, err := s.db.Exec(
		`INSERT INTO files(path, content, size, stored_at) VALUES(?,?,?,?)
		 ON CONFLICT(path) DO UPDATE SET content=excluded.content, size=excluded.size, stored_at=excluded.stored_at`,
		path, content, len(content), nowText())
	return err
}

// FileFetch returns (content, true, nil) if found, or ("", false, nil) if not.
func (s *Store) FileFetch(path string) (string, bool, error) {
	var content string
	err := s.db.QueryRow(`SELECT content FROM files WHERE path = ?`, path).Scan(&content)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return content, true, nil
}

// FileSlice is a windowed read of a stored file (adr/tool-output-spill.md).
type FileSlice struct {
	Content string `json:"content"`
	Offset  int    `json:"offset"` // character offset the window starts at
	Chars   int    `json:"chars"`  // characters actually returned
	Total   int    `json:"total"`  // total characters in the file
}

// FileFetchRange returns a window of the file at path: limit characters
// starting at character offset. It is how an agent reads back a large stored
// file — a spilled tool output, typically — without pulling the whole thing
// into context.
//
// Offsets and lengths are in **characters**, not bytes, so a window can never
// split a multi-byte rune, and the slicing happens in the database so a huge
// file is never materialized in the daemon. A negative offset clamps to 0; a
// non-positive limit means "to the end of the file". An offset past the end
// returns an empty window rather than an error, so a paging loop terminates
// cleanly. Returns found=false if the path does not exist.
func (s *Store) FileFetchRange(path string, offset, limit int) (FileSlice, bool, error) {
	if offset < 0 {
		offset = 0
	}
	var (
		content string
		total   int
	)
	// substr(text, start, count) is 1-based and character-oriented; length()
	// on text likewise counts characters.
	query := `SELECT substr(content, ?, ?), length(content) FROM files WHERE path = ?`
	args := []any{offset + 1, limit, path}
	if limit <= 0 {
		query = `SELECT substr(content, ?), length(content) FROM files WHERE path = ?`
		args = []any{offset + 1, path}
	}
	err := s.db.QueryRow(query, args...).Scan(&content, &total)
	if err == sql.ErrNoRows {
		return FileSlice{}, false, nil
	}
	if err != nil {
		return FileSlice{}, false, err
	}
	return FileSlice{
		Content: content,
		Offset:  offset,
		Chars:   utf8.RuneCountInString(content),
		Total:   total,
	}, true, nil
}

// FileDeleteOlderThan removes files under pathPrefix last stored more than
// olderThan ago, returning how many rows it deleted. It is the retention sweep
// for spilled tool output, which is per-session debris: without it the files
// table grows without bound (adr/tool-output-spill.md §5). An empty prefix is
// rejected — this must never be able to clear the whole store.
func (s *Store) FileDeleteOlderThan(pathPrefix string, olderThan time.Duration) (int64, error) {
	if pathPrefix == "" {
		return 0, fmt.Errorf("file delete: a path prefix is required")
	}
	if olderThan <= 0 {
		return 0, fmt.Errorf("file delete: age must be positive, got %s", olderThan)
	}
	res, err := s.db.Exec(
		`DELETE FROM files WHERE path LIKE ? AND stored_at < ?`,
		pathPrefix+"%", writeTime(time.Now().Add(-olderThan)))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// FileList returns paths with the given prefix, sorted. Pass "" for all.
func (s *Store) FileList(prefix string) ([]string, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if prefix == "" {
		rows, err = s.db.Query(`SELECT path FROM files ORDER BY path`)
	} else {
		rows, err = s.db.Query(`SELECT path FROM files WHERE path LIKE ? ORDER BY path`, prefix+"%")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, rows.Err()
}

// FileSearchResult is one FTS5 hit.
type FileSearchResult struct {
	Path    string `json:"path"`
	Snippet string `json:"snippet"`
}

// FileSearchText runs a full-text search over every stored file and returns up
// to limit results.
func (s *Store) FileSearchText(query string, limit int) ([]FileSearchResult, error) {
	return s.FileSearchTextScoped(query, "", limit)
}

// FileSearchTextScoped is FileSearchText confined to paths under pathPrefix.
// Passing an exact path narrows the search to a single file, which is how an
// agent locates the relevant region of one large spilled output rather than
// searching the whole store (adr/tool-output-spill.md §3). An empty prefix
// searches everything.
func (s *Store) FileSearchTextScoped(query, pathPrefix string, limit int) ([]FileSearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	// ftsQuery guarantees a syntactically valid MATCH expression, or "" when the
	// input has nothing searchable in it — an empty MATCH is itself an error, so
	// that case short-circuits to no results.
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	// The path filter is an optional conjunct rather than a second query so the
	// ranking and snippet extraction stay identical either way.
	//
	// bm25() returns the negation of the usual score, so best-first is plain
	// ascending order — a DESC here would rank the worst matches first.
	rows, err := s.db.Query(
		`SELECT f.path, snippet(files_fts, 0, '[', ']', '…', 10) AS snippet
		 FROM files_fts
		 JOIN files f ON f.rowid = files_fts.rowid
		 WHERE files_fts MATCH ?
		   AND (? = '' OR f.path LIKE ? || '%')
		 ORDER BY bm25(files_fts)
		 LIMIT ?`,
		match, pathPrefix, pathPrefix, limit)
	if err != nil {
		return nil, fmt.Errorf("fts query: %w", err)
	}
	defer rows.Close()

	var results []FileSearchResult
	for rows.Next() {
		var r FileSearchResult
		if err := rows.Scan(&r.Path, &r.Snippet); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// FileListString returns paths joined by newlines (for LLM tool output).
func (s *Store) FileListString(prefix string) (string, error) {
	paths, err := s.FileList(prefix)
	if err != nil {
		return "", err
	}
	return strings.Join(paths, "\n"), nil
}

// FileSearchTextJSON returns FTS results as a JSON string (for LLM tool
// output), optionally confined to paths under pathPrefix.
func (s *Store) FileSearchTextJSON(query, pathPrefix string, limit int) (string, error) {
	results, err := s.FileSearchTextScoped(query, pathPrefix, limit)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
