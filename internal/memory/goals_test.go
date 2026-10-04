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
	if len(g.Subtree) != 0 {
		t.Errorf("fresh subtree = %v, want empty", g.Subtree)
	}

	if err := store.GoalUpdateStatus("g1", "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.GoalUpdateDescription("g1", "monitor + triage"); err != nil {
		t.Fatal(err)
	}
	// Children are created as goals with parent_id, which is the authoritative
	// edge; subtree is derived from it rather than appended to by hand.
	if err := store.GoalCreate("sub-goal-A", "first child", "g1", "goal"); err != nil {
		t.Fatal(err)
	}
	if err := store.GoalCreate("task-B", "second child", "g1", "goal"); err != nil {
		t.Fatal(err)
	}

	g, _ = store.GoalGet("g1")
	if g.Status != "done" || g.Description != "monitor + triage" {
		t.Errorf("after updates = %+v", g)
	}
	if len(g.Subtree) != 2 || g.Subtree[0] != "sub-goal-A" || g.Subtree[1] != "task-B" {
		t.Errorf("subtree = %v, want [sub-goal-A task-B] in order", g.Subtree)
	}

	// A child's own subtree is empty, and it is not confused for its parent's.
	child, err := store.GoalGet("sub-goal-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Subtree) != 0 {
		t.Errorf("child subtree = %v, want empty", child.Subtree)
	}
	if child.ParentID != "g1" {
		t.Errorf("child parent_id = %q, want g1", child.ParentID)
	}
}

// The subtree a model sees is derived from parent_id at read time, so it cannot
// drift from the relation the schema enforces — which is the whole reason the
// stored copy went away (adr/concept-consolidation.md C6).
func TestGoalSubtreeIsDerivedFromParentID(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.GoalCreate("root", "root goal", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if err := store.GoalCreate(id, "child "+id, "root", "goal"); err != nil {
			t.Fatal(err)
		}
	}
	// A grandchild belongs to its own parent, not the root.
	if err := store.GoalCreate("gc1", "grandchild", "c1", "goal"); err != nil {
		t.Fatal(err)
	}

	root, err := store.GoalGet("root")
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Subtree) != 3 {
		t.Errorf("root subtree = %v, want its 3 direct children only", root.Subtree)
	}
	for _, id := range root.Subtree {
		if id == "gc1" {
			t.Error("root subtree contains a grandchild; it must list direct children only")
		}
	}

	c1, err := store.GoalGet("c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(c1.Subtree) != 1 || c1.Subtree[0] != "gc1" {
		t.Errorf("c1 subtree = %v, want [gc1]", c1.Subtree)
	}

	children, err := store.GoalListChildren("root")
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 3 {
		t.Errorf("GoalListChildren = %v, want 3", children)
	}
	if got, _ := store.GoalListChildren("no-such-goal"); len(got) != 0 {
		t.Errorf("GoalListChildren(unknown) = %v, want empty", got)
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

func TestGoalDeleteTakesTheSubtree(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	// root ─ child ─ grandchild, plus an unrelated goal that must survive.
	for _, g := range []struct{ id, parent, ptype string }{
		{"root", "conv1", "conversation"},
		{"child", "root", "goal"},
		{"grandchild", "child", "goal"},
		{"other", "conv1", "conversation"},
	} {
		if err := store.GoalCreate(g.id, g.id, g.parent, g.ptype); err != nil {
			t.Fatal(err)
		}
	}

	ids, err := store.GoalDelete("root")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"root", "child", "grandchild"}
	if len(ids) != len(want) {
		t.Fatalf("deleted %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("deleted %v, want %v (goal first, then by depth)", ids, want)
		}
	}
	goals, err := store.GoalList()
	if err != nil {
		t.Fatal(err)
	}
	if len(goals) != 1 || goals[0].ID != "other" {
		t.Errorf("remaining goals = %+v, want only \"other\"", goals)
	}

	// A missing id is not an error; the caller decides what it means.
	if ids, err := store.GoalDelete("nope"); err != nil || len(ids) != 0 {
		t.Errorf("GoalDelete(missing) = %v, %v; want empty, nil", ids, err)
	}
}
