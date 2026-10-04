package runtime_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/protocol"
	"nine/internal/runtime"
	"nine/internal/workflow"
)

// mockStore implements the queryBackend interface for testing.
// Each method delegates to an optional function field; nil means return zero/nil.
type mockStore struct {
	workflowListFn   func(string) ([]workflow.Workflow, error)
	workflowCancelFn func(string) (int, error)
	workflowFailFn   func(string, bool) (int, error)
}

func (m *mockStore) GoalList() ([]memory.Goal, error)                   { return nil, nil }
func (m *mockStore) GeneratedToolList() ([]memory.GeneratedTool, error) { return nil, nil }
func (m *mockStore) WorkflowList(id string) ([]workflow.Workflow, error) {
	if m.workflowListFn != nil {
		return m.workflowListFn(id)
	}
	return nil, nil
}
func (m *mockStore) WorkflowCancel(id string) (int, error) {
	if m.workflowCancelFn != nil {
		return m.workflowCancelFn(id)
	}
	return 0, nil
}
func (m *mockStore) WorkflowFail(id string, all bool) (int, error) {
	if m.workflowFailFn != nil {
		return m.workflowFailFn(id, all)
	}
	return 0, nil
}
func (m *mockStore) UserNotificationList(_ bool) ([]memory.UserNotification, error) {
	return nil, nil
}
func (m *mockStore) UserNotificationMarkSeen(_ string) error { return nil }
func (m *mockStore) SessionEventsByAgent(_ string) ([]memory.SessionEvent, error) {
	return nil, nil
}
func (m *mockStore) SessionEventsAfter(_ int64, _ int) ([]memory.SessionEvent, error) {
	return nil, nil
}
func (m *mockStore) EventCursorGet(_ string) (int64, error) { return 0, nil }
func (m *mockStore) EventCursorSet(_ string, _ int64) error { return nil }
func (m *mockStore) QueueMessage(_, _ string) error                { return nil }
func (m *mockStore) UnconsumedMessagesCount(_ string) (int, error) { return 0, nil }
func (m *mockStore) DrainQueuedMessage(_ string) (string, error)    { return "", nil }

// makeSimpleDaemon returns a running daemon backed by an always-answer provider.
// Callers must call d.Stop() and cancel() when done.
func makeSimpleDaemon(t *testing.T) (*runtime.Daemon, string, context.CancelFunc) {
	t.Helper()
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: "ok", StopReason: "end_turn"}, nil
	})
	factory := func(id string, _ runtime.RoleParams) *agent.Loop {
		return agent.NewLoop(agent.Config{
			SystemCore: "test",
			Priority:   llm.PriorityConversation,
		}, ninectx.New(ninectx.Config{Budget: 100_000}), llm.NewQueue(provider, 1), agent.New())
	}
	sock := tmpSock(t)
	d := runtime.New(sock, factory, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go d.Start(ctx) //nolint:errcheck
	waitForSock(t, sock)
	t.Cleanup(d.Stop)
	return d, sock, cancel
}

// --- WorkflowList ---

func TestListWorkflows(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	wf := workflow.Workflow{ID: "wf-1", Name: "alpha", Status: "active"}
	d.ConfigureMemory(&mockStore{
		workflowListFn: func(_ string) ([]workflow.Workflow, error) { return []workflow.Workflow{wf}, nil },
	})

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	raw, err := c.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if raw == "" || raw == "[]" {
		t.Errorf("ListWorkflows = %q, want non-empty workflow list", raw)
	}
}

func TestListWorkflowsNoStore(t *testing.T) {
	_, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	raw, err := c.ListWorkflows()
	if err != nil {
		t.Fatalf("ListWorkflows with no store: %v", err)
	}
	if raw != `[]` {
		t.Errorf("no store returned %q, want []", raw)
	}
}

func TestListWorkflowsStoreError(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	d.ConfigureMemory(&mockStore{
		workflowListFn: func(_ string) ([]workflow.Workflow, error) { return nil, fmt.Errorf("db down") },
	})

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	_, err = c.ListWorkflows()
	if err == nil {
		t.Fatal("expected error from store, got nil")
	}
}

// --- StopWorkflow ---

func TestStopWorkflow(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	var cancelledID string
	d.ConfigureMemory(&mockStore{
		workflowCancelFn: func(id string) (int, error) { cancelledID = id; return 1, nil },
	})

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.StopWorkflow("wf-abc"); err != nil {
		t.Fatalf("StopWorkflow: %v", err)
	}
	if cancelledID != "wf-abc" {
		t.Errorf("cancelledID = %q, want wf-abc", cancelledID)
	}
}

func TestStopWorkflowNoStore(t *testing.T) {
	_, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.StopWorkflow("wf-1"); err == nil {
		t.Error("expected error when store is not set, got nil")
	}
}

func TestStopWorkflowStoreError(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	d.ConfigureMemory(&mockStore{
		workflowCancelFn: func(_ string) (int, error) { return 0, fmt.Errorf("workflow not found") },
	})

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.StopWorkflow("wf-missing"); err == nil {
		t.Fatal("expected error from store, got nil")
	}
}

// --- FailWorkflow ---

func TestFailWorkflow(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	var failedID string
	var failedAll bool
	d.ConfigureMemory(&mockStore{
		workflowFailFn: func(id string, all bool) (int, error) {
			failedID = id
			failedAll = all
			return 1, nil
		},
	})

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.FailWorkflow("wf-xyz", false); err != nil {
		t.Fatalf("FailWorkflow(id): %v", err)
	}
	if failedID != "wf-xyz" || failedAll {
		t.Errorf("fail(id): id=%q all=%v, want id=wf-xyz all=false", failedID, failedAll)
	}
}

func TestFailWorkflowAll(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	var failedAll bool
	d.ConfigureMemory(&mockStore{
		workflowFailFn: func(_ string, all bool) (int, error) { failedAll = all; return 1, nil },
	})

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.FailWorkflow("", true); err != nil {
		t.Fatalf("FailWorkflow(all): %v", err)
	}
	if !failedAll {
		t.Error("all=false, want true when using FailWorkflow with all=true")
	}
}

func TestFailWorkflowNoStore(t *testing.T) {
	_, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.FailWorkflow("wf-1", false); err == nil {
		t.Error("expected error when store is not set, got nil")
	}
}

func TestFailWorkflowStoreError(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	d.ConfigureMemory(&mockStore{
		workflowFailFn: func(_ string, _ bool) (int, error) { return 0, fmt.Errorf("already closed") },
	})

	c, err := protocol.Connect(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close() //nolint:errcheck

	if err := c.FailWorkflow("wf-1", false); err == nil {
		t.Fatal("expected error from store, got nil")
	}
}

// --- concurrent dispatch sanity check ---

func TestWorkflowDispatchConcurrent(t *testing.T) {
	d, sock, cancel := makeSimpleDaemon(t)
	defer cancel()

	d.ConfigureMemory(&mockStore{
		workflowCancelFn: func(_ string) (int, error) { return 1, nil },
		workflowFailFn:   func(_ string, _ bool) (int, error) { return 1, nil },
	})

	// Exercise all three workflow messages concurrently using three clients.
	type result struct{ err error }
	results := make(chan result, 3)

	connect := func() *protocol.Client {
		c, err := protocol.Connect(sock)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		return c
	}

	// list
	go func() {
		c := connect()
		defer c.Close() //nolint:errcheck
		_, err := c.ListWorkflows()
		results <- result{err}
	}()

	// stop
	go func() {
		c := connect()
		defer c.Close() //nolint:errcheck
		results <- result{c.StopWorkflow("wf-1")}
	}()

	// fail
	go func() {
		c := connect()
		defer c.Close() //nolint:errcheck
		results <- result{c.FailWorkflow("wf-2", false)}
	}()

	deadline := time.After(2 * time.Second)
	for range 3 {
		select {
		case r := <-results:
			if r.err != nil {
				t.Errorf("concurrent dispatch error: %v", r.err)
			}
		case <-deadline:
			t.Fatal("timeout waiting for concurrent workflow dispatch results")
		}
	}
}

// The session-lifecycle half of queryBackend: inert stubs. The cascade is
// covered in internal/memory and the wiring in session_delete_test.go.
func (m *mockStore) SessionList() ([]memory.SessionSummary, error) { return nil, nil }
func (m *mockStore) SessionGet(string) (memory.SessionSummary, bool, error) {
	return memory.SessionSummary{}, false, nil
}
func (m *mockStore) SessionsReapable(time.Duration) ([]memory.SessionSummary, error) { return nil, nil }
func (m *mockStore) SessionDelete(string) (memory.SessionDeleteCounts, error) {
	return memory.SessionDeleteCounts{}, nil
}
func (m *mockStore) JobsRunning() ([]memory.Job, error) { return nil, nil }

// Goal and skill verbs: inert, so these mocks still satisfy queryBackend.
func (m *mockStore) GoalGet(string) (*memory.Goal, error)          { return nil, nil }
func (m *mockStore) GoalCreate(_, _, _, _ string) error            { return nil }
func (m *mockStore) GoalDelete(string) ([]string, error)           { return nil, nil }
func (m *mockStore) SkillList() ([]memory.Skill, error)            { return nil, nil }
