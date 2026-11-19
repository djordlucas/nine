package memory_test

import (
	"os"
	"path/filepath"
	"testing"

	"nine/internal/memory"
)

func TestAgentStoresOpenAndLRU(t *testing.T) {
	dir := t.TempDir()
	m, err := memory.OpenAgentStores(filepath.Join(dir, "nine.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	for _, id := range []string{"agent_a", "agent_b", "agent_c"} {
		if _, err := m.AgentStore(id); err != nil {
			t.Fatalf("open %s: %v", id, err)
		}
	}
	// Cap is 2: opening the third evicts the least-recently-used (agent_a).
	if got := m.OpenCount(); got != 2 {
		t.Fatalf("OpenCount = %d, want 2 (LRU cap)", got)
	}
	if _, err := os.Stat(m.AgentDBPath("agent_a")); err != nil {
		t.Fatalf("evicted store's file should still exist on disk: %v", err)
	}
	// Reopening agent_a evicts agent_b now.
	if _, err := m.AgentStore("agent_a"); err != nil {
		t.Fatal(err)
	}
	if got := m.OpenCount(); got != 2 {
		t.Fatalf("OpenCount after reopen = %d, want 2", got)
	}
	ids, err := m.AgentIDs()
	if err != nil || len(ids) != 3 {
		t.Fatalf("AgentIDs: %v %v", ids, err)
	}
}

func TestAgentStoresSessionIndex(t *testing.T) {
	dir := t.TempDir()
	m, err := memory.OpenAgentStores(filepath.Join(dir, "nine.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	// Unmapped sessions land in the default agent's db.
	st, err := m.StoreForSession("conv_x")
	if err != nil {
		t.Fatal(err)
	}
	if st.Path() != m.AgentDBPath(memory.DefaultAgentID) {
		t.Fatalf("unmapped session store = %s, want default agent db", st.Path())
	}
	// Mapped sessions land in their agent's db.
	if err := m.Registry().SessionIndexSet("conv_y", "agent_z", "active"); err != nil {
		t.Fatal(err)
	}
	st, err = m.StoreForSession("conv_y")
	if err != nil {
		t.Fatal(err)
	}
	if st.Path() != m.AgentDBPath("agent_z") {
		t.Fatalf("mapped session store = %s, want agent_z db", st.Path())
	}
}

// TestMigrateToAgentStores builds a legacy single-file store (conversations and
// journal rows in the one db) and checks the boot split: one agent per
// conversation, rows moved, index set, backup file kept (Q11/Q29).
func TestMigrateToAgentStores(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "nine.db")

	legacy, err := memory.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.ConversationCreate("conv_1"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.ConversationCreate("conv_2"); err != nil {
		t.Fatal(err)
	}
	// A journal row recording conv_1's role (what legacyRole reads).
	if err := legacy.SessionEventsAppend([]memory.SessionEvent{{
		AgentID: "conv_1", Type: "session_created", Payload: []byte(`{"role":"researcher"}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := legacy.QueueMessage("conv_1", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	m, err := memory.OpenAgentStores(dbPath, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := memory.MigrateToAgentStores(m); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Backup file kept (Q11).
	if _, err := os.Stat(dbPath + ".pre-agents.bak"); err != nil {
		t.Fatalf("pre-agents backup: %v", err)
	}
	// Registry conversations are drained; each legacy conversation is an agent.
	if c, err := m.Registry().ConversationGet("conv_1"); err != nil || c != nil {
		t.Fatalf("registry still holds conv_1 after migration: c=%v err=%v", c, err)
	}
	agents, err := m.Registry().AgentList()
	if err != nil || len(agents) != 2 {
		t.Fatalf("agents after migration: n=%d err=%v", len(agents), err)
	}
	var sawResearcher bool
	for _, a := range agents {
		if a.Role == "researcher" {
			sawResearcher = true
		}
		// The conversation row moved into the agent's db; the journal stays in
		// the registry (the wire follow's single seq cursor requires one global
		// journal).
		st, err := m.AgentStore(a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.ConversationGet(conversationIDForAgent(t, m, a.ID)); err != nil {
			t.Fatalf("agent %s db missing its conversation: %v", a.ID, err)
		}
	}
	if !sawResearcher {
		t.Fatal("no migrated agent adopted the session_created role")
	}
	// The event journal stays daemon-wide in the registry db (only conv_1 has
	// one in this fixture).
	if evs, err := m.Registry().SessionEventsByAgent("conv_1"); err != nil || len(evs) == 0 {
		t.Fatalf("registry journal lost conv_1's events: %v %d", err, len(evs))
	}
	// Session index maps both conversations.
	for _, c := range []string{"conv_1", "conv_2"} {
		if _, ok := m.Registry().SessionIndexGet(c); !ok {
			t.Fatalf("session index missing %s", c)
		}
	}
	// Re-running the migration is a no-op.
	if err := memory.MigrateToAgentStores(m); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	agents2, _ := m.Registry().AgentList()
	if len(agents2) != 2 {
		t.Fatalf("re-migrate created more agents: %d", len(agents2))
	}
}

func conversationIDForAgent(t *testing.T, m *memory.AgentStores, agentID string) string {
	t.Helper()
	index, err := m.Registry().SessionIndexList()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range index {
		if e.AgentID == agentID {
			return e.ID
		}
	}
	t.Fatalf("no session mapped to %s", agentID)
	return ""
}

func TestAgentBundleWithDBRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m, err := memory.OpenAgentStores(filepath.Join(dir, "nine.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	a, err := m.Registry().AgentCreate("WithDB", "writer", `[]`, "", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.AgentStore(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ConversationCreate("conv_in_agent"); err != nil {
		t.Fatal(err)
	}
	if err := st.CheckpointWAL(); err != nil {
		t.Fatal(err)
	}
	dbBytes, err := os.ReadFile(m.AgentDBPath(a.ID))
	if err != nil {
		t.Fatal(err)
	}

	tarBytes, err := memory.AgentBundleTarWithDB(a, dbBytes)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := memory.ParseAgentBundleTar(tarBytes)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m2.Name != "WithDB" || m2.SourceAgentID != a.ID {
		t.Fatalf("manifest: %+v", m2)
	}
}
