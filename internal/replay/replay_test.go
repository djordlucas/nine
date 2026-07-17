package replay_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/memory"
	"nine/internal/replay"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// oneToolTurn is a synthetic journal for a turn that calls "echo" then answers.
func oneToolTurn() []memory.SessionEvent {
	return []memory.SessionEvent{
		{Seq: 1, Turn: 1, Type: "turn_start", Payload: mustJSON(map[string]any{"input": "what is it?", "trigger": "user"})},
		{Seq: 2, Turn: 1, Type: "llm_request", Payload: mustJSON(map[string]any{"llm_call_n": 1})},
		{Seq: 3, Turn: 1, Type: "llm_response", Payload: mustJSON(map[string]any{
			"text": "let me check", "stop_reason": "tool_use",
			"tool_calls": []map[string]any{{"ID": "c1", "Name": "echo", "Input": json.RawMessage(`{"v":"hi"}`)}},
		})},
		{Seq: 4, Turn: 1, Type: "tool_start", Payload: mustJSON(map[string]any{"name": "echo"})},
		{Seq: 5, Turn: 1, Type: "tool_end", Payload: mustJSON(map[string]any{"name": "echo", "output": "echoed:hi"})},
		{Seq: 6, Turn: 1, Type: "llm_response", Payload: mustJSON(map[string]any{"text": "the answer is hi", "stop_reason": "end_turn"})},
		{Seq: 7, Turn: 1, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "the answer is hi"})},
	}
}

func TestFromEventsGroups(t *testing.T) {
	rec, err := replay.FromEvents(oneToolTurn())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Turns) != 1 {
		t.Fatalf("got %d turns, want 1", len(rec.Turns))
	}
	if rec.Turns[0].Input != "what is it?" || rec.Turns[0].Result != "the answer is hi" {
		t.Errorf("turn = %+v", rec.Turns[0])
	}
}

func TestSessionReplaysDeterministically(t *testing.T) {
	rec, err := replay.FromEvents(oneToolTurn())
	if err != nil {
		t.Fatal(err)
	}

	// Two independent replays must produce identical answers with no live calls.
	a1, err := replay.Session(context.Background(), rec)
	if err != nil {
		t.Fatalf("replay 1: %v", err)
	}
	a2, err := replay.Session(context.Background(), rec)
	if err != nil {
		t.Fatalf("replay 2: %v", err)
	}
	if len(a1) != 1 || a1[0] != "the answer is hi" {
		t.Fatalf("replay answers = %v, want [the answer is hi]", a1)
	}
	if len(a2) != len(a1) || a2[0] != a1[0] {
		t.Errorf("nondeterministic replay: %v vs %v", a1, a2)
	}

	// The replayed answer matches the recorded golden result.
	if a1[0] != rec.Turns[0].Result {
		t.Errorf("replay %q != recorded golden %q", a1[0], rec.Turns[0].Result)
	}
}

// TestSessionMultiTurn checks responses/tool outputs are consumed in order
// across turn boundaries.
func TestSessionMultiTurn(t *testing.T) {
	events := []memory.SessionEvent{
		{Seq: 1, Turn: 1, Type: "turn_start", Payload: mustJSON(map[string]any{"input": "q1"})},
		{Seq: 2, Turn: 1, Type: "llm_response", Payload: mustJSON(map[string]any{"text": "a1", "stop_reason": "end_turn"})},
		{Seq: 3, Turn: 1, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "a1"})},
		{Seq: 4, Turn: 2, Type: "turn_start", Payload: mustJSON(map[string]any{"input": "q2"})},
		{Seq: 5, Turn: 2, Type: "llm_response", Payload: mustJSON(map[string]any{"text": "a2", "stop_reason": "end_turn"})},
		{Seq: 6, Turn: 2, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "a2"})},
	}
	rec, err := replay.FromEvents(events)
	if err != nil {
		t.Fatal(err)
	}
	answers, err := replay.Session(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 2 || answers[0] != "a1" || answers[1] != "a2" {
		t.Errorf("answers = %v, want [a1 a2]", answers)
	}
}
