package memory

import (
	"database/sql"
	"encoding/json"
)

// A generated tool is store state, exactly like a goal, a workflow, or an agent
// skill: listable, readable, exportable, deletable, and journalled
// (docs/sandboxed-tools.md §2). It does not drift the binary — the property that
// docs/ and the binary match its version is untouched — which is precisely why
// runtime tool *generation* is permitted where runtime plugin generation is not.
//
// The row holds the code and the tool's *declaration*. It deliberately does not
// hold a grant: capabilities come from `[tools.agent.capabilities]` in nine.toml
// at load time, written by the operator. The agent writes this table; it never
// writes the grant. Those are never the same actor.

// GeneratedTool is one row of the tools table.
type GeneratedTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Source      string          `json:"source,omitempty"`
	// Capabilities is the tool's declaration, stored as the JSON the agent
	// supplied. Checked against the operator's ceiling at load, and re-checked
	// on every reload — so narrowing the ceiling retroactively disables a tool
	// that no longer fits under it rather than leaving it running.
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	CreatedAt    string          `json:"created_at,omitempty"`
	UpdatedAt    string          `json:"updated_at,omitempty"`
	// LastCalledAt drives LRU eviction (docs/sandboxed-tools.md §9.2). Empty
	// until the tool is first called.
	LastCalledAt string `json:"last_called_at,omitempty"`
	CallCount    int    `json:"call_count"`
}

// GeneratedToolUpsert inserts or replaces a tool by name.
//
// Replacing rather than duplicating is what keeps the catalog from filling with
// near-identical variants as the agent iterates (§9.2). The usage counters are
// deliberately preserved across a rewrite: a tool the agent improved is the same
// tool as far as eviction is concerned, and resetting its history would make the
// most actively maintained tools the most likely to be evicted.
func (s *Store) GeneratedToolUpsert(t GeneratedTool) error {
	schema := string(t.InputSchema)
	if schema == "" {
		schema = `{"type":"object","properties":{}}`
	}
	caps := string(t.Capabilities)
	if caps == "" {
		caps = "{}"
	}
	_, err := s.db.Exec(
		`INSERT INTO tools(name, description, input_schema, source, capabilities, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(name) DO UPDATE SET
		   description=excluded.description, input_schema=excluded.input_schema,
		   source=excluded.source, capabilities=excluded.capabilities,
		   updated_at=excluded.updated_at`,
		t.Name, t.Description, schema, t.Source, caps, nowText(), nowText())
	return err
}

// GeneratedToolGet returns (tool, true, nil) if found, or (zero, false, nil).
func (s *Store) GeneratedToolGet(name string) (GeneratedTool, bool, error) {
	var (
		t      GeneratedTool
		schema string
		caps   string
		last   sql.NullString
	)
	err := s.db.QueryRow(
		`SELECT name, description, input_schema, source, capabilities,
		        created_at, updated_at, last_called_at, call_count
		   FROM tools WHERE name = ?`, name).
		Scan(&t.Name, &t.Description, &schema, &t.Source, &caps,
			&t.CreatedAt, &t.UpdatedAt, &last, &t.CallCount)
	if err == sql.ErrNoRows {
		return GeneratedTool{}, false, nil
	}
	if err != nil {
		return GeneratedTool{}, false, err
	}
	t.InputSchema = json.RawMessage(schema)
	t.Capabilities = json.RawMessage(caps)
	t.LastCalledAt = last.String
	return t, true, nil
}

// GeneratedToolList returns every generated tool, source included, oldest use
// first — the order eviction walks.
func (s *Store) GeneratedToolList() ([]GeneratedTool, error) {
	rows, err := s.db.Query(
		`SELECT name, description, input_schema, source, capabilities,
		        created_at, updated_at, last_called_at, call_count
		   FROM tools
		  ORDER BY COALESCE(NULLIF(last_called_at, ''), created_at) ASC, name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	out := []GeneratedTool{}
	for rows.Next() {
		var (
			t      GeneratedTool
			schema string
			caps   string
			last   sql.NullString
		)
		if err := rows.Scan(&t.Name, &t.Description, &schema, &t.Source, &caps,
			&t.CreatedAt, &t.UpdatedAt, &last, &t.CallCount); err != nil {
			return nil, err
		}
		t.InputSchema = json.RawMessage(schema)
		t.Capabilities = json.RawMessage(caps)
		t.LastCalledAt = last.String
		out = append(out, t)
	}
	return out, rows.Err()
}

// GeneratedToolTouch records a call, for LRU eviction and for `nine tools show`.
// Best-effort by design: a bookkeeping failure must not fail the tool call the
// model is waiting on.
func (s *Store) GeneratedToolTouch(name string) error {
	_, err := s.db.Exec(
		`UPDATE tools SET last_called_at = ?, call_count = call_count + 1 WHERE name = ?`,
		nowText(), name)
	return err
}

// GeneratedToolDelete removes a tool by name.
func (s *Store) GeneratedToolDelete(name string) error {
	_, err := s.db.Exec(`DELETE FROM tools WHERE name = ?`, name)
	return err
}

// GeneratedToolCount returns how many generated tools exist.
func (s *Store) GeneratedToolCount() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tools`).Scan(&n)
	return n, err
}

// GeneratedToolEvictOldest deletes tools beyond max, least-recently-called
// first, and returns the names it removed.
//
// The cap exists because an agent that can write tools will write tools, and
// every one competes for the context budget in selectTools. A catalog of 200
// half-redundant generated tools degrades the ranking for the *built-in* tools
// too — the agent poisons its own tool selection and gets worse at everything
// (docs/sandboxed-tools.md §9.2). Eviction is on last-called-at, so a tool that
// earns its place keeps it.
func (s *Store) GeneratedToolEvictOldest(max int) ([]string, error) {
	if max <= 0 {
		return nil, nil
	}
	rows, err := s.db.Query(
		`SELECT name FROM tools
		  ORDER BY COALESCE(NULLIF(last_called_at, ''), created_at) ASC, name ASC
		  LIMIT MAX(0, (SELECT COUNT(*) FROM tools) - ?)`, max)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var evict []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		evict = append(evict, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, n := range evict {
		if err := s.GeneratedToolDelete(n); err != nil {
			return nil, err
		}
	}
	return evict, nil
}
