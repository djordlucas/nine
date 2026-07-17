package runtime_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

// registerPursueStage registers the real "pursue" StageHandler factory for
// the duration of the test, mirroring cmd/nine/daemon.go's wiring. Required
// for SpawnGoalSession's plan to load successfully.
func registerPursueStage(t *testing.T, store *memory.Store) {
	t.Helper()
	runtime.StageRegistry["pursue"] = func() runtime.StageHandler { return runtime.NewPursueStage(store) }
	t.Cleanup(func() { delete(runtime.StageRegistry, "pursue") })
}

func TestSpawnGoalSessionRequiresPlanStore(t *testing.T) {
	d, _ := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)

	_, err := d.SpawnGoalSession(context.Background(), "goal-1")
	if err == nil {
		t.Fatal("SpawnGoalSession without a plan store: want error, got nil")
	}
}

func TestSpawnGoalSessionCreatesAndIsIdempotent(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	registerPursueStage(t, store)

	d, _ := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigurePlanStore(store)

	spawned, err := d.SpawnGoalSession(context.Background(), "goal-1")
	if err != nil {
		t.Fatalf("SpawnGoalSession: %v", err)
	}
	if !spawned {
		t.Fatal("SpawnGoalSession spawned = false, want true")
	}

	plan, err := store.SessionPlanGet("goal-1")
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || len(plan.Stages) != 1 || plan.Stages[0].Kind != "pursue" || plan.Stages[0].Status != "active" {
		t.Fatalf("SessionPlanGet(goal-1) = %+v, want one active pursue stage", plan)
	}
	if got := d.ActiveGoalSessionCountForTest(); got != 1 {
		t.Errorf("ActiveGoalSessionCount = %d, want 1", got)
	}

	// Spawning again for the same goal is a no-op.
	spawned, err = d.SpawnGoalSession(context.Background(), "goal-1")
	if err != nil {
		t.Fatalf("SpawnGoalSession (second call): %v", err)
	}
	if !spawned {
		t.Error("SpawnGoalSession (second call) spawned = false, want true (idempotent)")
	}
	if got := d.ActiveGoalSessionCountForTest(); got != 1 {
		t.Errorf("ActiveGoalSessionCount after second spawn = %d, want 1", got)
	}
}

func TestSpawnStandingSessionSeedsRoleAndInterval(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	registerPursueStage(t, store)

	d, _ := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigurePlanStore(store)

	spawned, err := d.SpawnStandingSession(context.Background(), "sec-watch", "monitor", false, time.Hour, "")
	if err != nil {
		t.Fatalf("SpawnStandingSession: %v", err)
	}
	if !spawned {
		t.Fatal("SpawnStandingSession spawned = false, want true")
	}

	plan, err := store.SessionPlanGet("sec-watch")
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || len(plan.Stages) != 1 || plan.Stages[0].Kind != "pursue" {
		t.Fatalf("SessionPlanGet(sec-watch) = %+v, want one pursue stage", plan)
	}
	// The seeded stage config carries the work role and the configured interval.
	var cfg struct {
		IdleIntervalSeconds int    `json:"idle_interval_seconds"`
		Role                string `json:"role"`
	}
	if err := json.Unmarshal(plan.Stages[0].Config, &cfg); err != nil {
		t.Fatalf("unmarshal stage config: %v", err)
	}
	if cfg.Role != "monitor" {
		t.Errorf("stage role = %q, want %q", cfg.Role, "monitor")
	}
	if cfg.IdleIntervalSeconds != 3600 {
		t.Errorf("idle interval = %d s, want 3600", cfg.IdleIntervalSeconds)
	}

	// Idempotent for an already-running standing session.
	spawned, err = d.SpawnStandingSession(context.Background(), "sec-watch", "monitor", false, time.Hour, "")
	if err != nil || !spawned {
		t.Errorf("SpawnStandingSession (second call) = (%v, %v), want (true, nil)", spawned, err)
	}
	if got := d.ActiveGoalSessionCountForTest(); got != 1 {
		t.Errorf("ActiveGoalSessionCount = %d, want 1", got)
	}
}

func TestTeardownStandingSession(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	registerPursueStage(t, store)

	d, _ := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigurePlanStore(store)

	if _, err := d.SpawnStandingSession(context.Background(), "sec-watch", "monitor", false, time.Hour, ""); err != nil {
		t.Fatalf("SpawnStandingSession: %v", err)
	}
	if got := d.ActiveGoalSessionCountForTest(); got != 1 {
		t.Fatalf("ActiveGoalSessionCount = %d, want 1 before teardown", got)
	}

	if err := d.TeardownStandingSession(context.Background(), "sec-watch"); err != nil {
		t.Fatalf("TeardownStandingSession: %v", err)
	}

	// Session is gone and no longer counts against the cap.
	if got := d.ActiveGoalSessionCountForTest(); got != 0 {
		t.Errorf("ActiveGoalSessionCount = %d, want 0 after teardown", got)
	}
	// The plan is deactivated, so a later boot never resumes it.
	plan, err := store.SessionPlanGet("sec-watch")
	if err != nil {
		t.Fatal(err)
	}
	if plan == nil || plan.Status == "active" {
		t.Errorf("session plan = %+v, want a non-active status after teardown", plan)
	}

	// Idempotent: a second teardown (nothing running, plan already inactive) is a no-op.
	if err := d.TeardownStandingSession(context.Background(), "sec-watch"); err != nil {
		t.Errorf("second TeardownStandingSession returned error: %v", err)
	}
}

func TestSpawnGoalSessionRespectsMaxGoalSessions(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck
	registerPursueStage(t, store)

	d, _ := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigurePlanStore(store)
	d.SetMaxGoalSessions(1)

	spawned, err := d.SpawnGoalSession(context.Background(), "goal-1")
	if err != nil || !spawned {
		t.Fatalf("SpawnGoalSession(goal-1) = (%v, %v), want (true, nil)", spawned, err)
	}

	spawned, err = d.SpawnGoalSession(context.Background(), "goal-2")
	if err != nil {
		t.Fatalf("SpawnGoalSession(goal-2): %v", err)
	}
	if spawned {
		t.Error("SpawnGoalSession(goal-2) spawned = true, want false (at MaxGoalSessions cap)")
	}

	if got := d.ActiveGoalSessionCountForTest(); got != 1 {
		t.Errorf("ActiveGoalSessionCount = %d, want 1", got)
	}
	if plan, err := store.SessionPlanGet("goal-2"); err != nil || plan != nil {
		t.Errorf("SessionPlanGet(goal-2) = (%+v, %v), want (nil, nil) when at cap", plan, err)
	}
}
