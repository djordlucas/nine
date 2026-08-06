package memory

// RawColumn exposes a single-column read for tests that need to assert on what
// is physically stored rather than on what a typed accessor returns — most
// importantly the on-disk shape of timestamps, which no exported method reveals
// and which SQLite will silently mis-order if it ever drifts.
//
// query must be a SELECT of exactly one column.
func (s *Store) RawColumn(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ExecRaw runs a statement for tests that need to set up state no exported
// method can produce.
func (s *Store) ExecRaw(query string, args ...any) error {
	_, err := s.db.Exec(query, args...)
	return err
}

// FTSQuery exposes the MATCH-expression builder so its guarantee — that no
// input can produce a syntax error — is testable without a database.
var FTSQuery = ftsQuery
