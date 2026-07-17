package memory

// Reflection is one row from the reflections table.
type Reflection struct {
	ID      string `json:"id"`
	RanAt   string `json:"ran_at"`
	Summary string `json:"summary"`
}

// ReflectionCreate inserts a new reflection.
func (s *Store) ReflectionCreate(id, summary string) error {
	_, err := s.db.Exec(`INSERT INTO reflections (id, summary) VALUES (?, ?)`, id, summary)
	return err
}

// ReflectionList returns all reflections ordered by time ascending.
func (s *Store) ReflectionList() ([]Reflection, error) {
	rows, err := s.db.Query(`SELECT id, ran_at, summary FROM reflections ORDER BY ran_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []Reflection
	for rows.Next() {
		var r Reflection
		if err := rows.Scan(&r.ID, &r.RanAt, &r.Summary); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
