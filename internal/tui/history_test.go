package tui

import (
	"encoding/json"
	"testing"

	"nine/internal/protocol"
)

// TestHistoryToChatMsgs verifies the reattach transcript reconstructs the same
// alternating user/nine messages a live session produces: a user prompt, the
// tool activity that followed grouped into the nine turn, then the response.
func TestHistoryToChatMsgs(t *testing.T) {
	msgs := []protocol.Msg{
		protocol.NewHistoryUserMsg("a", "first"),
		protocol.NewToolStartMsg("a", "echo", "", json.RawMessage(`{"v":1}`)),
		protocol.NewToolEndMsg("a", "echo", "", json.RawMessage(`{"v":1}`), "1"),
		protocol.NewResponseMsg("a", "answer one"),
		// An idle turn: a response with no preceding user prompt.
		protocol.NewResponseMsg("a", "background note"),
		protocol.NewHistoryUserMsg("a", "second"),
		protocol.NewResponseMsg("a", "answer two"),
	}

	got := historyToChatMsgs(msgs)

	type want struct {
		role  string
		text  string
		tools int
	}
	wants := []want{
		{"user", "first", 0},
		{"nine", "answer one", 1}, // echo tool paired into this turn
		{"nine", "background note", 0},
		{"user", "second", 0},
		{"nine", "answer two", 0},
	}
	if len(got) != len(wants) {
		t.Fatalf("got %d messages, want %d: %+v", len(got), len(wants), got)
	}
	for i, w := range wants {
		if got[i].role != w.role {
			t.Errorf("msg[%d].role = %q, want %q", i, got[i].role, w.role)
		}
		if got[i].text != w.text {
			t.Errorf("msg[%d].text = %q, want %q", i, got[i].text, w.text)
		}
		if len(got[i].toolEvents) != w.tools {
			t.Errorf("msg[%d] toolEvents = %d, want %d", i, len(got[i].toolEvents), w.tools)
		}
	}
	// The paired tool event should carry both input and output.
	if te := got[1].toolEvents; len(te) == 1 {
		if te[0].name != "echo" || te[0].outputStr == "" {
			t.Errorf("tool event not paired: %+v", te[0])
		}
	}
}

// TestHistoryToChatMsgsEmpty returns nothing for an empty transcript.
func TestHistoryToChatMsgsEmpty(t *testing.T) {
	if got := historyToChatMsgs(nil); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestHistoryTrailingActivity keeps an in-flight turn (tool activity but no
// response yet) visible instead of dropping it.
func TestHistoryTrailingActivity(t *testing.T) {
	msgs := []protocol.Msg{
		protocol.NewHistoryUserMsg("a", "do it"),
		protocol.NewToolStartMsg("a", "slow", "", nil),
	}
	got := historyToChatMsgs(msgs)
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(got), got)
	}
	if got[1].role != "nine" || len(got[1].toolEvents) != 1 {
		t.Errorf("trailing in-flight turn not preserved: %+v", got[1])
	}
}
