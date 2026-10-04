package protocol

import (
	"fmt"
	"strings"
)

// Msg is a flat envelope with 29 optional fields, so which of them a given
// message type actually needs was recorded only in a doc comment. That is the
// thing this file turns into a check.
//
// The requirements below were derived from what the daemon's handlers read, not
// from that comment — the comment is unverified by construction, and deriving
// from the code found at least one case where the two disagreed (see
// TypeAttach).
//
// Scope is deliberately narrow: **presence**, not meaning. Whether an agent id
// names a live session, or a mode string is one of the three legal modes, stays
// with the handler that knows. This only catches a message that cannot possibly
// be served.

// fieldRule names one required field and how to read it, so a failure can say
// which wire field was missing rather than just "invalid".
type fieldRule struct {
	name string
	get  func(Msg) string
}

var (
	needAgentID   = fieldRule{"agent_id", func(m Msg) string { return m.AgentID }}
	needText      = fieldRule{"text", func(m Msg) string { return m.Text }}
	needToolName  = fieldRule{"tool_name", func(m Msg) string { return m.ToolName }}
	needRequestID = fieldRule{"request_id", func(m Msg) string { return m.RequestID }}
)

// clientMsgSpec is the ingress contract for one client-sent message type.
type clientMsgSpec struct {
	required []fieldRule

	// satisfiedBy, when set, is an alternative that excuses the required list —
	// for a message with more than one legal shape. session_stop is the only
	// one: it takes either an agent id or the "--all" sentinel in text.
	satisfiedBy func(Msg) bool
	altDesc     string
}

// clientMsgSpecs covers every type in ClientMsgTypes. A type absent from this
// map carries no required fields; the test asserts the two sets agree, so a new
// message type cannot be added to one and forgotten in the other.
var clientMsgSpecs = map[MsgType]clientMsgSpec{
	// Attach must name its target. It used to be tolerated empty, and the result
	// was worse than an error: resolveID falls back to a prefix match, and every
	// id has "" as a prefix, so an empty agent id attached the client to whichever
	// session Go's randomized map iteration happened to yield first.
	TypeAttach:  {required: []fieldRule{needAgentID}},
	TypeContext: {required: []fieldRule{needAgentID}},

	TypeUserTurn:    {required: []fieldRule{needAgentID, needText}},
	TypeSetPlanMode: {required: []fieldRule{needAgentID, needText}},

	TypeWorkflowStop: {required: []fieldRule{needText}},
	TypeWorkflowFail: {required: []fieldRule{needText}},

	TypeSessionStop: {
		required:    []fieldRule{needAgentID},
		satisfiedBy: func(m Msg) bool { return m.Text == "--all" },
		altDesc:     `text "--all"`,
	},

	// No --all escape hatch, unlike session_stop. Erasing every session at once
	// is not an operation anyone should reach by a flag.
	TypeSessionDelete: {required: []fieldRule{needAgentID}},

	TypeStandingShow: {required: []fieldRule{needAgentID}},
	// The action is required too: a control message with no verb should not
	// silently mean one of them.
	TypeStandingControl: {required: []fieldRule{needAgentID, needText}},
	TypeToolCall:        {required: []fieldRule{needToolName}},

	TypePluginCall:       {required: []fieldRule{needToolName}},
	TypeHumanInputAnswer: {required: []fieldRule{needRequestID}},

	// Both are required: a decision with no id, or an id with no verb, must not
	// resolve to a default. Widening a capability ceiling is not a place for one.
	TypeGrantsDecide: {required: []fieldRule{needRequestID, needText}},

	TypeGoalCreate: {required: []fieldRule{needText}},
	TypeGoalDelete: {required: []fieldRule{needText}},

	// The same reason attach must name its target: a session read or watch with
	// an empty id would resolve by prefix to an arbitrary session.
	TypeSessionHistory: {required: []fieldRule{needAgentID}},
	TypeSessionEvents:  {required: []fieldRule{needAgentID}},
	TypeWatch:          {required: []fieldRule{needAgentID}},
}

// ValidateClient checks an inbound client message carries what its type needs.
//
// It returns nil for a type with no requirements, and for a type that is not a
// client message at all — rejecting those is the dispatcher's job (R-PROTO.9),
// and doing it here too would report the same fault in two voices.
func (m Msg) ValidateClient() error {
	spec, ok := clientMsgSpecs[m.Type]
	if !ok {
		return nil
	}
	if spec.satisfiedBy != nil && spec.satisfiedBy(m) {
		return nil
	}
	var missing []string
	for _, r := range spec.required {
		if r.get(m) == "" {
			missing = append(missing, r.name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if spec.altDesc != "" {
		return fmt.Errorf("%s requires %s (or %s)",
			m.Type, strings.Join(missing, " and "), spec.altDesc)
	}
	return fmt.Errorf("%s requires %s", m.Type, strings.Join(missing, " and "))
}

// SpecifiedClientMsgTypes lists the types carrying field requirements. It exists
// for the test that keeps this map and ClientMsgTypes from drifting apart: a
// requirement written against a type the daemon never routes would be checked on
// a path no message travels.
func SpecifiedClientMsgTypes() []MsgType {
	out := make([]MsgType, 0, len(clientMsgSpecs))
	for mt := range clientMsgSpecs {
		out = append(out, mt)
	}
	return out
}
