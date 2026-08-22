package tui

import (
	"os"
	"strings"
	"testing"

	"nine/internal/protocol"
)

// Types the daemon sends that the TUI deliberately does not match on by name,
// with the reason. Each is consumed somewhere the type is already known from
// context, so a `case` for it would be dead code rather than missing code.
var notMatchedByName = map[protocol.MsgType]string{
	protocol.TypeConversationID: "read during connect, before the event loop starts",
	protocol.TypeOK:             "read during attach/connect handshake",
	protocol.TypeDone:           "ends the response stream; handled by the stream reader",
	protocol.TypeError:          "surfaced through the error path, not the progress switch",
	protocol.TypeResponseChunk:  "delivered as chunkMsg by the stream reader",
}

// Every message type the daemon can send is either matched by name in this
// package or listed above with a reason.
//
// This is the client-side mirror of the daemon's dispatch coverage test. The
// failure it guards is quieter than the daemon's: an unhandled *client* message
// comes back as "unknown message type", but a daemon message no client renders
// produces nothing at all — the feature looks unimplemented rather than
// unwired. Adding a type to ServerMsgTypes and forgetting the TUI is exactly
// the mistake that has no other symptom.
//
// It matches on source text rather than by driving the model, because the
// switch lives inside the Bubble Tea update loop and reaching every branch
// would mean constructing a session for each. The weaker check still fails
// when a new type is added and nothing renders it, which is the case that
// actually occurs.
func TestServerMsgTypesAreRendered(t *testing.T) {
	src, err := os.ReadFile("tui.go")
	if err != nil {
		t.Fatalf("read tui.go: %v", err)
	}
	body := string(src)

	// Guard the guard: if the constants stopped being referenced by name at all,
	// every assertion below would pass vacuously.
	if n := strings.Count(body, "case protocol.Type"); n < 10 {
		t.Fatalf("only %d typed cases found; the switch no longer matches on "+
			"protocol constants and this test proves nothing", n)
	}

	for _, mt := range protocol.ServerMsgTypes {
		name := constName(mt)
		if name == "" {
			t.Errorf("%q has no constant name mapping in this test", mt)
			continue
		}
		if strings.Contains(body, "protocol."+name) {
			continue
		}
		if why, ok := notMatchedByName[mt]; ok {
			if why == "" {
				t.Errorf("%s is exempt but carries no reason", name)
			}
			continue
		}
		t.Errorf("the daemon can send %s (%q) but the TUI never mentions it — "+
			"either render it, or add it to notMatchedByName with a reason",
			name, mt)
	}
}

// constName maps a wire value back to its Go constant name. Written out rather
// than reflected so a renamed constant with an unchanged wire value is caught.
func constName(mt protocol.MsgType) string {
	switch mt {
	case protocol.TypeConversationID:
		return "TypeConversationID"
	case protocol.TypeOK:
		return "TypeOK"
	case protocol.TypeResponse:
		return "TypeResponse"
	case protocol.TypeDone:
		return "TypeDone"
	case protocol.TypeError:
		return "TypeError"
	case protocol.TypeHistoryUser:
		return "TypeHistoryUser"
	case protocol.TypeSetName:
		return "TypeSetName"
	case protocol.TypeSetInstanceName:
		return "TypeSetInstanceName"
	case protocol.TypeToolStart:
		return "TypeToolStart"
	case protocol.TypeToolEnd:
		return "TypeToolEnd"
	case protocol.TypeContextUpdate:
		return "TypeContextUpdate"
	case protocol.TypeResponseChunk:
		return "TypeResponseChunk"
	case protocol.TypeThinkingChunk:
		return "TypeThinkingChunk"
	case protocol.TypeThinking:
		return "TypeThinking"
	case protocol.TypeSubAgentStart:
		return "TypeSubAgentStart"
	case protocol.TypeSubAgentEnd:
		return "TypeSubAgentEnd"
	case protocol.TypeStage:
		return "TypeStage"
	case protocol.TypePlanStart:
		return "TypePlanStart"
	case protocol.TypePlanEnd:
		return "TypePlanEnd"
	case protocol.TypeNotice:
		return "TypeNotice"
	case protocol.TypeHumanInputRequired:
		return "TypeHumanInputRequired"
	}
	return ""
}
