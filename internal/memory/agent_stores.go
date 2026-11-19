package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// AgentStores manages the registry database plus one database file per agent
// (adr/agents-concept.md A4, Q9): the registry lives at [memory].path and holds
// the cross-agent state — processes, board_tasks, agent definitions, the skills
// catalog, and the session index; each agent's own state (conversations, kv,
// files, vectors, events, goals, …) lives in <dir>/agents/<agentID>.db next to
// it. Agent stores open lazily and are LRU-closed beyond the open cap (Q35).

const (
	// DefaultMaxOpenAgents is the default LRU cap on concurrently open agent
	// databases (Q35).
	DefaultMaxOpenAgents = 32
	// DefaultAgentID is the agent whose database holds sessions that were not
	// run from a stored agent (root conversations, daemon-spawned workers,
	// Q28).
	DefaultAgentID = "agent_default"
)

// AgentStores is the registry plus the set of lazily opened per-agent stores.
type AgentStores struct {
	registry *Store
	dir      string // <dir of registry>/agents
	maxOpen  int

	mu   sync.Mutex
	open map[string]*Store
	lru  []string // agentIDs, least-recently-used first

	// onOpen, when set, runs on every freshly opened agent store (seeding
	// skills, wiring the journal hook).
	onOpen func(*Store)
}

// SetOnOpen registers a callback run on every freshly opened agent store.
func (m *AgentStores) SetOnOpen(fn func(*Store)) { m.onOpen = fn }

// OpenAgentStores opens the registry at registryPath and prepares the per-agent
// directory beside it. maxOpen <= 0 uses DefaultMaxOpenAgents.
func OpenAgentStores(registryPath string, maxOpen int) (*AgentStores, error) {
	reg, err := Open(registryPath)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(filepath.Dir(registryPath), "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		reg.Close() //nolint:errcheck
		return nil, fmt.Errorf("create agent store directory: %w", err)
	}
	if maxOpen <= 0 {
		maxOpen = DefaultMaxOpenAgents
	}
	return &AgentStores{
		registry: reg,
		dir:      dir,
		maxOpen:  maxOpen,
		open:     map[string]*Store{},
	}, nil
}

// Registry returns the registry store (cross-agent state).
func (m *AgentStores) Registry() *Store { return m.registry }

// AgentDBPath is the file an agent's database lives in (Q9).
func (m *AgentStores) AgentDBPath(agentID string) string {
	return filepath.Join(m.dir, agentID+".db")
}

// AgentStore returns one agent's store, opening its file on first use. Beyond
// the open cap the least-recently-used store is closed (its WAL is checkpointed
// on Close, Q33) (Q35).
func (m *AgentStores) AgentStore(agentID string) (*Store, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.open[agentID]; ok {
		m.touchLocked(agentID)
		return st, nil
	}
	st, err := Open(m.AgentDBPath(agentID))
	if err != nil {
		return nil, err
	}
	if m.onOpen != nil {
		m.onOpen(st)
	}
	m.open[agentID] = st
	m.lru = append(m.lru, agentID)
	for len(m.open) > m.maxOpen {
		victim := m.lru[0]
		m.lru = m.lru[1:]
		v := m.open[victim]
		delete(m.open, victim)
		if err := v.Close(); err != nil {
			return nil, fmt.Errorf("evict %s: %w", victim, err)
		}
	}
	return st, nil
}

// OpenCount reports how many agent stores are currently open (for tests/metrics).
func (m *AgentStores) OpenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.open)
}

func (m *AgentStores) touchLocked(agentID string) {
	for i, id := range m.lru {
		if id == agentID {
			m.lru = append(m.lru[:i], m.lru[i+1:]...)
			m.lru = append(m.lru, agentID)
			return
		}
	}
}

// AgentIDs lists every agent that has a database file on disk (open or closed).
func (m *AgentStores) AgentIDs() ([]string, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) != ".db" {
			continue
		}
		out = append(out, name[:len(name)-len(".db")])
	}
	sort.Strings(out)
	return out, nil
}

// StoreForSession maps a session (conversation) id to the store holding its
// rows: the agent it was run from, or the default agent for sessions with no
// stored agent (Q28). The mapping lives in the registry's session_index.
func (m *AgentStores) StoreForSession(sessionID string) (*Store, error) {
	agentID, ok := m.registry.SessionIndexGet(sessionID)
	if !ok || agentID == "" {
		agentID = DefaultAgentID
	}
	return m.AgentStore(agentID)
}

// Close closes every open agent store and the registry.
func (m *AgentStores) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var firstErr error
	for id, st := range m.open {
		if err := st.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", id, err)
		}
		delete(m.open, id)
	}
	m.lru = nil
	if err := m.registry.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// --- the registry-side session index ---

// SessionIndexSet records which agent's database holds a session's rows.
func (s *Store) SessionIndexSet(id, agentID, status string) error {
	_, err := s.db.Exec(
		`INSERT INTO session_index (id, agent_id, status, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET agent_id = excluded.agent_id, status = excluded.status,
		 updated_at = excluded.updated_at`,
		id, agentID, status, nowText())
	return err
}

// SessionIndexGet returns the agent id a session is mapped to.
func (s *Store) SessionIndexGet(id string) (string, bool) {
	var agentID string
	err := s.db.QueryRow(`SELECT agent_id FROM session_index WHERE id = ?`, id).Scan(&agentID)
	if err == sql.ErrNoRows {
		return "", false
	}
	return agentID, err == nil
}

// SessionIndexDelete drops a session's index row (session_delete).
func (s *Store) SessionIndexDelete(id string) error {
	_, err := s.db.Exec(`DELETE FROM session_index WHERE id = ?`, id)
	return err
}

// SessionIndexEntry is one row of session_index.
type SessionIndexEntry struct {
	ID        string
	AgentID   string
	Status    string
	CreatedAt string
	UpdatedAt string
}

// SessionIndexList returns the whole index (the roster's backbone).
func (s *Store) SessionIndexList() ([]SessionIndexEntry, error) {
	rows, err := s.db.Query(`SELECT id, agent_id, status, created_at, updated_at FROM session_index ORDER BY updated_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionIndexEntry
	for rows.Next() {
		var e SessionIndexEntry
		if err := rows.Scan(&e.ID, &e.AgentID, &e.Status, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CheckpointWAL checkpoints the store's write-ahead log so the db file is
// self-contained (Q33 — called before a backup or bundle copies the file).
func (s *Store) CheckpointWAL() error {
	_, err := s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}
