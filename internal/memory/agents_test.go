package memory_test

import (
	"encoding/json"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func TestAgentCRUD(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Create requires a name and a JSON-array skills field.
	if _, err := store.AgentCreate("  ", "", "", "", ""); err == nil {
		t.Fatal("create with empty name: want error")
	}
	if _, err := store.AgentCreate("a", "", "not-json", "", ""); err == nil {
		t.Fatal("create with non-JSON skills: want error")
	}

	a1, err := store.AgentCreate("Researcher", "researcher", `["web"]`, `{"k":"v"}`, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a1.ID == "" || a1.Version != 1 || a1.Name != "Researcher" || a1.Role != "researcher" {
		t.Fatalf("bad initial agent: %+v", a1)
	}

	// Get + list.
	got, ok, err := store.AgentGet(a1.ID)
	if err != nil || !ok || got.Name != "Researcher" || got.Skills != `["web"]` {
		t.Fatalf("get: ok=%v err=%v agent=%+v", ok, err, got)
	}
	if _, ok, err := store.AgentGet("agent_nope"); err != nil || ok {
		t.Fatalf("get missing: ok=%v err=%v", ok, err)
	}
	all, err := store.AgentList()
	if err != nil || len(all) != 1 {
		t.Fatalf("list: n=%d err=%v", len(all), err)
	}

	// Update bumps the version and records both versions; nil fields are
	// unchanged, an empty name is refused.
	role := "writer"
	upd, ok, err := store.AgentUpdate(a1.ID, memory.AgentPatch{Role: &role})
	if err != nil || !ok {
		t.Fatalf("update: ok=%v err=%v", ok, err)
	}
	if upd.Version != 2 || upd.Role != "writer" || upd.Name != "Researcher" {
		t.Fatalf("bad update: %+v", upd)
	}
	empty := ""
	if _, _, err := store.AgentUpdate(a1.ID, memory.AgentPatch{Name: &empty}); err == nil {
		t.Fatal("update with empty name: want error")
	}
	if _, ok, err := store.AgentUpdate("agent_nope", memory.AgentPatch{Role: &role}); err != nil || ok {
		t.Fatalf("update missing: ok=%v err=%v", ok, err)
	}
	versions, err := store.AgentVersions(a1.ID)
	if err != nil || len(versions) != 2 || versions[0].Version != 1 || versions[1].Version != 2 {
		t.Fatalf("versions: n=%d err=%v", len(versions), err)
	}
	var v1 memory.Agent
	if err := json.Unmarshal([]byte(versions[0].Definition), &v1); err != nil || v1.Role != "researcher" {
		t.Fatalf("version 1 blob: %v %+v", err, v1)
	}

	// Delete removes the row and its versions.
	ok, err = store.AgentDelete(a1.ID)
	if err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := store.AgentGet(a1.ID); ok {
		t.Fatal("get after delete: want not found")
	}
	if versions, _ := store.AgentVersions(a1.ID); len(versions) != 0 {
		t.Fatalf("versions after delete: n=%d", len(versions))
	}
	if ok, _ := store.AgentDelete(a1.ID); ok {
		t.Fatal("second delete: want false")
	}
}
