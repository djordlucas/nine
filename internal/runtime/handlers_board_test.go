package runtime_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/memory/memtest"
)

// Posting a task to a session's board wakes that session, so an idle
// orchestrator picks the task up instead of leaving it queued forever when no
// boardworker process is running (adr/orchestrator-board.md §2.3, wake
// primary). An operator post has no session to wake and must not.
func TestBoardPostWakesIdlePoster(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var lastReq atomic.Value // llm.Request
	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		calls.Add(1)
		lastReq.Store(req)
		return finalResp("dispatched"), nil
	})
	d, sock := startDaemon(t, makeFactory(provider), nil, nil)
	d.ConfigureMemory(store)
	c := dial(t, sock)

	id, err := c.NewConversation()
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}

	// An operator post wakes nobody: the poster is not a session.
	if _, err := c.BoardCreate("Operator task", "", "", 0, ""); err != nil {
		t.Fatalf("BoardCreate as operator: %v", err)
	}
	// A post to the conversation's board wakes it.
	task, err := c.BoardCreate("Review the queue", "look at it", "", 0, id)
	if err != nil {
		t.Fatalf("BoardCreate: %v", err)
	}
	if task.Status != "queued" {
		t.Fatalf("task status = %q, want queued", task.Status)
	}

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if calls.Load() < 1 {
		t.Fatal("posting to the session's board did not wake the idle session")
	}
	req, ok := lastReq.Load().(llm.Request)
	if !ok {
		t.Fatal("no LLM request captured")
	}
	found := false
	for _, m := range req.Messages {
		if strings.Contains(m.Text, "queued on your board") && strings.Contains(m.Text, "Review the queue") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the wake notice with the task title never reached the model; messages: %+v", req.Messages)
	}

	// The task itself is queued and readable on the board.
	list, err := c.BoardList("queued")
	if err != nil {
		t.Fatalf("BoardList: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("queued tasks = %d, want 2 (operator post + session post)", len(list))
	}
}
