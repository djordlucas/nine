package runtime_test

import (
	"encoding/json"
	"testing"

	"nine/internal/memory"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

// TestJournalReplayReattach verifies the journal-backed reattach snapshot
// (docs/event-log.md §7.6): with an empty in-memory ring — the state after a
// daemon restart — attach sources the last turn's tool trajectory and final
// response from the durable journal.
func TestJournalReplayReattach(t *testing.T) {
	mustJSON := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	events := []memory.SessionEvent{
		// An older turn that must be excluded (only the last turn replays).
		{Seq: 1, Turn: 1, Type: "tool_start", Payload: mustJSON(map[string]any{"name": "old_tool"})},
		{Seq: 2, Turn: 1, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "old answer"})},
		// The last turn.
		{Seq: 3, Turn: 2, Type: "turn_start", Payload: mustJSON(map[string]any{"input": "hi", "trigger": "user"})},
		{Seq: 4, Turn: 2, Type: "tool_start", Payload: mustJSON(map[string]any{"name": "echo", "input": json.RawMessage(`{"v":1}`)})},
		{Seq: 5, Turn: 2, Type: "tool_end", Payload: mustJSON(map[string]any{"name": "echo", "output": "1"})},
		{Seq: 6, Turn: 2, Type: "sub_agent_start", Payload: mustJSON(map[string]any{"sub_id": "s1", "task": "dig"})},
		{Seq: 7, Turn: 2, Type: "sub_agent_end", Payload: mustJSON(map[string]any{"sub_id": "s1", "status": "done"})},
		{Seq: 8, Turn: 2, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "final answer"})},
	}

	d := runtime.New("/tmp/unused.sock", nil, nil, nil)
	d.ConfigureMemory(&mockGoalStore{events: events})

	msgs, response := d.JournalReplayForTest("agent-x")

	if response != "final answer" {
		t.Errorf("response = %q, want last turn's result", response)
	}
	// Only the last turn's replayable events, in order, no turn_start/turn_end.
	wantTypes := []protocol.MsgType{
		protocol.TypeToolStart, protocol.TypeToolEnd,
		protocol.TypeSubAgentStart, protocol.TypeSubAgentEnd,
	}
	if len(msgs) != len(wantTypes) {
		t.Fatalf("got %d msgs, want %d: %+v", len(msgs), len(wantTypes), msgs)
	}
	for i, want := range wantTypes {
		if msgs[i].Type != want {
			t.Errorf("msg[%d].Type = %q, want %q", i, msgs[i].Type, want)
		}
	}
	if msgs[0].ToolName != "echo" {
		t.Errorf("tool_start name = %q, want echo", msgs[0].ToolName)
	}
	if msgs[3].Status != "done" {
		t.Errorf("sub_agent_end status = %q, want done", msgs[3].Status)
	}
	// The excluded older turn must not leak in.
	for _, m := range msgs {
		if m.ToolName == "old_tool" {
			t.Error("older turn leaked into reattach snapshot")
		}
	}
}

// TestJournalHistoryReattach verifies the full-transcript reconstruction spans
// every turn — user prompts, tool/sub-agent activity, and responses in order —
// so a reattaching client sees the whole conversation, not just the last turn.
func TestJournalHistoryReattach(t *testing.T) {
	mustJSON := func(v any) json.RawMessage {
		b, _ := json.Marshal(v)
		return b
	}
	events := []memory.SessionEvent{
		// Turn 1: a user prompt with a tool call and a response.
		{Seq: 1, Turn: 1, Type: "turn_start", Payload: mustJSON(map[string]any{"input": "first", "trigger": "user"})},
		{Seq: 2, Turn: 1, Type: "tool_start", Payload: mustJSON(map[string]any{"name": "echo"})},
		{Seq: 3, Turn: 1, Type: "tool_end", Payload: mustJSON(map[string]any{"name": "echo", "output": "1"})},
		{Seq: 4, Turn: 1, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "answer one"})},
		// Turn 2: an idle/background turn — no user bubble, but its response shows.
		{Seq: 5, Turn: 2, Type: "turn_start", Payload: mustJSON(map[string]any{"trigger": "idle"})},
		{Seq: 6, Turn: 2, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "background note"})},
		// Turn 3: a second user prompt.
		{Seq: 7, Turn: 3, Type: "turn_start", Payload: mustJSON(map[string]any{"input": "second", "trigger": "user"})},
		{Seq: 8, Turn: 3, Type: "turn_end", Payload: mustJSON(map[string]any{"result": "answer two"})},
	}

	d := runtime.New("/tmp/unused.sock", nil, nil, nil)
	d.ConfigureMemory(&mockGoalStore{events: events})

	msgs := d.JournalHistoryForTest("agent-x")

	type want struct {
		typ  protocol.MsgType
		text string
	}
	wants := []want{
		{protocol.TypeHistoryUser, "first"},
		{protocol.TypeToolStart, ""},
		{protocol.TypeToolEnd, ""},
		{protocol.TypeResponse, "answer one"},
		{protocol.TypeResponse, "background note"}, // idle turn: response but no user bubble
		{protocol.TypeHistoryUser, "second"},
		{protocol.TypeResponse, "answer two"},
	}
	if len(msgs) != len(wants) {
		t.Fatalf("got %d msgs, want %d: %+v", len(msgs), len(wants), msgs)
	}
	for i, w := range wants {
		if msgs[i].Type != w.typ {
			t.Errorf("msg[%d].Type = %q, want %q", i, msgs[i].Type, w.typ)
		}
		if w.text != "" && msgs[i].Text != w.text {
			t.Errorf("msg[%d].Text = %q, want %q", i, msgs[i].Text, w.text)
		}
	}
}

// TestJournalReplayEmpty returns an empty snapshot when the journal has nothing.
func TestJournalReplayEmpty(t *testing.T) {
	d := runtime.New("/tmp/unused.sock", nil, nil, nil)
	d.ConfigureMemory(&mockGoalStore{})
	msgs, response := d.JournalReplayForTest("ghost")
	if len(msgs) != 0 || response != "" {
		t.Errorf("expected empty snapshot, got %d msgs, response %q", len(msgs), response)
	}
}
