package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// FileStore saves or replaces the file at path with content.
func (s *Store) FileStore(path, content string) error {
	_, err := s.db.Exec(
		`INSERT INTO files(path, content, size, stored_at) VALUES(?,?,?,CURRENT_TIMESTAMP)
		 ON CONFLICT(path) DO UPDATE SET content=excluded.content, size=excluded.size, stored_at=CURRENT_TIMESTAMP`,
		path, content, len(content))
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

// FileSearchText runs a full-text search and returns up to limit results.
func (s *Store) FileSearchText(query string, limit int) ([]FileSearchResult, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := s.db.Query(
		`SELECT path,
		        ts_headline('english', content, websearch_to_tsquery('english', ?),
		                    'StartSel=[, StopSel=], MaxFragments=1, MaxWords=10, MinWords=1') AS snippet
		 FROM files
		 WHERE search_tsv @@ websearch_to_tsquery('english', ?)
		 ORDER BY ts_rank(search_tsv, websearch_to_tsquery('english', ?)) DESC
		 LIMIT ?`,
		query, query, query, limit)
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

// FileSearchTextJSON returns FTS results as a JSON string (for LLM tool output).
func (s *Store) FileSearchTextJSON(query string, limit int) (string, error) {
	results, err := s.FileSearchText(query, limit)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(results)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
