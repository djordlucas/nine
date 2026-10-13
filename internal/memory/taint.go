package memory

// Taint marks data written from an untrusted source — by a turn a piped report
// or an event's content started, or one that read such data — so a later turn
// that reads it is held to the same restriction (internal/runtime/taint.go).

// Taint kinds.
const (
	TaintFile   = "file"
	TaintMemory = "memory"
)

// TaintSet marks key, of kind, as written from an untrusted source.
func (s *Store) TaintSet(kind, key, source string) error {
	_, err := s.db.Exec(`INSERT INTO tainted(kind, key, source, at) VALUES(?,?,?,?)
		ON CONFLICT(kind, key) DO UPDATE SET source=excluded.source, at=excluded.at`,
		kind, key, source, nowText())
	return err
}

// TaintClear unmarks key: it was rewritten whole by a clean turn, or deleted.
func (s *Store) TaintClear(kind, key string) error {
	_, err := s.db.Exec(`DELETE FROM tainted WHERE kind = ? AND key = ?`, kind, key)
	return err
}

// Tainted reports whether key, of kind, is marked.
func (s *Store) Tainted(kind, key string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tainted WHERE kind = ? AND key = ?`, kind, key).Scan(&n)
	return n > 0, err
}

// TaintedKeys lists the marked keys of kind.
func (s *Store) TaintedKeys(kind string) ([]string, error) {
	rows, err := s.db.Query(`SELECT key FROM tainted WHERE kind = ? ORDER BY key`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

const taintedSchema = `CREATE TABLE IF NOT EXISTS tainted (
	kind   TEXT NOT NULL,
	key    TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT '',
	at     TEXT NOT NULL,
	PRIMARY KEY (kind, key)
)`
