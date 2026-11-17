package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
)

// Boot migration to per-agent databases (adr/agents-concept.md A4, Q11, Q29):
// a database written by a pre-agents binary holds everything in the single
// [memory].path file. At boot, every legacy conversation becomes its own agent
// (Q29 — adopting the role recorded on its session_created journal event), its
// keyed rows move into agents/<agentID>.db, the shared key-less tables move to
// the default agent's db, and the original file is kept as
// <path>.pre-agents.bak (Q11).

// keyedTables maps a per-agent table to the column that keys it to a session
// (conversation) id. capability_grants is handled specially (keyed by request).
var keyedTables = []struct {
	table string
	col   string
}{
	{"conversations", "id"},
	{"related_sessions", "agent_id"},
	{"interactive_sessions", "id"},
	{"jobs", "owner_id"},
	// workflows, goals, notifications, user_notifications, human_requests,
	// capability_requests/grants and session_events are deliberately absent:
	// their writes journal state events, and the wire follow's single seq
	// cursor only works with one global journal in the registry db.
}

// sharedTables have no agent key; they belong to the default agent.
var sharedTables = []string{"kv", "files", "workspace_files", "vectors", "tools", "tool_state"}

// MigrateToAgentStores runs the split on boot. It is a no-op when the registry
// holds no conversation rows (a fresh or already-migrated database).
func MigrateToAgentStores(m *AgentStores) error {
	reg := m.Registry()
	var n int
	if err := reg.db.QueryRow(`SELECT count(*) FROM conversations`).Scan(&n); err != nil {
		return fmt.Errorf("migrate to agent stores: %w", err)
	}
	if n == 0 {
		return nil
	}

	// Q11: keep the original file as <path>.pre-agents.bak before touching rows.
	regPath := reg.Path()
	if regPath != "" {
		if raw, err := os.ReadFile(regPath); err == nil {
			_ = os.WriteFile(regPath+".pre-agents.bak", raw, 0o600)
		}
	}

	// Shared, key-less tables move once, into the default agent's db.
	defStore, err := m.AgentStore(DefaultAgentID)
	if err != nil {
		return err
	}
	for _, t := range sharedTables {
		if err := copyTable(reg, defStore, t, "1=1"); err != nil {
			return fmt.Errorf("migrate %s: %w", t, err)
		}
		if _, err := reg.db.Exec(`DELETE FROM ` + t); err != nil {
			return fmt.Errorf("clear registry %s: %w", t, err)
		}
	}

	// One agent per legacy conversation (Q29).
	rows, err := reg.db.Query(`SELECT id FROM conversations ORDER BY created_at, id`)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()

	for _, convID := range ids {
		role := legacyRole(reg, convID)
		agent, err := reg.AgentCreateWithStatus("legacy-"+convID, role, "[]", "", "", AgentActive)
		if err != nil {
			return fmt.Errorf("migrate conversation %s: %w", convID, err)
		}
		dst, err := m.AgentStore(agent.ID)
		if err != nil {
			return err
		}
		for _, kt := range keyedTables {
			if err := copyTable(reg, dst, kt.table, kt.col+" = ?", convID); err != nil {
				return fmt.Errorf("migrate %s for %s: %w", kt.table, convID, err)
			}
			if _, err := reg.db.Exec(`DELETE FROM `+kt.table+` WHERE `+kt.col+` = ?`, convID); err != nil {
				return fmt.Errorf("clear registry %s for %s: %w", kt.table, convID, err)
			}
		}
		if err := reg.SessionIndexSet(convID, agent.ID, "active"); err != nil {
			return err
		}
	}
	return nil
}

// legacyRole recovers the role a legacy conversation ran as from its
// session_created journal event; "" when none is recorded.
func legacyRole(reg *Store, convID string) string {
	var payload string
	err := reg.db.QueryRow(
		`SELECT payload FROM session_events WHERE agent_id = ? AND type = 'session_created' ORDER BY seq DESC LIMIT 1`,
		convID).Scan(&payload)
	if err != nil || payload == "" {
		return ""
	}
	var p struct {
		Role string `json:"role"`
	}
	if json.Unmarshal([]byte(payload), &p) != nil {
		return ""
	}
	return p.Role
}

// copyTable copies rows matching `where` from src to dst, then the caller
// deletes them from src.
func copyTable(src, dst *Store, table, where string, args ...any) error {
	rows, err := src.db.Query(`SELECT * FROM `+table+` WHERE `+where, args...)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if err := insertRow(dst, table, cols, vals); err != nil {
			return err
		}
	}
	return rows.Err()
}

// insertRow inserts one row (cols/vals) into table in dst, ignoring conflicts.
func insertRow(dst *Store, table string, cols []string, vals []any) error {
	placeholders := make([]byte, 0, len(cols)*2)
	for i := range cols {
		if i > 0 {
			placeholders = append(placeholders, ',')
		}
		placeholders = append(placeholders, '?')
	}
	colList := ""
	for i, c := range cols {
		if i > 0 {
			colList += ","
		}
		colList += c
	}
	_, err := dst.db.Exec(
		`INSERT OR IGNORE INTO `+table+` (`+colList+`) VALUES (`+string(placeholders)+`)`, vals...)
	return err
}

// Path returns the file the store was opened from, "" when unknown (in-memory).
func (s *Store) Path() string { return s.path }
