package protocol_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"nine/internal/llm"
	"nine/internal/protocol"
)

// R-PROTO.1: every Msg marshals/unmarshals over newline-delimited JSON framing.
// send/recv use json.Encoder/Decoder, which frame with a trailing newline; two
// messages written back-to-back must decode as two distinct messages in order.
func TestMsgFramingRoundTrip(t *testing.T) {
	msgs := []protocol.Msg{
		protocol.NewUserTurnMsg("a1", "hello"),
		protocol.NewToolStartMsg("a1", "shell", "Shell", json.RawMessage(`{"cmd":"ls"}`)),
		protocol.NewResponseMsg("a1", "done"),
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
	}
	// Framing: one newline-terminated JSON object per message.
	if n := bytes.Count(buf.Bytes(), []byte{'\n'}); n != len(msgs) {
		t.Errorf("framing: %d newlines, want %d", n, len(msgs))
	}

	dec := json.NewDecoder(&buf)
	for i, want := range msgs {
		var got protocol.Msg
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("decode %d: %v", i, err)
		}
		if got.Type != want.Type || got.AgentID != want.AgentID || got.Text != want.Text {
			t.Errorf("msg %d = %+v, want %+v", i, got, want)
		}
	}
}

// R-PROTO.2: request constructors set the right type and fields, including the
// new_conversation Interactive flag.
func TestClientMessageConstructors(t *testing.T) {
	if m := protocol.NewUserTurnMsg("a", "hi"); m.Type != "user_turn" || m.AgentID != "a" || m.Text != "hi" {
		t.Errorf("NewUserTurnMsg = %+v", m)
	}
	if m := protocol.NewAttachMsg("a"); m.Type != "attach" || m.AgentID != "a" {
		t.Errorf("NewAttachMsg = %+v", m)
	}
	// The Interactive flag round-trips on new_conversation.
	nc := protocol.Msg{Type: "new_conversation", Interactive: true}
	var got protocol.Msg
	b, _ := json.Marshal(nc)
	_ = json.Unmarshal(b, &got)
	if !got.Interactive {
		t.Errorf("Interactive flag lost in round-trip: %s", b)
	}
}

// R-PROTO.3: daemon→client progress messages convert to structured
// ProgressEvents with the matching type; non-progress messages do not.
func TestProgressEventConversion(t *testing.T) {
	cases := []struct {
		msg      protocol.Msg
		wantType protocol.MsgType
		wantOK   bool
	}{
		{protocol.NewToolStartMsg("a", "shell", "Shell", nil), "tool_start", true},
		{protocol.NewToolEndMsg("a", "shell", "Shell", nil, "out"), "tool_end", true},
		{protocol.NewContextUpdateMsg("a", 100, 1000), "context_update", true},
		{protocol.NewResponseChunkMsg("a", "tok"), "response_chunk", true},
		{protocol.NewThinkingChunkMsg("a", "reasoning"), "thinking_chunk", true},
		{protocol.NewStageMsg("a", "queued"), "stage", true},
		{protocol.NewResponseMsg("a", "final"), "", false}, // final response is not a progress event
	}
	for _, c := range cases {
		ev, ok := c.msg.ToProgressEvent()
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v, want %v", c.msg.Type, ok, c.wantOK)
			continue
		}
		if ok && ev.Type != c.wantType {
			t.Errorf("%s → event type %q, want %q", c.msg.Type, ev.Type, c.wantType)
		}
	}

	// context_update carries the numbers through.
	ev, _ := protocol.NewContextUpdateMsg("a", 100, 1000).ToProgressEvent()
	if ev.ContextUsed != 100 || ev.ContextBudget != 1000 {
		t.Errorf("context_update event = %+v, want used=100 budget=1000", ev)
	}

	// thinking_chunk carries the reasoning token through the Text field.
	tev, _ := protocol.NewThinkingChunkMsg("a", "reasoning").ToProgressEvent()
	if tev.Text != "reasoning" {
		t.Errorf("thinking_chunk event Text = %q, want \"reasoning\"", tev.Text)
	}
}

// R-PROTO.6 / I8: display names are carried on the client-facing tool events but
// MUST NOT be serialized on the tool definitions sent to the LLM.
func TestDisplayNamesClientOnly(t *testing.T) {
	// Client-facing: the tool event carries the display name for the TUI.
	ev, _ := protocol.NewToolStartMsg("a", "shell", "Run Shell", nil).ToProgressEvent()
	if ev.ToolDisplayName != "Run Shell" {
		t.Errorf("client event display name = %q, want \"Run Shell\"", ev.ToolDisplayName)
	}

	// LLM-facing: llm.ToolDef.DisplayName is json:"-", so it never appears in the
	// tool definition marshaled into a completion request.
	b, err := json.Marshal(llm.ToolDef{Name: "shell", Description: "run", DisplayName: "Run Shell"})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("Run Shell")) || bytes.Contains(b, []byte("display")) {
		t.Errorf("display name leaked into LLM-facing ToolDef JSON: %s", b)
	}
}

// R-PROTO.7 (partial): with no daemon listening, CanConnect reports false. (Full
// auto-start/re-exec is covered by the runtime integration tests.)
func TestCanConnectDeadSocket(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "nope.sock")
	if protocol.CanConnect(dead) {
		t.Error("CanConnect on a nonexistent socket should be false")
	}
}

// R-HITL.6: human_input_required survives the wire and parses into a
// ProgressEvent. Origin attributes a question raised by a sub-agent; AgentID
// stays the owning session, so a client answers with that ID and needs no
// knowledge of sub-agent IDs.
func TestHumanInputRequiredCarriesOrigin(t *testing.T) {
	origin := `sub-agent "executor" · audit the repo`
	msg := protocol.NewHumanInputRequiredMsg("root-1", "req-9", "Run tool \"shell\"?", []string{"yes", "no"}, 300, origin)

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(msg); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got protocol.Msg
	if err := json.NewDecoder(&buf).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	evt, ok := got.ToProgressEvent()
	if !ok {
		t.Fatal("human_input_required should parse as a progress event")
	}
	if evt.HumanRequest == nil {
		t.Fatal("progress event carries no HumanRequest")
	}
	if evt.HumanRequest.Origin != origin {
		t.Errorf("Origin = %q, want %q", evt.HumanRequest.Origin, origin)
	}
	if got.AgentID != "root-1" {
		t.Errorf("AgentID = %q, want the owning session", got.AgentID)
	}
	if evt.HumanRequest.RequestID != "req-9" || evt.HumanRequest.TimeoutSeconds != 300 {
		t.Errorf("request round-tripped as %+v", evt.HumanRequest)
	}

	// A question from the session's own loop carries no origin, and the field
	// is omitted from the wire form entirely.
	plain := protocol.NewHumanInputRequiredMsg("root-1", "req-10", "Proceed?", nil, 300, "")
	raw, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Contains(raw, []byte("origin")) {
		t.Errorf("empty origin should be omitted, got %s", raw)
	}
}
