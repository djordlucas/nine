package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
	"nine/internal/embed"
	"nine/internal/memory"
)

func TestSkillWriteEmbedsAndPersists(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()

	e := embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1}, nil
	})
	agent.RegisterSkillTools(d, store, e)

	res, err := d.Dispatch(context.Background(), "skill_write",
		json.RawMessage(`{"name":"s","description":"does stuff","content":"body"}`))
	if err != nil {
		t.Fatalf("skill_write: %v", err)
	}
	if res.Output != "ok" {
		t.Errorf("output = %q, want 'ok'", res.Output)
	}

	// Content is persisted and readable.
	rd, err := d.Dispatch(context.Background(), "skill_read", json.RawMessage(`{"name":"s"}`))
	if err != nil {
		t.Fatalf("skill_read: %v", err)
	}
	if rd.Output != "body" {
		t.Errorf("skill_read = %q, want 'body'", rd.Output)
	}

	// Description is embedded for semantic retrieval.
	results, err := store.VectorQuery("skills", []float32{1}, 1)
	if err != nil {
		t.Fatalf("vector query: %v", err)
	}
	if len(results) == 0 {
		t.Error("no vector stored after skill_write")
	}
}

func TestSkillWriteRefusesBuiltin(t *testing.T) {
	store := newTestStore(t)
	if err := store.SkillUpsert(memory.Skill{Name: "core", Source: memory.SkillSourceBuiltin}); err != nil {
		t.Fatal(err)
	}
	d := agent.New()
	agent.RegisterSkillTools(d, store, nil)

	if _, err := d.Dispatch(context.Background(), "skill_write",
		json.RawMessage(`{"name":"core","description":"x","content":"y"}`)); err == nil {
		t.Fatal("expected skill_write to refuse overwriting a built-in skill")
	}
}

// A user skill is file-backed and operator-owned, so Nine may not overwrite or
// modify it either — a runtime write would be undone by the next boot's reseed
// and would let the agent edit the operator's intent (R-SKILL.2).
func TestSkillWriteAndModifyRefuseUserSkills(t *testing.T) {
	store := newTestStore(t)
	if err := store.SkillUpsert(memory.Skill{
		Name: "ops-runbook", Description: "Operator's runbook.", Content: "original",
		Source: memory.SkillSourceUser,
	}); err != nil {
		t.Fatal(err)
	}
	d := agent.New()
	agent.RegisterSkillTools(d, store, nil)

	if _, err := d.Dispatch(context.Background(), "skill_write",
		json.RawMessage(`{"name":"ops-runbook","description":"x","content":"y"}`)); err == nil {
		t.Error("expected skill_write to refuse overwriting a user skill")
	}
	if _, err := d.Dispatch(context.Background(), "skill_modify",
		json.RawMessage(`{"name":"ops-runbook","content":"y"}`)); err == nil {
		t.Error("expected skill_modify to refuse modifying a user skill")
	}

	sk, found, err := store.SkillGet("ops-runbook")
	if err != nil || !found {
		t.Fatalf("skill vanished: found=%v err=%v", found, err)
	}
	if sk.Content != "original" {
		t.Errorf("content = %q, want it unchanged", sk.Content)
	}
}

func TestFileSearchSemanticDefaultTopK(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterMemoryTools(d, store, embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1, 0}, nil
	}), nil, false)

	_, err := d.Dispatch(context.Background(), "file_search_semantic",
		json.RawMessage(`{"query":"test"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func TestMemorySetIndexesAndDeleteRemovesFromPool(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()

	// A deterministic embedder so the query below is an exact hit.
	vec := []float32{1, 0, 0}
	e := embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return vec, nil
	})
	agent.RegisterMemoryTools(d, store, e, nil, true /* indexMemories */)

	if _, err := d.Dispatch(context.Background(), "memory_set",
		json.RawMessage(`{"key":"pref","value":"user runs Postgres"}`)); err != nil {
		t.Fatalf("memory_set: %v", err)
	}

	// The memory is mirrored into the shared pool, keyed by the KV key.
	got, err := store.VectorQuery(memory.MemoriesNamespace, vec, 5)
	if err != nil {
		t.Fatalf("vector query: %v", err)
	}
	if len(got) != 1 || got[0].Key != "pref" {
		t.Fatalf("pool after set = %+v, want one entry keyed 'pref'", got)
	}

	// memory_delete removes the mirror so the pool stays in sync with the KV store.
	if _, err := d.Dispatch(context.Background(), "memory_delete",
		json.RawMessage(`{"key":"pref"}`)); err != nil {
		t.Fatalf("memory_delete: %v", err)
	}
	if got, _ := store.VectorQuery(memory.MemoriesNamespace, vec, 5); len(got) != 0 {
		t.Fatalf("pool after delete = %+v, want empty", got)
	}
}

func TestMemorySetDoesNotIndexWhenDisabled(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	e := embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1, 0, 0}, nil
	})
	agent.RegisterMemoryTools(d, store, e, nil, false /* indexMemories off */)

	if _, err := d.Dispatch(context.Background(), "memory_set",
		json.RawMessage(`{"key":"pref","value":"v"}`)); err != nil {
		t.Fatalf("memory_set: %v", err)
	}
	if got, _ := store.VectorQuery(memory.MemoriesNamespace, []float32{1, 0, 0}, 5); len(got) != 0 {
		t.Fatalf("pool = %+v, want empty when indexing is disabled", got)
	}
}

func TestMemoryDeleteProtectedKey(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterMemoryTools(d, store, nil, []string{"self/"}, false)

	if err := store.Set("self/capabilities", "can do things"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := d.Dispatch(context.Background(), "memory_delete",
		json.RawMessage(`{"key":"self/capabilities"}`))
	if err == nil {
		t.Fatal("expected error deleting protected key, got nil")
	}

	// Key must still exist.
	val, found, _ := store.Get("self/capabilities")
	if !found || val == "" {
		t.Error("protected key was deleted despite protection")
	}
}

func TestMemoryDeleteUnprotectedKey(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterMemoryTools(d, store, nil, []string{"self/"}, false)

	if err := store.Set("user/pref", "dark mode"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	_, err := d.Dispatch(context.Background(), "memory_delete",
		json.RawMessage(`{"key":"user/pref"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, found, _ := store.Get("user/pref")
	if found {
		t.Error("unprotected key was not deleted")
	}
}
