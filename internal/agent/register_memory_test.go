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

func TestFileSearchSemanticDefaultTopK(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterMemoryTools(d, store, embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) {
		return []float32{1, 0}, nil
	}), nil)

	_, err := d.Dispatch(context.Background(), "file_search_semantic",
		json.RawMessage(`{"query":"test"}`))
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func TestMemoryDeleteProtectedKey(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterMemoryTools(d, store, nil, []string{"self/"})

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
	agent.RegisterMemoryTools(d, store, nil, []string{"self/"})

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
