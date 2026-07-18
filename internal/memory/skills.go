package memory

import (
	"database/sql"
	"encoding/json"
)

// Skill source values. Built-in skills are seeded from the binary and user
// skills from the operator's skills directory; both are immutable at runtime,
// their files being the source of truth. Agent skills are authored by Nine
// itself and may be updated via skill_write/skill_modify.
const (
	SkillSourceBuiltin = "builtin"
	SkillSourceUser    = "user"
	SkillSourceAgent   = "agent"
)

// SkillSourceImmutable reports whether skills from this source are read-only
// to the agent. Both file-backed sources are: a runtime write would be
// silently undone by the next boot's reseed, and for user skills it would also
// let Nine edit the operator's intent (spec/contracts/skills.md R-SKILL.2).
func SkillSourceImmutable(source string) bool {
	return source == SkillSourceBuiltin || source == SkillSourceUser
}

// Skill is one row of the skills table.
type Skill struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Content     string   `json:"content,omitempty"`
	Source      string   `json:"source"`
}

// SkillUpsert inserts or replaces a skill by name.
func (s *Store) SkillUpsert(sk Skill) error {
	tags, err := json.Marshal(sk.Tags)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO skills(name, description, tags, content, source, updated_at)
		 VALUES(?,?,?,?,?,CURRENT_TIMESTAMP)
		 ON CONFLICT(name) DO UPDATE SET
		   description=excluded.description, tags=excluded.tags,
		   content=excluded.content, source=excluded.source,
		   updated_at=CURRENT_TIMESTAMP`,
		sk.Name, sk.Description, string(tags), sk.Content, sk.Source)
	return err
}

// SkillGet returns (skill, true, nil) if found, or (zero, false, nil) if not.
func (s *Store) SkillGet(name string) (Skill, bool, error) {
	var (
		sk   Skill
		tags string
	)
	err := s.db.QueryRow(
		`SELECT name, description, tags, content, source FROM skills WHERE name = ?`, name).
		Scan(&sk.Name, &sk.Description, &tags, &sk.Content, &sk.Source)
	if err == sql.ErrNoRows {
		return Skill{}, false, nil
	}
	if err != nil {
		return Skill{}, false, err
	}
	_ = json.Unmarshal([]byte(tags), &sk.Tags)
	return sk, true, nil
}

// SkillList returns all skills (without content), sorted by name.
func (s *Store) SkillList() ([]Skill, error) {
	rows, err := s.db.Query(
		`SELECT name, description, tags, source FROM skills ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	skills := []Skill{}
	for rows.Next() {
		var (
			sk   Skill
			tags string
		)
		if err := rows.Scan(&sk.Name, &sk.Description, &tags, &sk.Source); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &sk.Tags)
		skills = append(skills, sk)
	}
	return skills, rows.Err()
}

// SkillListJSON returns SkillList as a JSON array string (for LLM tool output).
func (s *Store) SkillListJSON() (string, error) {
	skills, err := s.SkillList()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(skills)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// SkillsBySource returns all skills with the given source, content included,
// sorted by name. Use SkillNamesBySource when only the names are needed — this
// one carries every body and is meant for callers that must parse frontmatter
// (the role registry).
func (s *Store) SkillsBySource(source string) ([]Skill, error) {
	rows, err := s.db.Query(
		`SELECT name, description, tags, content, source FROM skills WHERE source = ? ORDER BY name`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	skills := []Skill{}
	for rows.Next() {
		var (
			sk   Skill
			tags string
		)
		if err := rows.Scan(&sk.Name, &sk.Description, &tags, &sk.Content, &sk.Source); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(tags), &sk.Tags)
		skills = append(skills, sk)
	}
	return skills, rows.Err()
}

// SkillNamesBySource returns the names of all skills with the given source.
func (s *Store) SkillNamesBySource(source string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM skills WHERE source = ? ORDER BY name`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// SkillDelete removes a skill by name.
func (s *Store) SkillDelete(name string) error {
	_, err := s.db.Exec(`DELETE FROM skills WHERE name = ?`, name)
	return err
}
