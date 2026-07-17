package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

// Store-level round-trip for the workflow methods (the sqlWorkflowRepo adapter
// behind workflow.Service). Domain rules are unit-tested in internal/workflow;
// this verifies persistence through the real Postgres store.
func TestWorkflowCreateGetListUpdate(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	cr, err := store.WorkflowCreate("agent-1", "ship feature", []string{"design", "build"})
	if err != nil {
		t.Fatal(err)
	}
	if cr.ID == "" || len(cr.Steps) != 2 {
		t.Fatalf("WorkflowCreate = %+v, want id + 2 steps", cr)
	}

	wf, err := store.WorkflowGet(cr.ID)
	if err != nil || wf == nil {
		t.Fatalf("WorkflowGet: wf=%v err=%v", wf, err)
	}
	if wf.Name != "ship feature" || wf.Status != "active" || wf.AgentID != "agent-1" {
		t.Errorf("workflow = %+v", wf)
	}

	// Listed for its agent.
	list, err := store.WorkflowList("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != cr.ID {
		t.Errorf("WorkflowList(agent-1) = %+v", list)
	}

	// Completing every step auto-closes the workflow as done (no explicit close).
	for _, st := range cr.Steps {
		if _, err := store.WorkflowUpdate(cr.ID, st.ID, "done", "ok", ""); err != nil {
			t.Fatalf("WorkflowUpdate(%s): %v", st.ID, err)
		}
	}
	wf, _ = store.WorkflowGet(cr.ID)
	if wf.Status != "done" {
		t.Errorf("after all steps done, workflow status = %q, want done", wf.Status)
	}
}

func TestWorkflowCancelAndScrub(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := store.WorkflowCreate("agent-2", "wf", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	// Mark a step running, then Cancel: pending→skipped, running→failed, wf→cancelled.
	if _, err := store.WorkflowUpdate(cr.ID, cr.Steps[0].ID, "running", "", ""); err != nil {
		t.Fatal(err)
	}
	n, err := store.WorkflowCancel(cr.ID)
	if err != nil || n == 0 {
		t.Fatalf("WorkflowCancel = %d, err=%v", n, err)
	}
	wf, _ := store.WorkflowGet(cr.ID)
	if wf.Status != "cancelled" {
		t.Errorf("cancelled workflow status = %q", wf.Status)
	}
}
