package runtime

import (
	"path/filepath"
	"testing"

	"nine/internal/memory"
)

// The router over per-agent stores (A4/Q31): session-scoped writes land in the
// session's agent db, daemon-wide reads fan out and merge, cross-agent state
// stays in the registry, and backup snapshots every db (Q33).
func TestAgentStoreRouterRoutesAndFansOut(t *testing.T) {
	dir := t.TempDir()
	mgr, err := memory.OpenAgentStores(filepath.Join(dir, "nine.db"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer mgr.Close()
	r := newAgentStoreRouter(mgr)

	a1, err := r.AgentCreate("Alpha", "researcher", "[]", "", "")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := r.AgentCreate("Beta", "writer", "[]", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SessionIndexSet("s1", a1.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.SessionIndexSet("s2", a2.ID); err != nil {
		t.Fatal(err)
	}

	// Session-scoped rows go to the mapped agent's db, not the registry.
	st1, err := mgr.StoreForSession("s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := st1.ConversationCreate("s1"); err != nil {
		t.Fatal(err)
	}
	if err := st1.Set("k1", "v1"); err != nil {
		t.Fatal(err)
	}
	st2, err := mgr.StoreForSession("s2")
	if err != nil {
		t.Fatal(err)
	}
	if err := st2.ConversationCreate("s2"); err != nil {
		t.Fatal(err)
	}
	if err := st2.Set("k2", "v2"); err != nil {
		t.Fatal(err)
	}
	if c, err := mgr.Registry().ConversationGet("s1"); err != nil || c != nil {
		t.Fatalf("registry holds s1: c=%v err=%v", c, err)
	}

	// An unmapped session lands in the default agent's db (Q28).
	if err := r.ConversationCreate("s0"); err != nil {
		t.Fatal(err)
	}
	st0, err := mgr.StoreForSession("s0")
	if err != nil {
		t.Fatal(err)
	}
	if st0.Path() != mgr.AgentDBPath(memory.DefaultAgentID) {
		t.Fatalf("unmapped session in %s, want default agent db", st0.Path())
	}
	if c, err := st0.ConversationGet("s0"); err != nil || c == nil {
		t.Fatalf("default agent db missing s0: c=%v err=%v", c, err)
	}

	// Daemon-wide reads fan out and merge across agent dbs.
	sessions, err := r.SessionList()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("SessionList = %d sessions, want 2: %+v", len(sessions), sessions)
	}
	kvs, err := r.KVEntries()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, kv := range kvs {
		seen[kv.Key] = true
	}
	if !seen["k1"] || !seen["k2"] {
		t.Fatalf("KVEntries = %+v, want k1 and k2", kvs)
	}
	if v, ok, err := r.Get("k1"); err != nil || !ok || v != "v1" {
		t.Fatalf("Get(k1) = %q %v %v", v, ok, err)
	}

	// Cross-agent state stays in the registry and round-trips through the router.
	bt, err := r.BoardTaskCreate("title", "spec", "test", "test", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := r.BoardTaskGet(bt.ID); err != nil || !ok {
		t.Fatalf("BoardTaskGet: ok=%v err=%v", ok, err)
	}
	agents, err := r.AgentList()
	if err != nil || len(agents) != 2 {
		t.Fatalf("AgentList: n=%d err=%v", len(agents), err)
	}

	// Backup snapshots the registry plus every agent db (Q33).
	files, err := r.BackupDBs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
	}
	for _, want := range []string{"nine.db", "agents/" + a1.ID + ".db", "agents/" + a2.ID + ".db", "agents/" + memory.DefaultAgentID + ".db"} {
		if !names[want] {
			t.Fatalf("backup missing %s: %v", want, names)
		}
	}
}
