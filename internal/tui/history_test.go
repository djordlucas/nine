package tui

import (
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/protocol"
)

// TestHistoryToChatMsgs verifies the reattach transcript reconstructs the same
// alternating user/nine messages a live session produces: a user prompt, the
// tool activity that followed grouped into the nine turn, then the response.
func TestHistoryToChatMsgs(t *testing.T) {
	msgs := []protocol.Msg{
		protocol.NewHistoryUserMsg("a", "first"),
		protocol.NewToolStartMsg("a", "echo", "", "", json.RawMessage(`{"v":1}`)),
		protocol.NewToolEndMsg("a", "echo", "", "", json.RawMessage(`{"v":1}`), "1"),
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

// TestSubAgentRoleThreaded verifies a sub-agent's resolved leaf role survives the
// replay round-trip and is rendered on the sub-agent line, so the user can see
// which kind of agent ran.
func TestSubAgentRoleThreaded(t *testing.T) {
	evts := replayToToolEvents([]protocol.Msg{
		protocol.NewSubAgentStartMsg("a", "sub1", "build the parser", "software-dev"),
		protocol.NewSubAgentEndMsg("a", "sub1", "build the parser", "done", "software-dev"),
	})
	if len(evts) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(evts), evts)
	}
	if !evts[0].subAgent || evts[0].subAgentRole != "software-dev" {
		t.Fatalf("role not threaded onto sub-agent event: %+v", evts[0])
	}

	var sb strings.Builder
	renderToolEvent(&sb, evts[0], true, autoPalette(), 80)
	if out := sb.String(); !strings.Contains(out, "sub-agent · software-dev") {
		t.Errorf("rendered line missing role label:\n%s", out)
	}
}

// TestSubAgentRoleOmittedWhenEmpty keeps the bare "sub-agent" label when no role
// is known (e.g. a workflow step spawn resolving to the default leaf).
func TestSubAgentRoleOmittedWhenEmpty(t *testing.T) {
	evts := replayToToolEvents([]protocol.Msg{
		protocol.NewSubAgentStartMsg("a", "sub1", "do it", ""),
	})
	var sb strings.Builder
	renderToolEvent(&sb, evts[0], true, autoPalette(), 80)
	if out := sb.String(); strings.Contains(out, " · ") {
		t.Errorf("expected no role separator for empty role:\n%s", out)
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
		protocol.NewToolStartMsg("a", "slow", "", "", nil),
	}
	got := historyToChatMsgs(msgs)
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(got), got)
	}
	if got[1].role != "nine" || len(got[1].toolEvents) != 1 {
		t.Errorf("trailing in-flight turn not preserved: %+v", got[1])
	}
}
