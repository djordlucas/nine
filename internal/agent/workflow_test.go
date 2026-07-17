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

func newWorkflowStore(t *testing.T) *memory.Store {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// --- RegisterWorkflowTools tests ---

func TestWireWorkflowCreate(t *testing.T) {
	store := newWorkflowStore(t)
	d := agent.New()
	agent.RegisterWorkflowTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "workflow_create",
		json.RawMessage(`{"name":"my plan","steps":["step A","step B"]}`))
	if err != nil {
		t.Fatalf("workflow_create: %v", err)
	}

	var out struct {
		ID    string `json:"id"`
		Steps []struct {
			ID     string `json:"id"`
			Label  string `json:"label"`
			Status string `json:"status"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID == "" {
		t.Error("expected non-empty workflow ID")
	}
	if len(out.Steps) != 2 {
		t.Fatalf("steps count = %d, want 2", len(out.Steps))
	}
	if out.Steps[0].Label != "step A" || out.Steps[1].Label != "step B" {
		t.Errorf("step labels wrong: %+v", out.Steps)
	}
	for _, s := range out.Steps {
		if s.Status != "pending" {
			t.Errorf("step %s status = %q, want pending", s.ID, s.Status)
		}
	}
}

func TestWireWorkflowCreateInjectsAgentID(t *testing.T) {
	store := newWorkflowStore(t)
	d := agent.New()
	agent.RegisterWorkflowTools(d, "my-agent-id", store, nil)

	_, err := d.Dispatch(context.Background(), "workflow_create",
		json.RawMessage(`{"name":"test","steps":["step"]}`))
	if err != nil {
		t.Fatalf("workflow_create: %v", err)
	}

	workflows, err := store.WorkflowList("my-agent-id")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(workflows) != 1 {
		t.Fatalf("got %d workflows, want 1", len(workflows))
	}
	if workflows[0].AgentID != "my-agent-id" {
		t.Errorf("agent_id = %q, want 'my-agent-id'", workflows[0].AgentID)
	}
}

// TestWireWorkflowToolsAgentIDIsolation verifies that two Wires with different
// agentIDs bind their workflows to the correct agent.
func TestWireWorkflowToolsAgentIDIsolation(t *testing.T) {
	store := newWorkflowStore(t)
	d1, d2 := agent.New(), agent.New()
	agent.RegisterWorkflowTools(d1, "agent-alpha", store, nil)
	agent.RegisterWorkflowTools(d2, "agent-beta", store, nil)

	for _, d := range []*agent.Dispatcher{d1, d2} {
		_, err := d.Dispatch(context.Background(), "workflow_create",
			json.RawMessage(`{"name":"test","steps":["s"]}`))
		if err != nil {
			t.Fatalf("workflow_create: %v", err)
		}
	}

	alphaWFs, err := store.WorkflowList("agent-alpha")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	betaWFs, err := store.WorkflowList("agent-beta")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(alphaWFs) != 1 {
		t.Errorf("agent-alpha workflows = %d, want 1", len(alphaWFs))
	}
	if len(betaWFs) != 1 {
		t.Errorf("agent-beta workflows = %d, want 1", len(betaWFs))
	}
}

func TestWireWorkflowGet(t *testing.T) {
	store := newWorkflowStore(t)
	result, err := store.WorkflowCreate("agent-1", "test wf", []string{"solo"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	d := agent.New()
	agent.RegisterWorkflowTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "workflow_get",
		json.RawMessage(fmt.Sprintf(`{"workflow_id":%q}`, result.ID)))
	if err != nil {
		t.Fatalf("workflow_get: %v", err)
	}
	if !strings.Contains(res.Output, result.ID) {
		t.Errorf("output = %q, want to contain workflow ID", res.Output)
	}
	if !strings.Contains(res.Output, "test wf") {
		t.Errorf("output = %q, want to contain 'test wf'", res.Output)
	}
}

func TestWireWorkflowGetInvalidJSON(t *testing.T) {
	store := newWorkflowStore(t)
	d := agent.New()
	agent.RegisterWorkflowTools(d, "a", store, nil)

	_, err := d.Dispatch(context.Background(), "workflow_get", json.RawMessage(`{bad}`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestWireWorkflowUpdate(t *testing.T) {
	store := newWorkflowStore(t)
	result, err := store.WorkflowCreate("agent-1", "update test", []string{"step"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wfID := result.ID
	stepID := result.Steps[0].ID

	d := agent.New()
	agent.RegisterWorkflowTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "workflow_update",
		json.RawMessage(fmt.Sprintf(`{"workflow_id":%q,"step_id":%q,"status":"done","result":"great"}`, wfID, stepID)))
	if err != nil {
		t.Fatalf("workflow_update: %v", err)
	}
	if !strings.Contains(res.Output, `"ok"`) {
		t.Errorf("output = %q, want to contain ok", res.Output)
	}
	wf, err := store.WorkflowGet(wfID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wf.Steps[0].Status != "done" {
		t.Errorf("step status = %q, want done", wf.Steps[0].Status)
	}
	if wf.Steps[0].Result != "great" {
		t.Errorf("step result = %q, want 'great'", wf.Steps[0].Result)
	}
}

func TestWireWorkflowList(t *testing.T) {
	store := newWorkflowStore(t)
	if _, err := store.WorkflowCreate("agent-1", "alpha", []string{"s"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := store.WorkflowCreate("agent-2", "beta", []string{"s"}); err != nil {
		t.Fatalf("create: %v", err)
	}

	d := agent.New()
	agent.RegisterWorkflowTools(d, "agent-1", store, nil)

	res, err := d.Dispatch(context.Background(), "workflow_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("workflow_list: %v", err)
	}
	// workflow_list passes agentID="agent-1" — only alpha matches.
	if !strings.Contains(res.Output, "alpha") {
		t.Errorf("output = %q, want to contain 'alpha' (agent-1's workflow)", res.Output)
	}
	if strings.Contains(res.Output, "beta") {
		t.Errorf("output = %q, should not contain 'beta' (other agent's workflow)", res.Output)
	}
}

func TestWireWorkflowListNoArgs(t *testing.T) {
	store := newWorkflowStore(t)
	d := agent.New()
	agent.RegisterWorkflowTools(d, "agent-X", store, nil)

	_, err := d.Dispatch(context.Background(), "workflow_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("workflow_list: %v", err)
	}
}

func TestWireWorkflowRetryStep(t *testing.T) {
	store := newWorkflowStore(t)
	result, err := store.WorkflowCreate("agent-1", "retry test", []string{"run tests"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wfID := result.ID
	stepID := result.Steps[0].ID
	// Mark as failed first.
	store.WorkflowUpdate(wfID, stepID, "failed", "", "it broke") //nolint:errcheck

	var spawnedTask string
	spawnFn := func(_ context.Context, task, _ string) (string, error) {
		spawnedTask = task
		return "tests passed on retry", nil
	}

	d := agent.New()
	agent.RegisterWorkflowTools(d, "agent-1", store, spawnFn)

	res, err := d.Dispatch(context.Background(), "workflow_retry_step",
		json.RawMessage(fmt.Sprintf(`{"workflow_id":%q,"step_id":%q}`, wfID, stepID)))
	if err != nil {
		t.Fatalf("workflow_retry_step: %v", err)
	}
	if spawnedTask != "run tests" {
		t.Errorf("spawnedTask = %q, want 'run tests'", spawnedTask)
	}
	if !strings.Contains(res.Output, "tests passed on retry") {
		t.Errorf("output = %q, want to contain 'tests passed on retry'", res.Output)
	}
	wf, err := store.WorkflowGet(wfID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if wf.Steps[0].Status != "done" {
		t.Errorf("step status = %q, want done after retry", wf.Steps[0].Status)
	}
}

func TestWireWorkflowRetryStepSpawnError(t *testing.T) {
	store := newWorkflowStore(t)
	result, err := store.WorkflowCreate("agent-1", "error retry", []string{"failing step"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wfID := result.ID
	stepID := result.Steps[0].ID
	store.WorkflowUpdate(wfID, stepID, "failed", "", "it broke") //nolint:errcheck

	spawnFn := func(_ context.Context, _, _ string) (string, error) {
		return "", fmt.Errorf("spawn failed")
	}

	d := agent.New()
	agent.RegisterWorkflowTools(d, "agent-1", store, spawnFn)

	_, err = d.Dispatch(context.Background(), "workflow_retry_step",
		json.RawMessage(fmt.Sprintf(`{"workflow_id":%q,"step_id":%q}`, wfID, stepID)))
	if err == nil {
		t.Fatal("expected error when spawn fails, got nil")
	}
	if !strings.Contains(err.Error(), "spawn failed") {
		t.Errorf("error = %v, want to mention 'spawn failed'", err)
	}
}

func TestWireWorkflowRetryStepInvalidJSON(t *testing.T) {
	store := newWorkflowStore(t)
	d := agent.New()
	agent.RegisterWorkflowTools(d, "a", store, func(_ context.Context, _, _ string) (string, error) {
		return "ok", nil
	})

	_, err := d.Dispatch(context.Background(), "workflow_retry_step", json.RawMessage(`{bad}`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestWireWorkflowCreateInvalidJSON(t *testing.T) {
	store := newWorkflowStore(t)
	d := agent.New()
	agent.RegisterWorkflowTools(d, "a", store, nil)

	_, err := d.Dispatch(context.Background(), "workflow_create", json.RawMessage(`{bad}`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestWireWorkflowUpdateInvalidJSON(t *testing.T) {
	store := newWorkflowStore(t)
	d := agent.New()
	agent.RegisterWorkflowTools(d, "a", store, nil)

	_, err := d.Dispatch(context.Background(), "workflow_update", json.RawMessage(`{bad}`))
	if err == nil {
		t.Error("expected error for invalid JSON, got nil")
	}
}

func TestWireWorkflowContextPropagated(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")

	store := newWorkflowStore(t)
	result, err := store.WorkflowCreate("a", "ctx test", []string{"step"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	wfID := result.ID
	stepID := result.Steps[0].ID
	store.WorkflowUpdate(wfID, stepID, "failed", "", "broke") //nolint:errcheck

	var spawnCtx context.Context
	d := agent.New()
	agent.RegisterWorkflowTools(d, "a", store, func(c context.Context, _, _ string) (string, error) {
		spawnCtx = c
		return "ok", nil
	})

	d.Dispatch(ctx, "workflow_retry_step", //nolint:errcheck
		json.RawMessage(fmt.Sprintf(`{"workflow_id":%q,"step_id":%q}`, wfID, stepID)))

	if spawnCtx == nil || spawnCtx.Value(ctxKey{}) != "marker" {
		t.Error("context not propagated to spawnFn in workflow_retry_step")
	}
}
