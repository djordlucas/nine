package runtime_test

import (
	"sync"
	"sync/atomic"
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

// collectingStore is a minimal sessionEventStore that records appended events.
type collectingStore struct {
	mu     sync.Mutex
	events []memory.SessionEvent
}

func (c *collectingStore) SessionEventsAppend(evs []memory.SessionEvent) error {
	c.mu.Lock()
	c.events = append(c.events, evs...)
	c.mu.Unlock()
	return nil
}

func (c *collectingStore) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// TestEventSinkNotifiesOnFlush: the sink calls onFlush after writing a batch, so
// subscribers wake on newly journaled events.
func TestEventSinkNotifiesOnFlush(t *testing.T) {
	store := &collectingStore{}
	var flushes atomic.Int64
	sink := runtime.NewSQLEventSink(store, func() { flushes.Add(1) })

	sink.Append(memory.SessionEvent{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_end"})
	// Close drains and flushes the buffered event before returning.
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	if store.count() != 1 {
		t.Errorf("stored %d events, want 1", store.count())
	}
	if flushes.Load() == 0 {
		t.Error("onFlush was never called after a batch write")
	}
}

// An event appended after Close is dropped, not sent on the closed channel: a
// process session's turn can still be finishing when the sink closes. Close
// is idempotent.
func TestSQLEventSinkAppendAfterCloseIsDropped(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	sink := runtime.NewSQLEventSinkForTest(store)
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	sink.Append(memory.SessionEvent{AgentID: "late", Type: "turn_end"})
	if err := sink.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
