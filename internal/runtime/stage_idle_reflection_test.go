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

	h := runtime.NewIdleReflectionStage()
	text, ok := h.OnIdle(context.Background(), "self-reflection")
	if !ok {
		t.Fatal("OnIdle ok = false, want true")
	}
	if text != runtime.ReflectionPrompt {
		t.Errorf("OnIdle text = %q, want ReflectionPrompt", text)
	}
}

// OnTurnEnd is a no-op now: a reflection turn is recorded by the journal like
// any other, under its own agent_id. The dedicated `reflections` table it used
// to write recorded no agent id at all, which broke as soon as more than one
// session could reflect (docs/concept-consolidation.md C5).
func TestIdleReflectionOnTurnEndIsANoOp(t *testing.T) {
	h := runtime.NewIdleReflectionStage()
	cases := []struct {
		name   string
		result string
		err    error
	}{
		{"stalled", "", runtime.ErrStall},
		{"empty result", "", nil},
		{"turn error", "result", errors.New("boom")},
		{"successful reflection", "I learned something", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := h.OnTurnEnd(context.Background(), "self-reflection", tc.result, tc.err); err != nil {
				t.Errorf("OnTurnEnd = %v, want nil", err)
			}
		})
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
