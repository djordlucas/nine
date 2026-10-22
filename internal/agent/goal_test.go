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

// A wrong id or an unknown status is refused, not answered "ok" with nothing
// written: the model would otherwise believe the goal had changed.
func TestGoalUpdateStatus_RefusesUnknownGoalAndStatus(t *testing.T) {
	store := newGoalStore(t)
	if err := store.GoalCreate("goal-1", "test goal", "", ""); err != nil {
		t.Fatalf("create: %v", err)
	}
	d := agent.New()
	agent.RegisterGoalTools(d, "agent-1", store, nil)

	for _, tc := range []struct{ args, want string }{
		{`{"goal_id":"nope","status":"paused"}`, "goal not found"},
		{`{"id":"goal-1","status":"paused"}`, "goal_id is required"},
		{`{"goal_id":"goal-1","status":"sleeping"}`, "is not one of"},
	} {
		_, err := d.Dispatch(context.Background(), "goal_update_status", json.RawMessage(tc.args))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	if g, _ := store.GoalGet("goal-1"); g.Status != "active" {
		t.Errorf("a refused update was written: status %q", g.Status)
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
	spawnFn := agent.GoalSessionSpawnFn(func(_ context.Context, goalID, _ string) (bool, error) {
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
	spawnFn := agent.GoalSessionSpawnFn(func(context.Context, string, string) (bool, error) {
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
	spawnFn := agent.GoalSessionSpawnFn(func(context.Context, string, string) (bool, error) {
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
	spawnFn := agent.GoalSessionSpawnFn(func(context.Context, string, string) (bool, error) {
		return false, fmt.Errorf("boom")
	})
	agent.RegisterGoalTools(d, "agent-1", store, spawnFn)

	_, err := d.Dispatch(context.Background(), "goal_create",
		json.RawMessage(`{"description":"will fail to spawn"}`))
	if err == nil {
		t.Error("expected error when spawnFn fails, got nil")
	}
}

// goal_create hands a top-level goal's schedule to the spawn and returns it;
// it refuses an unreadable one, and a schedule on a sub-goal, before recording
// anything.
func TestGoalCreateWithASchedule(t *testing.T) {
	store := newGoalStore(t)
	d := agent.New()
	var gotCadence string
	agent.RegisterGoalTools(d, "agent-1", store, func(_ context.Context, goalID, cadence string) (bool, error) {
		gotCadence = cadence
		return true, store.ProcessUpsertDefinition(memory.Process{ID: "goal:" + goalID, Tool: "pursue", GoalID: goalID,
			IntervalSecs: 86400, Args: `{"schedule":"` + cadence + `"}`})
	})

	res, err := d.Dispatch(context.Background(), "goal_create", json.RawMessage(`{"description":"a daily news digest","schedule":"24h"}`))
	if err != nil {
		t.Fatalf("goal_create: %v", err)
	}
	if gotCadence != "24h" || !strings.Contains(res.Output, `"schedule":"24h"`) {
		t.Errorf("cadence passed %q, result %s; want 24h in both", gotCadence, res.Output)
	}

	before, _ := store.GoalList()
	for _, args := range []string{
		`{"description":"news","schedule":"daily"}`,
		`{"description":"sub","schedule":"24h","parent_id":"g0","parent_type":"goal"}`,
	} {
		if _, err := d.Dispatch(context.Background(), "goal_create", json.RawMessage(args)); err == nil {
			t.Errorf("goal_create %s: want an error", args)
		}
	}
	if after, _ := store.GoalList(); len(after) != len(before) {
		t.Errorf("a refused goal_create recorded a goal: %d goals, want %d", len(after), len(before))
	}
}
