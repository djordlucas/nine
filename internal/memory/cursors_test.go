package memory_test

import (
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func TestSessionEventsAfterAndCursor(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_start"},
		{AgentID: "b", Turn: 1, SpanID: "t1", Type: "turn_start"},
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_end"},
	}); err != nil {
		t.Fatal(err)
	}

	// From the start, forward across all agents in seq order.
	all, err := store.SessionEventsAfter(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("SessionEventsAfter(0) returned %d, want 3", len(all))
	}
	if all[0].Seq >= all[1].Seq || all[1].Seq >= all[2].Seq {
		t.Errorf("not seq-ordered: %d %d %d", all[0].Seq, all[1].Seq, all[2].Seq)
	}

	// A cursor past the first event yields only later ones; limit is honored.
	rest, err := store.SessionEventsAfter(all[0].Seq, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].Seq != all[1].Seq {
		t.Errorf("SessionEventsAfter(seq0, 1) = %+v, want just the second event", rest)
	}

	// Cursor round-trips; unknown subscriber reads 0.
	if got, _ := store.EventCursorGet("sub-x"); got != 0 {
		t.Errorf("unknown cursor = %d, want 0", got)
	}
	if err := store.EventCursorSet("sub-x", all[2].Seq); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.EventCursorGet("sub-x"); got != all[2].Seq {
		t.Errorf("cursor = %d, want %d", got, all[2].Seq)
	}
	// Upsert updates in place.
	if err := store.EventCursorSet("sub-x", all[2].Seq+5); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.EventCursorGet("sub-x"); got != all[2].Seq+5 {
		t.Errorf("cursor after upsert = %d, want %d", got, all[2].Seq+5)
	}
}
