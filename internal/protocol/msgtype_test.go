package protocol_test

import (
	"encoding/json"
	"testing"

	"nine/internal/protocol"
)

// MsgType is a named string type, so it must encode exactly as the bare string
// literal it replaced. If this ever changed, every deployed client would break
// at once — the point of the type is compile-time safety, not a wire change.
func TestMsgTypeEncodesAsPlainString(t *testing.T) {
	b, err := json.Marshal(protocol.NewUserTurnMsg("a1", "hello"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	got, ok := raw["type"].(string)
	if !ok {
		t.Fatalf(`"type" is %T, want a JSON string`, raw["type"])
	}
	if got != "user_turn" {
		t.Errorf(`"type" = %q, want "user_turn"`, got)
	}
}

// A message written by an older client — a bare JSON string — must still decode
// into the typed field.
func TestMsgTypeDecodesFromPlainString(t *testing.T) {
	var m protocol.Msg
	if err := json.Unmarshal([]byte(`{"type":"user_turn","agent_id":"a1","text":"hi"}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Type != protocol.TypeUserTurn {
		t.Errorf("Type = %q, want %q", m.Type, protocol.TypeUserTurn)
	}
	if m.AgentID != "a1" || m.Text != "hi" {
		t.Errorf("decoded %+v, want agent_id a1 / text hi", m)
	}
}

// Two constants sharing a wire value would make one of them undispatchable: the
// switch would bind both to whichever case comes first.
func TestClientMsgTypesAreDistinct(t *testing.T) {
	seen := map[protocol.MsgType]bool{}
	for _, mt := range protocol.ClientMsgTypes {
		if mt == "" {
			t.Error("ClientMsgTypes contains an empty type")
			continue
		}
		if seen[mt] {
			t.Errorf("duplicate type %q in ClientMsgTypes", mt)
		}
		seen[mt] = true
	}
	if len(protocol.ClientMsgTypes) == 0 {
		t.Fatal("ClientMsgTypes is empty")
	}
}
