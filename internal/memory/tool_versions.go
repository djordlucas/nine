package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Versions of the tools Nine writes (adr/process-sessions.md §9). Every write
// of an existing tool keeps the one it replaces, with the process definition it
// ran under, so a version can be restored by hand, and a process's new version
// that fails is rolled back to the previous one. A restore is itself a write:
// the history only moves forward.

// MaxToolVersions is how many previous versions a tool keeps.
const MaxToolVersions = 10

// ToolVersion is one previous version of a generated tool.
type ToolVersion struct {
	Name         string          `json:"name"`
	Version      int             `json:"version"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	Source       string          `json:"source,omitempty"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	Lockfile     json.RawMessage `json:"lockfile,omitempty"`
	Resumable    bool            `json:"resumable,omitempty"`
	Live         bool            `json:"live,omitempty"`
	// Process is the process definition the version ran under, as JSON, or
	// empty for a tool that was not a process.
	Process   string `json:"process,omitempty"`
	WrittenBy string `json:"written_by,omitempty"`
	WrittenAt string `json:"written_at"`
}

// GeneratedToolWrite writes t, keeping the version it replaces with prevProcess,
// that version's process definition. It returns t's version number. t's
// Probation and WrittenBy are stored with it.
func (s *Store) GeneratedToolWrite(t GeneratedTool, prevProcess string) (int, error) {
	schema, caps, lock := orObject(t.InputSchema, `{"type":"object","properties":{}}`), orObject(t.Capabilities, "{}"), orObject(t.Lockfile, "{}")
	tx, err := s.db.BeginWrite()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck // a no-op after Commit

	var version int
	err = tx.QueryRow(`SELECT version FROM tools WHERE name = ?`, t.Name).Scan(&version)
	switch {
	case err == sql.ErrNoRows:
		version = 1
		if _, err := tx.Exec(
			`INSERT INTO tools(name, description, input_schema, source, capabilities, lockfile,
			                   resumable, live, version, probation, written_by, created_at, updated_at)
			 VALUES(?,?,?,?,?,?,?,?,1,?,?,?,?)`,
			t.Name, t.Description, schema, t.Source, caps, lock, t.Resumable, t.Live,
			t.Probation, t.WrittenBy, nowText(), nowText()); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	default:
		if _, err := tx.Exec(
			`INSERT OR REPLACE INTO tool_versions(name, version, description, input_schema, source, capabilities,
			                                      lockfile, resumable, live, process, written_by, written_at)
			 SELECT name, version, description, input_schema, source, capabilities,
			        lockfile, resumable, live, ?, written_by, updated_at
			   FROM tools WHERE name = ?`, prevProcess, t.Name); err != nil {
			return 0, err
		}
		version++
		// Usage counters are kept across a rewrite: an improved tool is the
		// same tool as far as eviction is concerned.
		if _, err := tx.Exec(
			`UPDATE tools SET description=?, input_schema=?, source=?, capabilities=?, lockfile=?,
			                  resumable=?, live=?, version=?, probation=?, written_by=?, updated_at=?
			  WHERE name = ?`,
			t.Description, schema, t.Source, caps, lock, t.Resumable, t.Live,
			version, t.Probation, t.WrittenBy, nowText(), t.Name); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(
			`DELETE FROM tool_versions WHERE name = ? AND version <= ?`, t.Name, version-1-MaxToolVersions); err != nil {
			return 0, err
		}
	}
	return version, tx.Commit()
}

// GeneratedToolVersions returns a tool's previous versions, newest first.
func (s *Store) GeneratedToolVersions(name string) ([]ToolVersion, error) {
	rows, err := s.db.Query(`SELECT `+toolVersionColumns+` FROM tool_versions WHERE name = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := []ToolVersion{}
	for rows.Next() {
		v, err := scanToolVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// GeneratedToolVersion returns one previous version of a tool.
func (s *Store) GeneratedToolVersion(name string, version int) (ToolVersion, bool, error) {
	v, err := scanToolVersion(s.db.QueryRow(
		`SELECT `+toolVersionColumns+` FROM tool_versions WHERE name = ? AND version = ?`, name, version))
	if err == sql.ErrNoRows {
		return ToolVersion{}, false, nil
	}
	if err != nil {
		return ToolVersion{}, false, err
	}
	return v, true, nil
}

// GeneratedToolProve ends a tool's probation: its version has run.
func (s *Store) GeneratedToolProve(name string) error {
	_, err := s.db.Exec(`UPDATE tools SET probation = 0 WHERE name = ? AND probation != 0`, name)
	return err
}

const toolVersionColumns = `name, version, description, input_schema, source, capabilities,
	lockfile, resumable, live, process, written_by, written_at`

func scanToolVersion(row interface{ Scan(...any) error }) (ToolVersion, error) {
	var v ToolVersion
	var schema, caps, lock string
	if err := row.Scan(&v.Name, &v.Version, &v.Description, &schema, &v.Source, &caps,
		&lock, &v.Resumable, &v.Live, &v.Process, &v.WrittenBy, &v.WrittenAt); err != nil {
		return ToolVersion{}, err
	}
	v.InputSchema, v.Capabilities, v.Lockfile = json.RawMessage(schema), json.RawMessage(caps), json.RawMessage(lock)
	return v, nil
}

func orObject(raw json.RawMessage, def string) string {
	if len(raw) == 0 {
		return def
	}
	return string(raw)
}

// toolVersionsSchema is the versions table, for initSchema and its migration.
var toolVersionsSchema = fmt.Sprintf(`CREATE TABLE IF NOT EXISTS tool_versions (
	name         TEXT NOT NULL,
	version      INTEGER NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	input_schema TEXT NOT NULL DEFAULT '{}',
	source       TEXT NOT NULL DEFAULT '',
	capabilities TEXT NOT NULL DEFAULT '{}',
	lockfile     TEXT NOT NULL DEFAULT '{}',
	resumable    INTEGER NOT NULL DEFAULT 0,
	live         INTEGER NOT NULL DEFAULT 0,
	process      TEXT NOT NULL DEFAULT '',
	written_by   TEXT NOT NULL DEFAULT '',
	written_at   TEXT NOT NULL DEFAULT %s,
	PRIMARY KEY (name, version)
)`, nowExpr)
