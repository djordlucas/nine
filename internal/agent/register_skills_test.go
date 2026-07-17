package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/embed"
	"nine/internal/memory"
)

func skillDispatcher(t *testing.T) (*agent.Dispatcher, *memory.Store) {
	t.Helper()
	store := newTestStore(t)
	d := agent.New()
	e := embed.EmbedderFunc(func(_ context.Context, _ string) ([]float32, error) { return []float32{1}, nil })
	agent.RegisterSkillTools(d, store, e)
	return d, store
}

// skill_list returns the stored skills as JSON.
func TestSkillListTool(t *testing.T) {
	d, _ := skillDispatcher(t)
	ctx := context.Background()

	// Empty to start.
	res, err := d.Dispatch(ctx, "skill_list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Output) != "[]" {
		t.Errorf("empty skill_list = %q, want []", res.Output)
	}

	if _, err := d.Dispatch(ctx, "skill_write",
		json.RawMessage(`{"name":"deploy","description":"how to deploy","content":"steps"}`)); err != nil {
		t.Fatal(err)
	}
	res, _ = d.Dispatch(ctx, "skill_list", nil)
	if !strings.Contains(res.Output, "deploy") || !strings.Contains(res.Output, "how to deploy") {
		t.Errorf("skill_list = %q, want it to include the written skill", res.Output)
	}
}

// skill_modify merges into an existing agent skill (new content, preserved
// description), errors on an unknown skill, and refuses a built-in.
func TestSkillModifyTool(t *testing.T) {
	d, store := skillDispatcher(t)
	ctx := context.Background()

	// Unknown skill → error.
	if _, err := d.Dispatch(ctx, "skill_modify", json.RawMessage(`{"name":"ghost","content":"x"}`)); err == nil {
		t.Error("skill_modify on an unknown skill should error")
	}

	// Create an agent skill, then modify only its content.
	if _, err := d.Dispatch(ctx, "skill_write",
		json.RawMessage(`{"name":"notes","description":"orig desc","content":"v1"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, "skill_modify",
		json.RawMessage(`{"name":"notes","content":"v2"}`)); err != nil {
		t.Fatalf("skill_modify: %v", err)
	}
	rd, _ := d.Dispatch(ctx, "skill_read", json.RawMessage(`{"name":"notes"}`))
	if rd.Output != "v2" {
		t.Errorf("content after modify = %q, want v2", rd.Output)
	}
	// Description is preserved when not supplied (merge semantics).
	sk, _, _ := store.SkillGet("notes")
	if sk.Description != "orig desc" {
		t.Errorf("description = %q, want it preserved as \"orig desc\"", sk.Description)
	}

	// A built-in skill cannot be modified.
	if err := store.SkillUpsert(memory.Skill{Name: "core", Content: "x", Source: memory.SkillSourceBuiltin}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, "skill_modify", json.RawMessage(`{"name":"core","content":"y"}`)); err == nil {
		t.Error("skill_modify must refuse a built-in skill")
	}
}
