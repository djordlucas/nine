package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func TestSessionEventsAppendAndRead(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Interleave two agents to prove reads are filtered and seq-ordered.
	batch := []memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_start", Payload: json.RawMessage(`{"input":"hi"}`)},
		{AgentID: "b", Turn: 1, SpanID: "t1", Type: "turn_start"},
		{AgentID: "a", Turn: 1, SpanID: "t1.llm1", ParentSpanID: "t1", Type: "llm_request", Payload: json.RawMessage(`{"llm_call_n":1}`)},
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_end", Payload: json.RawMessage(`{"result":"done"}`)},
	}
	if err := store.SessionEventsAppend(batch); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Empty batch is a no-op.
	if err := store.SessionEventsAppend(nil); err != nil {
		t.Fatalf("empty append: %v", err)
	}

	got, err := store.SessionEventsByAgent("a")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 events for agent a, got %d", len(got))
	}

	// seq is monotonic and assigned by the DB.
	if got[0].Seq >= got[1].Seq || got[1].Seq >= got[2].Seq {
		t.Errorf("seq not monotonic: %d %d %d", got[0].Seq, got[1].Seq, got[2].Seq)
	}
	if got[0].Type != "turn_start" || got[1].Type != "llm_request" || got[2].Type != "turn_end" {
		t.Errorf("unexpected type order: %s %s %s", got[0].Type, got[1].Type, got[2].Type)
	}
	// parent_span_id round-trips (NULL → "" for roots, set for children).
	if got[0].ParentSpanID != "" {
		t.Errorf("turn root parent = %q, want empty", got[0].ParentSpanID)
	}
	if got[1].ParentSpanID != "t1" {
		t.Errorf("llm_request parent = %q, want t1", got[1].ParentSpanID)
	}
	// payload round-trips as JSON; TS is DB-assigned.
	if string(got[0].Payload) == "" || got[0].TS.IsZero() {
		t.Errorf("payload/ts not populated: payload=%q ts=%v", got[0].Payload, got[0].TS)
	}
}

func TestSessionEventsScrubByTurns(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Agent "a" spans turns 1..5; agent "b" has a single turn that must survive.
	var batch []memory.SessionEvent
	for turn := 1; turn <= 5; turn++ {
		batch = append(batch,
			memory.SessionEvent{AgentID: "a", Turn: turn, SpanID: "t", Type: "turn_start"},
			memory.SessionEvent{AgentID: "a", Turn: turn, SpanID: "t", Type: "turn_end"},
		)
	}
	batch = append(batch, memory.SessionEvent{AgentID: "b", Turn: 1, SpanID: "t", Type: "turn_start"})
	if err := store.SessionEventsAppend(batch); err != nil {
		t.Fatal(err)
	}

	// Keep the last 2 turns per agent → agent a drops turns 1..3 (6 rows).
	deleted, err := store.SessionEventsScrub(2, 0)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	if deleted != 6 {
		t.Errorf("deleted = %d, want 6", deleted)
	}

	got, _ := store.SessionEventsByAgent("a")
	if len(got) != 4 {
		t.Fatalf("agent a kept %d events, want 4 (turns 4,5)", len(got))
	}
	for _, e := range got {
		if e.Turn < 4 {
			t.Errorf("turn %d survived the scrub, want only 4,5", e.Turn)
		}
	}
	// Agent b (below the window) is untouched.
	if b, _ := store.SessionEventsByAgent("b"); len(b) != 1 {
		t.Errorf("agent b kept %d events, want 1", len(b))
	}
}

func TestSessionEventsScrubByAge(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t", Type: "turn_start"},
		{AgentID: "a", Turn: 1, SpanID: "t", Type: "turn_end"},
	}); err != nil {
		t.Fatal(err)
	}

	// A generous age bound deletes nothing (rows were just written).
	if n, err := store.SessionEventsScrub(0, 24*time.Hour); err != nil || n != 0 {
		t.Fatalf("scrub(0,24h) = %d, %v; want 0, nil", n, err)
	}
	// A sub-millisecond bound is already in the past for rows written a moment ago.
	n, err := store.SessionEventsScrub(0, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("age scrub deleted %d, want 2", n)
	}
	if got, _ := store.SessionEventsByAgent("a"); len(got) != 0 {
		t.Errorf("age scrub left %d events, want 0", len(got))
	}
}

func TestSessionEventsScrubDisabled(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SessionEventsAppend([]memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t", Type: "turn_start"},
	}); err != nil {
		t.Fatal(err)
	}
	// Both bounds disabled → no-op.
	if n, err := store.SessionEventsScrub(0, 0); err != nil || n != 0 {
		t.Fatalf("scrub(0,0) = %d, %v; want 0, nil", n, err)
	}
	if got, _ := store.SessionEventsByAgent("a"); len(got) != 1 {
		t.Errorf("disabled scrub deleted rows: kept %d, want 1", len(got))
	}
}

func TestLatestTurnResult(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// No turns yet → empty, no error.
	if got, err := store.LatestTurnResult("a"); err != nil || got != "" {
		t.Fatalf("no turns: got %q err %v, want empty", got, err)
	}

	batch := []memory.SessionEvent{
		{AgentID: "a", Turn: 1, SpanID: "t1", Type: "turn_end", Payload: json.RawMessage(`{"result":"first answer"}`)},
		{AgentID: "a", Turn: 2, SpanID: "t2", Type: "llm_request", Payload: json.RawMessage(`{"llm_call_n":1}`)},
		{AgentID: "a", Turn: 2, SpanID: "t2", Type: "turn_end", Payload: json.RawMessage(`{"result":"latest answer"}`)},
		{AgentID: "b", Turn: 1, SpanID: "t1", Type: "turn_end", Payload: json.RawMessage(`{"result":"other agent"}`)},
	}
	if err := store.SessionEventsAppend(batch); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Returns the most recent turn_end's result, filtered by agent.
	if got, err := store.LatestTurnResult("a"); err != nil || got != "latest answer" {
		t.Fatalf("agent a: got %q err %v, want \"latest answer\"", got, err)
	}
	if got, _ := store.LatestTurnResult("b"); got != "other agent" {
		t.Errorf("agent b: got %q, want \"other agent\"", got)
	}
}
