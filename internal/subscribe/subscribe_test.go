package subscribe_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/subscribe"
)

// recorder is a Handler that records the seqs it processes, filtered to types.
type recorder struct {
	id    string
	types []string
	mu    sync.Mutex
	seqs  []int64
}

func (r *recorder) ID() string      { return r.id }
func (r *recorder) Types() []string { return r.types }
func (r *recorder) Handle(_ context.Context, ev memory.SessionEvent) error {
	r.mu.Lock()
	r.seqs = append(r.seqs, ev.Seq)
	r.mu.Unlock()
	return nil
}
func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.seqs)
}
func (r *recorder) snapshot() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int64, len(r.seqs))
	copy(out, r.seqs)
	return out
}

// waitFor polls until fn() is true or the deadline passes.
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

func TestSubscriptionDeliversInOrderAndFilters(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_start"},
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "tool_end"},
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_end"},
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "tool_end"},
	}); err != nil {
		t.Fatal(err)
	}

	// Only tool_end events; expect 2 of the 4.
	h := &recorder{id: "sub-filter", types: []string{"tool_end"}}
	sub := subscribe.New(store, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sub.Run(ctx)
	sub.Notify()

	waitFor(t, func() bool { return h.count() == 2 })

	got := h.snapshot()
	if !(got[0] < got[1]) {
		t.Errorf("out of order: %v", got)
	}
	// Cursor advanced past every event (including filtered-out ones).
	waitFor(t, func() bool {
		c, _ := store.EventCursorGet("sub-filter")
		last, _ := store.SessionEventsAfter(0, 10)
		return c == last[len(last)-1].Seq
	})
}

func TestSubscriptionResumesFromCursorAfterRestart(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_start"},
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_end"},
	}); err != nil {
		t.Fatal(err)
	}

	// First run consumes the two existing events, then we stop it (the "restart").
	h1 := &recorder{id: "sub-resume"}
	sub1 := subscribe.New(store, h1)
	ctx1, cancel1 := context.WithCancel(context.Background())
	go sub1.Run(ctx1)
	sub1.Notify()
	waitFor(t, func() bool { return h1.count() == 2 })
	cancel1()

	// New events arrive while "down".
	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a", Turn: 2, SpanID: "t2", Type: "turn_start"},
		{AgentID: "a", Turn: 2, SpanID: "t2", Type: "turn_end"},
	}); err != nil {
		t.Fatal(err)
	}

	// A fresh subscription with the same ID must resume from the cursor and
	// process ONLY the two new events, not reprocess the old ones.
	h2 := &recorder{id: "sub-resume"}
	sub2 := subscribe.New(store, h2)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go sub2.Run(ctx2)
	sub2.Notify()
	waitFor(t, func() bool { return h2.count() == 2 })

	// Give it a beat to ensure it doesn't also pick up the old events.
	time.Sleep(50 * time.Millisecond)
	if h2.count() != 2 {
		t.Errorf("resumed subscriber processed %d events, want exactly the 2 new ones", h2.count())
	}
	// The resumed seqs are strictly greater than the first run's last seq.
	firstLast := h1.snapshot()[1]
	for _, seq := range h2.snapshot() {
		if seq <= firstLast {
			t.Errorf("reprocessed old event seq %d (cursor was %d)", seq, firstLast)
		}
	}
}
