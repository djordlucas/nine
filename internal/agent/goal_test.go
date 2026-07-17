package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func newGoalStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open goal store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// --- GoalTools tests ---

func TestWireGoalCreate(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "goal_create",
		json.RawMessage(`{"description":"keep dependencies up to date"}`))
	if err != nil {
		t.Fatalf("goal_create: %v", err)
	}

	var out struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		Status      string `json:"status"`
		ParentID    string `json:"parent_id"`
		ParentType  string `json:"parent_type"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID == "" {
		t.Error("expected non-empty goal ID")
	}
	if out.Description != "keep dependencies up to date" {
		t.Errorf("description = %q, want 'keep dependencies up to date'", out.Description)
	}
	if out.Status != "active" {
		t.Errorf("status = %q, want 'active'", out.Status)
	}
	// No parent given — defaults to the owning conversation.
	if out.ParentID != "agent-1" || out.ParentType != "conversation" {
		t.Errorf("parent = (%q, %q), want ('agent-1', 'conversation')", out.ParentID, out.ParentType)
	}
}

func TestWireGoalCreateExplicitParent(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "goal_create",
		json.RawMessage(`{"description":"sub-goal","parent_id":"goal-parent","parent_type":"goal"}`))
	if err != nil {
		t.Fatalf("goal_create: %v", err)
	}

	var out struct {
		ParentID   string `json:"parent_id"`
		ParentType string `json:"parent_type"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ParentID != "goal-parent" || out.ParentType != "goal" {
		t.Errorf("parent = (%q, %q), want ('goal-parent', 'goal')", out.ParentID, out.ParentType)
	}
}

func TestWireGoalCreateRequiresDescription(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	_, err := d.Dispatch(context.Background(), "goal_create", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for missing description, got nil")
	}
}

func TestWireGoalGet(t *testing.T) {
	store := newGoalStore(t)
	if err := store.GoalCreate("goal-1", "test goal", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "goal_get", json.RawMessage(`{"goal_id":"goal-1"}`))
	if err != nil {
		t.Fatalf("goal_get: %v", err)
	}
	if !strings.Contains(res.Output, "goal-1") {
		t.Errorf("output = %q, want to contain 'goal-1'", res.Output)
	}
	if !strings.Contains(res.Output, "test goal") {
		t.Errorf("output = %q, want to contain 'test goal'", res.Output)
	}
}

func TestWireGoalGetNotFound(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	_, err := d.Dispatch(context.Background(), "goal_get", json.RawMessage(`{"goal_id":"missing"}`))
	if err == nil {
		t.Error("expected error for missing goal, got nil")
	}
}

func TestWireGoalGetInvalidJSON(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	agent.RegisterGoalTools(d, "a", store, nil)

	_, err := d.Dispatch(context.Background(), "goal_get", json.RawMessage(`{bad}`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestWireGoalList(t *testing.T) {
	store := newGoalStore(t)
	if err := store.GoalCreate("goal-1", "alpha", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.GoalCreate("goal-2", "beta", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "goal_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("goal_list: %v", err)
	}
	if !strings.Contains(res.Output, "alpha") || !strings.Contains(res.Output, "beta") {
		t.Errorf("output = %q, want to contain both goals", res.Output)
	}
}

func TestWireGoalUpdateStatus(t *testing.T) {
	store := newGoalStore(t)
	if err := store.GoalCreate("goal-1", "test goal", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "goal_update_status",
		json.RawMessage(`{"goal_id":"goal-1","status":"done"}`))
	if err != nil {
		t.Fatalf("goal_update_status: %v", err)
	}
	if res.Output != "ok" {
		t.Errorf("output = %q, want 'ok'", res.Output)
	}

	g, err := store.GoalGet("goal-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if g.Status != "done" {
		t.Errorf("status = %q, want 'done'", g.Status)
	}
}

// --- GoalSessionSpawnFn ---

func TestWireGoalCreateSpawnsSessionForTopLevelGoal(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	var gotGoalID string
	spawnFn := agent.GoalSessionSpawnFn(func(_ context.Context, goalID string) (bool, error) {
		gotGoalID = goalID
		return true, nil
	})
	agent.RegisterGoalTools(d, "agent-1", store, spawnFn)

	res, err := d.Dispatch(context.Background(), "goal_create",
		json.RawMessage(`{"description":"monitor this repo for security issues"}`))
	if err != nil {
		t.Fatalf("goal_create: %v", err)
	}

	var out struct {
		ID            string `json:"id"`
		PursueSession string `json:"pursue_session"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PursueSession != "spawned" {
		t.Errorf("pursue_session = %q, want %q", out.PursueSession, "spawned")
	}
	if gotGoalID != out.ID {
		t.Errorf("spawnFn called with goalID %q, want %q", gotGoalID, out.ID)
	}
}

func TestWireGoalCreateLimitReached(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	spawnFn := agent.GoalSessionSpawnFn(func(context.Context, string) (bool, error) {
		return false, nil
	})
	agent.RegisterGoalTools(d, "agent-1", store, spawnFn)

	res, err := d.Dispatch(context.Background(), "goal_create",
		json.RawMessage(`{"description":"keep dependencies up to date"}`))
	if err != nil {
		t.Fatalf("goal_create: %v", err)
	}

	var out struct {
		PursueSession string `json:"pursue_session"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PursueSession != "limit_reached" {
		t.Errorf("pursue_session = %q, want %q", out.PursueSession, "limit_reached")
	}
}

func TestWireGoalCreateSubGoalDoesNotSpawn(t *testing.T) {
	store := newGoalStore(t)
	if err := store.GoalCreate("goal-parent", "parent goal", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := agent.New()
	called := false
	spawnFn := agent.GoalSessionSpawnFn(func(context.Context, string) (bool, error) {
		called = true
		return true, nil
	})
	agent.RegisterGoalTools(d, "agent-1", store, spawnFn)

	res, err := d.Dispatch(context.Background(), "goal_create",
		json.RawMessage(`{"description":"sub-goal","parent_id":"goal-parent","parent_type":"goal"}`))
	if err != nil {
		t.Fatalf("goal_create: %v", err)
	}
	if called {
		t.Error("spawnFn was called for a sub-goal, want it untouched")
	}

	var out map[string]any
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := out["pursue_session"]; ok {
		t.Errorf("result has pursue_session = %v for a sub-goal, want absent", out["pursue_session"])
	}
}

func TestWireGoalCreateSpawnFnError(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	spawnFn := agent.GoalSessionSpawnFn(func(context.Context, string) (bool, error) {
		return false, fmt.Errorf("boom")
	})
	agent.RegisterGoalTools(d, "agent-1", store, spawnFn)

	_, err := d.Dispatch(context.Background(), "goal_create",
		json.RawMessage(`{"description":"will fail to spawn"}`))
	if err == nil {
		t.Error("expected error when spawnFn fails, got nil")
	}
}

func TestWireGoalAppendSubtree(t *testing.T) {
	store := newGoalStore(t)
	if err := store.GoalCreate("goal-1", "test goal", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "goal_append_subtree",
		json.RawMessage(fmt.Sprintf(`{"goal_id":"goal-1","entry":%q}`, "sub-goal-1")))
	if err != nil {
		t.Fatalf("goal_append_subtree: %v", err)
	}
	if res.Output != "ok" {
		t.Errorf("output = %q, want 'ok'", res.Output)
	}

	g, err := store.GoalGet("goal-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(g.Subtree) != 1 || g.Subtree[0] != "sub-goal-1" {
		t.Errorf("subtree = %v, want ['sub-goal-1']", g.Subtree)
	}
}
