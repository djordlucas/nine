package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Stored agent definitions (adr/agents-concept.md A5, phase 1): an agent is a
// composable bundle — name, role, skills, self-model, reflection — persisted in
// the daemon's store so the API and CLI can compose and run it. Per-agent
// scoping of the bundle's parts lands in phase 2 (A3); here the definition is
// one row plus a version history (Q14: every change writes a version row).

// Agent is one row of the agents table. Skills, SelfModel and Reflection are
// JSON text, '' when unset.
type Agent struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	Skills     string `json:"skills"`
	SelfModel  string `json:"self_model"`
	Reflection string `json:"reflection"`
	Version    int    `json:"version"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// AgentPatch is an update to an agent definition; nil fields are left unchanged.
type AgentPatch struct {
	Name       *string
	Role       *string
	Skills     *string // JSON array text; set to "[]" to clear
	SelfModel  *string // JSON text; set to "" to clear
	Reflection *string // JSON text; set to "" to clear
}

// AgentVersion is one row of agent_versions: the agent's full definition as of
// that version.
type AgentVersion struct {
	AgentID    string `json:"agent_id"`
	Version    int    `json:"version"`
	Definition string `json:"definition"`
	CreatedAt  string `json:"created_at"`
}

func scanAgent(s interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	err := s.Scan(&a.ID, &a.Name, &a.Role, &a.Skills, &a.SelfModel, &a.Reflection,
		&a.Version, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

const agentCols = `id, name, role, skills, self_model, reflection, version, created_at, updated_at`

// validSkillsJSON reports whether s is a JSON array of strings ('' is valid).
func validSkillsJSON(s string) bool {
	if s == "" {
		return true
	}
	var skills []string
	return json.Unmarshal([]byte(s), &skills) == nil
}

// AgentCreate inserts a new agent at version 1 and records that version.
func (s *Store) AgentCreate(name, role, skills, selfModel, reflection string) (Agent, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Agent{}, fmt.Errorf("agent: name is required")
	}
	if !validSkillsJSON(skills) {
		return Agent{}, fmt.Errorf("agent: skills must be a JSON array of strings")
	}
	id := "agent_" + NewID()
	if _, err := s.db.Exec(
		`INSERT INTO agents (id, name, role, skills, self_model, reflection, version)
		 VALUES (?, ?, ?, ?, ?, ?, 1)`,
		id, name, role, skills, selfModel, reflection); err != nil {
		return Agent{}, err
	}
	a, ok, err := s.AgentGet(id)
	if err != nil || !ok {
		return Agent{}, fmt.Errorf("agent: read back %s: %w", id, err)
	}
	if err := s.agentRecordVersion(a); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// AgentGet returns one agent by id.
func (s *Store) AgentGet(id string) (Agent, bool, error) {
	row := s.db.QueryRow(`SELECT `+agentCols+` FROM agents WHERE id = ?`, id)
	a, err := scanAgent(row)
	if err == sql.ErrNoRows {
		return Agent{}, false, nil
	}
	return a, err == nil, err
}

// AgentList returns every agent, oldest first.
func (s *Store) AgentList() ([]Agent, error) {
	rows, err := s.db.Query(`SELECT ` + agentCols + ` FROM agents ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AgentUpdate applies a patch, bumps the version and records the new version.
func (s *Store) AgentUpdate(id string, p AgentPatch) (Agent, bool, error) {
	sets := []string{"updated_at = ?"}
	args := []any{nowText()}
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" {
			return Agent{}, false, fmt.Errorf("agent: name is required")
		}
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if p.Role != nil {
		sets = append(sets, "role = ?")
		args = append(args, *p.Role)
	}
	if p.Skills != nil {
		if !validSkillsJSON(*p.Skills) {
			return Agent{}, false, fmt.Errorf("agent: skills must be a JSON array of strings")
		}
		sets = append(sets, "skills = ?")
		args = append(args, *p.Skills)
	}
	if p.SelfModel != nil {
		sets = append(sets, "self_model = ?")
		args = append(args, *p.SelfModel)
	}
	if p.Reflection != nil {
		sets = append(sets, "reflection = ?")
		args = append(args, *p.Reflection)
	}
	args = append(args, id)
	res, err := s.db.Exec(
		`UPDATE agents SET `+strings.Join(sets, ", ")+`, version = version + 1 WHERE id = ?`, args...)
	if err != nil {
		return Agent{}, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Agent{}, false, nil
	}
	a, ok, err := s.AgentGet(id)
	if err != nil || !ok {
		return Agent{}, false, fmt.Errorf("agent: read back %s: %w", id, err)
	}
	if err := s.agentRecordVersion(a); err != nil {
		return Agent{}, false, err
	}
	return a, true, nil
}

// AgentDelete removes an agent and its version history.
func (s *Store) AgentDelete(id string) (bool, error) {
	if _, err := s.db.Exec(`DELETE FROM agent_versions WHERE agent_id = ?`, id); err != nil {
		return false, err
	}
	res, err := s.db.Exec(`DELETE FROM agents WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// AgentVersions returns an agent's version history, oldest first.
func (s *Store) AgentVersions(id string) ([]AgentVersion, error) {
	rows, err := s.db.Query(
		`SELECT agent_id, version, definition, created_at FROM agent_versions WHERE agent_id = ? ORDER BY version`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentVersion
	for rows.Next() {
		var v AgentVersion
		if err := rows.Scan(&v.AgentID, &v.Version, &v.Definition, &v.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// agentRecordVersion appends the agent's current row to agent_versions.
func (s *Store) agentRecordVersion(a Agent) error {
	blob, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO agent_versions (agent_id, version, definition) VALUES (?, ?, ?)`,
		a.ID, a.Version, string(blob))
	return err
}
