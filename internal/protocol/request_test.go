package protocol_test

import (
	"encoding/json"
	"testing"

	"nine/internal/protocol"
)

// The point of the typed requests is that a handler reads a struct carrying only
// its own fields, already populated — not a 29-field union it must know its way
// around.
func TestDecodeRequestProducesTypedRequests(t *testing.T) {
	cases := []struct {
		name  string
		msg   protocol.Msg
		check func(*testing.T, protocol.Request)
	}{
		{"user_turn", protocol.Msg{Type: protocol.TypeUserTurn, AgentID: "a1", Text: "hi", ForceThink: true},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.UserTurnReq)
				if !ok {
					t.Fatalf("got %T, want UserTurnReq", r)
				}
				if got.AgentID != "a1" || got.Text != "hi" || !got.ForceThink {
					t.Errorf("decoded %+v", got)
				}
			}},
		{"attach", protocol.Msg{Type: protocol.TypeAttach, AgentID: "a1"},
			func(t *testing.T, r protocol.Request) {
				if got, ok := r.(protocol.AttachReq); !ok || got.AgentID != "a1" {
					t.Errorf("got %#v", r)
				}
			}},
		// The wire carries the mode in `text`; the struct names it Mode, so a
		// handler no longer has to know that convention.
		{"set_plan_mode", protocol.Msg{Type: protocol.TypeSetPlanMode, AgentID: "a1", Text: "plan-only"},
			func(t *testing.T, r protocol.Request) {
				if got, ok := r.(protocol.SetPlanModeReq); !ok || got.Mode != "plan-only" {
					t.Errorf("got %#v", r)
				}
			}},
		// "--all" is a sentinel string on the wire and a bool in the struct.
		{"session_stop --all", protocol.Msg{Type: protocol.TypeSessionStop, Text: "--all"},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.SessionStopReq)
				if !ok || !got.All || got.AgentID != "" {
					t.Errorf("got %#v", r)
				}
			}},
		{"session_stop one", protocol.Msg{Type: protocol.TypeSessionStop, AgentID: "a1"},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.SessionStopReq)
				if !ok || got.All || got.AgentID != "a1" {
					t.Errorf("got %#v", r)
				}
			}},
		{"workflow_fail --all", protocol.Msg{Type: protocol.TypeWorkflowFail, Text: "--all"},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.WorkflowFailReq)
				if !ok || !got.All || got.ID != "" {
					t.Errorf("got %#v", r)
				}
			}},
		{"workflow_fail one", protocol.Msg{Type: protocol.TypeWorkflowFail, Text: "wf-1"},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.WorkflowFailReq)
				if !ok || got.All || got.ID != "wf-1" {
					t.Errorf("got %#v", r)
				}
			}},
		{"plugin_call", protocol.Msg{Type: protocol.TypePluginCall, ToolName: "echo", ToolInput: json.RawMessage(`{"v":1}`)},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.PluginCallReq)
				if !ok || got.Tool != "echo" || string(got.Args) != `{"v":1}` {
					t.Errorf("got %#v", r)
				}
			}},
		{"human answer", protocol.Msg{Type: protocol.TypeHumanInputAnswer, AgentID: "a1", RequestID: "r1", Answer: "yes"},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.HumanAnswerReq)
				if !ok || got.RequestID != "r1" || got.Answer != "yes" {
					t.Errorf("got %#v", r)
				}
			}},
		{"status is a query", protocol.Msg{Type: protocol.TypeStatus},
			func(t *testing.T, r protocol.Request) {
				got, ok := r.(protocol.QueryReq)
				if !ok || got.Kind != protocol.TypeStatus {
					t.Errorf("got %#v", r)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := protocol.DecodeRequest(tc.msg)
			if err != nil {
				t.Fatalf("DecodeRequest: %v", err)
			}
			if r.Type() != tc.msg.Type {
				t.Errorf("Type() = %q, want %q", r.Type(), tc.msg.Type)
			}
			tc.check(t, r)
		})
	}
}

// Decoding validates, so a handler never receives a typed request that is merely
// typed — it is populated. This is what lets the dispatch drop its per-field
// emptiness checks.
func TestDecodeRequestRejectsIncomplete(t *testing.T) {
	for _, m := range []protocol.Msg{
		{Type: protocol.TypeAttach},
		{Type: protocol.TypeUserTurn, AgentID: "a1"},
		{Type: protocol.TypeWorkflowStop},
		{Type: protocol.TypePluginCall},
	} {
		if _, err := protocol.DecodeRequest(m); err == nil {
			t.Errorf("DecodeRequest(%+v) succeeded, want a validation error", m)
		}
	}
}

// An unroutable type is reported by the decoder, which is why the dispatch's
// type switch needs no default for "unknown".
func TestDecodeRequestRejectsUnknownType(t *testing.T) {
	_, err := protocol.DecodeRequest(protocol.Msg{Type: "not_a_type"})
	if err == nil {
		t.Fatal("DecodeRequest accepted an unknown type")
	}
}

// Every client-sendable type must decode to some request, or the dispatch would
// reject traffic the protocol says is legal.
func TestEveryClientMsgTypeDecodes(t *testing.T) {
	// Minimal populated messages, one per type, so validation passes.
	fill := map[protocol.MsgType]protocol.Msg{
		protocol.TypeAttach:           {AgentID: "a1"},
		protocol.TypeContext:          {AgentID: "a1"},
		protocol.TypeUserTurn:         {AgentID: "a1", Text: "hi"},
		protocol.TypeSetPlanMode:      {AgentID: "a1", Text: "off"},
		protocol.TypeSessionStop:      {AgentID: "a1"},
		protocol.TypeSessionDelete:    {AgentID: "a1"},
		protocol.TypeStandingShow:     {AgentID: "s1"},
		protocol.TypeStandingControl:  {AgentID: "s1", Text: "stop"},
		protocol.TypeToolCall:         {ToolName: "echo"},
		protocol.TypeWorkflowStop:     {Text: "wf-1"},
		protocol.TypeWorkflowFail:     {Text: "wf-1"},
		protocol.TypePluginCall:       {ToolName: "echo"},
		protocol.TypeHumanInputAnswer: {AgentID: "a1", RequestID: "r1"},
		protocol.TypeGrantsDecide:     {RequestID: "cr1", Text: "approve"},
		protocol.TypeGoalCreate:       {Text: "watch the repo"},
		protocol.TypeGoalDelete:       {Text: "g1"},
		protocol.TypeSessionHistory:   {AgentID: "a1"},
		protocol.TypeSessionEvents:    {AgentID: "a1", Turn: -1},
		protocol.TypeWatch:            {AgentID: "a1"},
	}
	for _, mt := range protocol.ClientMsgTypes {
		m := fill[mt]
		m.Type = mt
		if _, err := protocol.DecodeRequest(m); err != nil {
			t.Errorf("%s does not decode: %v", mt, err)
		}
	}
}

// R-PROTO.1 is a statement about the bytes. The typed requests are a source-level
// change, so a message must marshal to exactly the flat object it always did —
// otherwise every deployed client breaks, and there is no version negotiation to
// catch it.
func TestWireShapeIsUnchanged(t *testing.T) {
	b, err := json.Marshal(protocol.NewUserTurnMsg("a1", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	// Flat: the fields sit at the top level, not under a payload object.
	for _, k := range []string{"type", "agent_id", "text"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("wire object is missing top-level %q: %s", k, b)
		}
	}
	if _, nested := raw["payload"]; nested {
		t.Errorf("wire object gained a nested payload, breaking R-PROTO.1: %s", b)
	}
}
