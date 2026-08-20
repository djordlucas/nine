package protocol_test

import (
	"strings"
	"testing"

	"nine/internal/protocol"
)

func TestValidateClientRejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		msg  protocol.Msg
		want string // substring the error must name
	}{
		{"attach with no agent", protocol.Msg{Type: protocol.TypeAttach}, "agent_id"},
		{"context with no agent", protocol.Msg{Type: protocol.TypeContext}, "agent_id"},
		{"user_turn with no agent", protocol.Msg{Type: protocol.TypeUserTurn, Text: "hi"}, "agent_id"},
		{"user_turn with no text", protocol.Msg{Type: protocol.TypeUserTurn, AgentID: "a1"}, "text"},
		{"user_turn with neither", protocol.Msg{Type: protocol.TypeUserTurn}, "agent_id and text"},
		{"set_plan_mode with no mode", protocol.Msg{Type: protocol.TypeSetPlanMode, AgentID: "a1"}, "text"},
		{"workflow_stop with no id", protocol.Msg{Type: protocol.TypeWorkflowStop}, "text"},
		{"workflow_fail with no id", protocol.Msg{Type: protocol.TypeWorkflowFail}, "text"},
		{"session_stop with neither", protocol.Msg{Type: protocol.TypeSessionStop}, "--all"},
		{"plugin_call with no tool", protocol.Msg{Type: protocol.TypePluginCall}, "tool_name"},
		{"human answer with no request", protocol.Msg{Type: protocol.TypeHumanInputAnswer, AgentID: "a1"}, "request_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.msg.ValidateClient()
			if err == nil {
				t.Fatalf("ValidateClient(%+v) = nil, want an error", tc.msg)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			// The message type belongs in the error: a client sending several
			// kinds needs to know which one was rejected.
			if !strings.Contains(err.Error(), string(tc.msg.Type)) {
				t.Errorf("error = %q, want it to name the message type", err)
			}
		})
	}
}

func TestValidateClientAcceptsWellFormed(t *testing.T) {
	cases := []protocol.Msg{
		{Type: protocol.TypeNewConversation},
		{Type: protocol.TypeNewConversation, Interactive: true},
		{Type: protocol.TypeAttach, AgentID: "a1"},
		{Type: protocol.TypeContext, AgentID: "a1"},
		{Type: protocol.TypeUserTurn, AgentID: "a1", Text: "hello"},
		{Type: protocol.TypeSetPlanMode, AgentID: "a1", Text: "plan-only"},
		{Type: protocol.TypeWorkflowStop, Text: "wf-1"},
		{Type: protocol.TypeWorkflowFail, Text: "--all"},
		{Type: protocol.TypeSessionStop, AgentID: "a1"},
		{Type: protocol.TypePluginCall, ToolName: "echo"},
		{Type: protocol.TypeHumanInputAnswer, AgentID: "a1", RequestID: "r1", Answer: "yes"},
		// The query verbs carry nothing.
		{Type: protocol.TypeStatus},
		{Type: protocol.TypeListGoals},
		{Type: protocol.TypeListWorkflows},
		{Type: protocol.TypeListTools},
		{Type: protocol.TypePluginsList},
		{Type: protocol.TypeToolsReload},
	}
	for _, m := range cases {
		if err := m.ValidateClient(); err != nil {
			t.Errorf("ValidateClient(%+v) = %v, want nil", m, err)
		}
	}
}

// session_stop has two legal shapes; the --all sentinel excuses the agent id.
func TestValidateClientSessionStopAcceptsEitherShape(t *testing.T) {
	for _, m := range []protocol.Msg{
		{Type: protocol.TypeSessionStop, AgentID: "a1"},
		{Type: protocol.TypeSessionStop, Text: "--all"},
	} {
		if err := m.ValidateClient(); err != nil {
			t.Errorf("ValidateClient(%+v) = %v, want nil", m, err)
		}
	}
	// But an arbitrary text is not the sentinel.
	m := protocol.Msg{Type: protocol.TypeSessionStop, Text: "some-session"}
	if err := m.ValidateClient(); err == nil {
		t.Error("session_stop with a non-sentinel text and no agent id was accepted")
	}
}

// A type the daemon does not route is not this check's business — R-PROTO.9's
// dispatch test owns that, and reporting it twice would give one fault two
// voices.
func TestValidateClientIgnoresUnknownTypes(t *testing.T) {
	if err := (protocol.Msg{Type: "not_a_real_type"}).ValidateClient(); err != nil {
		t.Errorf("ValidateClient(unknown) = %v, want nil", err)
	}
}

// Every constructor the client actually uses must produce a message that passes
// its own validation — otherwise the check rejects Nine's own traffic.
func TestClientConstructorsProduceValidMessages(t *testing.T) {
	cases := []protocol.Msg{
		protocol.NewAttachMsg("a1"),
		protocol.NewContextMsg("a1"),
		protocol.NewUserTurnMsg("a1", "hello"),
		protocol.NewSetPlanModeMsg("a1", "plan-only"),
		protocol.NewWorkflowStopMsg("wf-1"),
		protocol.NewWorkflowFailMsg("wf-1"),
		protocol.NewSessionStopMsg("a1", false),
		protocol.NewSessionStopMsg("", true),
		protocol.NewPluginCallMsg("echo", nil),
		protocol.NewHumanInputAnswerMsg("a1", "r1", "yes"),
		protocol.NewQueryMsg(protocol.TypeStatus),
		protocol.NewQueryMsg(protocol.TypeListGoals),
	}
	for _, m := range cases {
		if err := m.ValidateClient(); err != nil {
			t.Errorf("%s constructor produced a message its own validation rejects: %v", m.Type, err)
		}
	}
}

// The spec map is keyed by MsgType and must only ever describe client messages.
// A daemon→client type in there would be checked on a path it never travels.
func TestValidateSpecsCoverOnlyClientTypes(t *testing.T) {
	client := map[protocol.MsgType]bool{}
	for _, mt := range protocol.ClientMsgTypes {
		client[mt] = true
	}
	for _, mt := range protocol.SpecifiedClientMsgTypes() {
		if !client[mt] {
			t.Errorf("%q has field requirements but is not in ClientMsgTypes", mt)
		}
	}
}
