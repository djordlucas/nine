package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

func TestGoalCreateGetUpdate(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Missing goal → (nil, nil).
	if g, err := store.GoalGet("nope"); err != nil || g != nil {
		t.Fatalf("missing goal: g=%v err=%v, want nil/nil", g, err)
	}

	if err := store.GoalCreate("g1", "monitor the repo", "conv1", "conversation"); err != nil {
		t.Fatal(err)
	}
	g, err := store.GoalGet("g1")
	if err != nil || g == nil {
		t.Fatalf("GoalGet: g=%v err=%v", g, err)
	}
	if g.Description != "monitor the repo" || g.ParentID != "conv1" || g.ParentType != "conversation" {
		t.Errorf("goal fields = %+v", g)
	}
	if g.Status != "active" {
		t.Errorf("default status = %q, want active", g.Status)
	}
	if g.Subtree == nil || len(g.Subtree) != 0 {
		t.Errorf("fresh subtree = %v, want empty non-nil", g.Subtree)
	}

	if err := store.GoalUpdateStatus("g1", "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.GoalUpdateDescription("g1", "monitor + triage"); err != nil {
		t.Fatal(err)
	}
	if err := store.GoalAppendSubtree("g1", "sub-goal-A"); err != nil {
		t.Fatal(err)
	}
	if err := store.GoalAppendSubtree("g1", "task-B"); err != nil {
		t.Fatal(err)
	}

	g, _ = store.GoalGet("g1")
	if g.Status != "done" || g.Description != "monitor + triage" {
		t.Errorf("after updates = %+v", g)
	}
	if len(g.Subtree) != 2 || g.Subtree[0] != "sub-goal-A" || g.Subtree[1] != "task-B" {
		t.Errorf("subtree = %v, want [sub-goal-A task-B] in order", g.Subtree)
	}
}

func TestGoalList(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GoalList(); len(got) != 0 {
		t.Fatalf("empty GoalList = %v", got)
	}
	_ = store.GoalCreate("a", "A", "", "")
	_ = store.GoalCreate("b", "B", "", "")
	got, err := store.GoalList()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("GoalList = %d, want 2", len(got))
	}
}
