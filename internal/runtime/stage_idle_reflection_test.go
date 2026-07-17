package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

func TestIdleReflectionStageOnIdleAlwaysHasWork(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	h := runtime.NewIdleReflectionStage(store)
	text, ok := h.OnIdle(context.Background(), "self-reflection")
	if !ok {
		t.Fatal("OnIdle ok = false, want true")
	}
	if text != runtime.ReflectionPrompt {
		t.Errorf("OnIdle text = %q, want ReflectionPrompt", text)
	}
}

func TestIdleReflectionStageOnTurnEndRecordsResult(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	h := runtime.NewIdleReflectionStage(store)
	if err := h.OnTurnEnd(context.Background(), "self-reflection", "updated self/learned", nil); err != nil {
		t.Fatalf("OnTurnEnd: %v", err)
	}

	refs, err := store.ReflectionList()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Summary != "updated self/learned" {
		t.Errorf("ReflectionList = %+v, want one entry with summary %q", refs, "updated self/learned")
	}
}

func TestIdleReflectionStageOnTurnEndIgnoresErrorsAndEmptyResults(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	h := runtime.NewIdleReflectionStage(store)
	if err := h.OnTurnEnd(context.Background(), "self-reflection", "", runtime.ErrStall); err != nil {
		t.Fatalf("OnTurnEnd(ErrStall): %v", err)
	}
	if err := h.OnTurnEnd(context.Background(), "self-reflection", "", nil); err != nil {
		t.Fatalf("OnTurnEnd(empty result): %v", err)
	}
	if err := h.OnTurnEnd(context.Background(), "self-reflection", "result", errors.New("boom")); err != nil {
		t.Fatalf("OnTurnEnd(turn error): %v", err)
	}

	refs, err := store.ReflectionList()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Errorf("ReflectionList = %+v, want no entries", refs)
	}
}

// ---- BootstrapSelfReflection ----

func TestBootstrapSelfReflectionCreatesPlan(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	if err := runtime.BootstrapSelfReflection(store, 2*time.Minute); err != nil {
		t.Fatalf("BootstrapSelfReflection: %v", err)
	}

	plan, err := store.SessionPlanGet(runtime.SelfReflectionAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil {
		t.Fatal("SessionPlanGet = nil, want a bootstrapped self-reflection plan")
	}
	if plan.Status != "active" {
		t.Errorf("plan.Status = %q, want active", plan.Status)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].Kind != "idle-reflection" || plan.Stages[0].Status != "active" {
		t.Fatalf("plan.Stages = %+v", plan.Stages)
	}

	var cfg struct {
		IdleIntervalSeconds int `json:"idle_interval_seconds"`
	}
	if err := json.Unmarshal(plan.Stages[0].Config, &cfg); err != nil {
		t.Fatalf("unmarshal stage config: %v", err)
	}
	if cfg.IdleIntervalSeconds != 120 {
		t.Errorf("idle_interval_seconds = %d, want 120", cfg.IdleIntervalSeconds)
	}
}

func TestBootstrapSelfReflectionNoOpIfExists(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	existing := &memory.SessionPlan{
		ID:     runtime.SelfReflectionAgentID,
		Status: "paused",
		Stages: []memory.SessionStage{{Name: "idle-reflection", Kind: "idle-reflection", Status: "done", Result: "custom"}},
	}
	if err := store.SessionPlanSave(existing); err != nil {
		t.Fatal(err)
	}

	if err := runtime.BootstrapSelfReflection(store, 2*time.Minute); err != nil {
		t.Fatalf("BootstrapSelfReflection: %v", err)
	}

	plan, err := store.SessionPlanGet(runtime.SelfReflectionAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != "paused" || plan.Stages[0].Status != "done" || plan.Stages[0].Result != "custom" {
		t.Errorf("existing plan was overwritten: %+v", plan)
	}
}
