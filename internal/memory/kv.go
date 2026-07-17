package memory

import (
	"database/sql"
	"strings"
)

// Get returns the value for key. Returns ("", false, nil) if not found.
func (s *Store) Get(key string) (string, bool, error) {
	var val string
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&val)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return val, true, nil
}

// Set inserts or updates the value for key.
func (s *Store) Set(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO kv(key, value, updated_at) VALUES(?,?,CURRENT_TIMESTAMP)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP`,
		key, value)
	return err
}

// Delete removes key from the store. No-op if not found.
func (s *Store) Delete(key string) error {
	_, err := s.db.Exec(`DELETE FROM kv WHERE key = ?`, key)
	return err
}

// List returns all keys with the given prefix, sorted. Pass "" for all keys.
func (s *Store) List(prefix string) ([]string, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if prefix == "" {
		rows, err = s.db.Query(`SELECT key FROM kv ORDER BY key`)
	} else {
		rows, err = s.db.Query(`SELECT key FROM kv WHERE key LIKE ? ORDER BY key`, prefix+"%")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// KVGetString is a convenience wrapper that returns the value or "" on miss/error.
func (s *Store) KVGetString(key string) string {
	val, _, _ := s.Get(key)
	return val
}

// KVListString returns all keys joined by newlines (for LLM tool output).
func (s *Store) KVListString(prefix string) (string, error) {
	keys, err := s.List(prefix)
	if err != nil {
		return "", err
	}
	return strings.Join(keys, "\n"), nil
}
