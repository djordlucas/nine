package runtime_test

import (
	"context"
	"strings"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

func newPursueSession(t *testing.T, store *memory.Store, goalID, goalStatus string) {
	t.Helper()
	if err := store.GoalCreate(goalID, "monitor this repo for security issues", "conv-1", "conversation"); err != nil {
		t.Fatalf("GoalCreate: %v", err)
	}
	if goalStatus != "active" {
		if err := store.GoalUpdateStatus(goalID, goalStatus); err != nil {
			t.Fatalf("GoalUpdateStatus: %v", err)
		}
	}
	plan := &memory.SessionPlan{
		ID:      goalID,
		Status:  "active",
		Aspects: []memory.SessionAspect{{Name: "pursue", Kind: "pursue", Status: "active"}},
	}
	if err := store.SessionPlanSave(plan); err != nil {
		t.Fatalf("SessionPlanSave: %v", err)
	}
}

func stageStatus(t *testing.T, store *memory.Store, goalID string) string {
	t.Helper()
	plan, err := store.SessionPlanGet(goalID)
	if err != nil {
		t.Fatalf("SessionPlanGet: %v", err)
	}
	if plan == nil || len(plan.Aspects) != 1 {
		t.Fatalf("SessionPlanGet(%s) = %+v, want one stage", goalID, plan)
	}
	return plan.Aspects[0].Status
}

func TestPursueStageOnIdleActiveGoal(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	newPursueSession(t, store, "goal-1", "active")

	h := runtime.NewPursueAspect(store)
	text, ok := h.OnIdle(context.Background(), "goal-1")
	if !ok {
		t.Fatal("OnIdle ok = false, want true for active goal")
	}
	if !strings.Contains(text, "goal-1") || !strings.Contains(text, "monitor this repo for security issues") {
		t.Errorf("OnIdle text = %q, want it to mention the goal ID and description", text)
	}
}

func TestPursueStageOnIdleNoWorkWhenNotActive(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	h := runtime.NewPursueAspect(store)

	// No goal at all.
	if _, ok := h.OnIdle(context.Background(), "missing-goal"); ok {
		t.Error("OnIdle ok = true for missing goal, want false")
	}

	// Goal exists but is paused.
	newPursueSession(t, store, "goal-paused", "paused")
	if _, ok := h.OnIdle(context.Background(), "goal-paused"); ok {
		t.Error("OnIdle ok = true for paused goal, want false")
	}
}

func TestPursueStageOnIdleRetiresStageWhenNotActive(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	h := runtime.NewPursueAspect(store)

	// A goal paused/finished/archived while the session sat idle never reaches
	// OnTurnEnd, so OnIdle must sync the stage out of "active" itself — otherwise
	// the idle scheduler keeps arming and the slot keeps counting against
	// MaxGoalSessions.
	cases := []struct {
		goalStatus string
		wantStage  string
	}{
		{"paused", "paused"},
		{"done", "done"},
		{"archived", "done"},
	}
	for _, c := range cases {
		goalID := "idle-" + c.goalStatus
		newPursueSession(t, store, goalID, c.goalStatus)

		if _, ok := h.OnIdle(context.Background(), goalID); ok {
			t.Errorf("goal status %q: OnIdle ok = true, want false", c.goalStatus)
		}
		if got := stageStatus(t, store, goalID); got != c.wantStage {
			t.Errorf("goal status %q: stage status = %q, want %q", c.goalStatus, got, c.wantStage)
		}
	}
}

func TestPursueStageOnTurnEndSyncsStatus(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	h := runtime.NewPursueAspect(store)

	cases := []struct {
		goalStatus string
		wantStage  string
	}{
		{"active", "active"},
		{"paused", "paused"},
		{"done", "done"},
		{"archived", "done"},
	}
	for _, c := range cases {
		goalID := "goal-" + c.goalStatus
		newPursueSession(t, store, goalID, c.goalStatus)

		if err := h.OnTurnEnd(context.Background(), goalID, "did some work", nil); err != nil {
			t.Fatalf("OnTurnEnd(%s): %v", c.goalStatus, err)
		}
		if got := stageStatus(t, store, goalID); got != c.wantStage {
			t.Errorf("goal status %q: stage status = %q, want %q", c.goalStatus, got, c.wantStage)
		}
	}
}

func TestPursueStageOnTurnEndStallPausesGoal(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	newPursueSession(t, store, "goal-stalled", "active")

	h := runtime.NewPursueAspect(store)
	if err := h.OnTurnEnd(context.Background(), "goal-stalled", "", runtime.ErrStall); err != nil {
		t.Fatalf("OnTurnEnd(ErrStall): %v", err)
	}

	g, err := store.GoalGet("goal-stalled")
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != "paused" {
		t.Errorf("goal status = %q, want paused after stall", g.Status)
	}
	if got := stageStatus(t, store, "goal-stalled"); got != "paused" {
		t.Errorf("stage status = %q, want paused after stall", got)
	}
}

func TestPursueStageOnTurnEndUnknownGoalNoOp(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close() //nolint:errcheck

	h := runtime.NewPursueAspect(store)
	if err := h.OnTurnEnd(context.Background(), "no-such-goal", "result", nil); err != nil {
		t.Fatalf("OnTurnEnd(unknown goal): %v", err)
	}
}
