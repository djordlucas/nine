package runtime_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

// countingHandler records how many events it has processed.
type countingHandler struct {
	mu sync.Mutex
	n  int
}

func (h *countingHandler) ID() string      { return "counting" }
func (h *countingHandler) Types() []string { return []string{"turn_end"} }
func (h *countingHandler) Handle(_ context.Context, _ memory.SessionEvent) error {
	h.mu.Lock()
	h.n++
	h.mu.Unlock()
	return nil
}
func (h *countingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// TestDaemonHostsAndWakesSubscriber: a subscriber registered on the daemon is
// started by Start and woken by NotifySubscribers to drain new journal events.
func TestDaemonHostsAndWakesSubscriber(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	sock := tmpSock(t)
	d := runtime.New(sock, func(string, runtime.RoleParams) *agent.Loop { return nil }, nil, nil)
	d.ConfigureMemory(store)

	h := &countingHandler{}
	d.AddSubscriber(h)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Start(ctx) //nolint:errcheck
	waitForSock(t, sock)
	t.Cleanup(d.Stop)

	// Journal two turn_end events and one filtered-out event, then wake the
	// subscriber (as the sink would after a flush).
	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_end"},
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "tool_end"},
		{AgentID: "a", Turn: 2, SpanID: "t2", Type: "turn_end"},
	}); err != nil {
		t.Fatal(err)
	}
	d.NotifySubscribers()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && h.count() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if h.count() != 2 {
		t.Errorf("handler processed %d turn_end events, want 2", h.count())
	}
}
