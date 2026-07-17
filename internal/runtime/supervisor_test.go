package runtime_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/runtime"
)

// fakeEventStore is an in-memory SupervisorStore for tests that don't need a
// real database: it records appended events and serves them by cursor.
type fakeEventStore struct {
	mu      sync.Mutex
	events  []memory.SessionEvent
	cursors map[string]int64
}

func newFakeEventStore() *fakeEventStore {
	return &fakeEventStore{cursors: map[string]int64{}}
}

func (f *fakeEventStore) SessionEventsAppend(evs []memory.SessionEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range evs {
		e.Seq = int64(len(f.events)) + 1
		f.events = append(f.events, e)
	}
	return nil
}

func (f *fakeEventStore) SessionEventsAfter(after int64, limit int) ([]memory.SessionEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []memory.SessionEvent
	for _, e := range f.events {
		if e.Seq <= after {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeEventStore) EventCursorGet(id string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursors[id], nil
}

func (f *fakeEventStore) EventCursorSet(id string, seq int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors[id] = seq
	return nil
}

// eventsOfKind decodes journaled supervisor events of the given kind.
func (f *fakeEventStore) eventsOfKind(kind runtime.EventKind) []runtime.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []runtime.Event
	for _, e := range f.events {
		if e.Type != "supervisor" {
			continue
		}
		var p struct {
			Kind    runtime.EventKind `json:"kind"`
			AgentID string            `json:"agent_id"`
			Payload string            `json:"payload"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Kind == kind {
			out = append(out, runtime.Event{Kind: p.Kind, AgentID: p.AgentID, Payload: p.Payload})
		}
	}
	return out
}

func (f *fakeEventStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

// TestSupervisorInertWithoutStore: an unattached supervisor never panics and Run
// exits cleanly (used by wiring-only tests that don't exercise events).
func TestSupervisorInertWithoutStore(t *testing.T) {
	s := runtime.NewSupervisor(0)
	if s == nil {
		t.Fatal("NewSupervisor(0) returned nil")
	}
	s.Post(runtime.Event{Kind: runtime.EventAgentCompletes, AgentID: "test"}) // no-op, must not panic
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestSupervisorJournalsAndConsumes: posted events are durably journaled and the
// supervisor consumes them via its cursor-backed subscription.
func TestSupervisorJournalsAndConsumes(t *testing.T) {
	store := newFakeEventStore()
	s := runtime.NewSupervisor(16)
	s.Attach(store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	events := []runtime.Event{
		{Kind: runtime.EventAgentCompletes, AgentID: "a1"},
		{Kind: runtime.EventGoalStalls, AgentID: "a2"},
		{Kind: runtime.EventGapReported, AgentID: "a3", Payload: "no rename tool"},
	}
	for _, e := range events {
		s.Post(e)
	}

	// All three are durably journaled (synchronous append on Post).
	if store.count() != 3 {
		t.Fatalf("journaled %d events, want 3", store.count())
	}

	// The subscription consumes them: its cursor advances to the last event.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if c, _ := store.EventCursorGet("supervisor"); c == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if c, _ := store.EventCursorGet("supervisor"); c != 3 {
		t.Errorf("supervisor cursor = %d, want 3 (all consumed)", c)
	}
}

// TestSupervisorEventsByKind: journaled events are recoverable by kind.
func TestSupervisorEventsByKind(t *testing.T) {
	store := newFakeEventStore()
	s := runtime.NewSupervisor(16)
	s.Attach(store)

	s.Post(runtime.Event{Kind: runtime.EventAgentCompletes, AgentID: "a1"})
	s.Post(runtime.Event{Kind: runtime.EventGoalStalls, AgentID: "a2"})
	s.Post(runtime.Event{Kind: runtime.EventAgentCompletes, AgentID: "a3"})

	if got := store.eventsOfKind(runtime.EventAgentCompletes); len(got) != 2 {
		t.Fatalf("EventAgentCompletes count = %d, want 2", len(got))
	}
	stalls := store.eventsOfKind(runtime.EventGoalStalls)
	if len(stalls) != 1 || stalls[0].AgentID != "a2" {
		t.Fatalf("EventGoalStalls = %+v, want one for a2", stalls)
	}
}
