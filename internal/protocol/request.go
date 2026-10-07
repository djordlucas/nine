package protocol

import (
	"encoding/json"
	"fmt"
)

// This file is F2 step 3 for the client→daemon half: each request a client can
// send has its own struct with only the fields that request carries, instead of
// handlers reaching into a 29-field union and knowing by convention which of
// them are populated.
//
// The wire is unchanged. R-PROTO.1 requires "one flat object with a `type`
// discriminator … not a tagged union per direction", and that is a statement
// about the bytes: a Request marshals to exactly the flat Msg it came from. The
// union stays the transport; it stops being the thing handlers program against.
//
// That distinction is why this is not a breaking change. There is no version
// negotiation on this protocol, so a nested payload would strand any client
// built against an older daemon, and buy nothing the typed structs do not
// already give.

// Request is a decoded client→daemon message. The concrete types below are the
// full set; a type switch over them is exhaustive by construction, where a
// switch over MsgType was exhaustive only by inspection.
type Request interface {
	// Type reports the wire type this request decodes from.
	Type() MsgType
}

// --- the requests ---

// NewConversationReq opens a fresh conversation. Interactive marks it
// HITL-eligible; only the TUI sets it.
type NewConversationReq struct{ Interactive bool }

// AttachReq reconnects to an existing conversation.
type AttachReq struct{ AgentID string }

// UserTurnReq sends a message. ForceThink is the /think escape hatch.
type UserTurnReq struct {
	AgentID    string
	Text       string
	ForceThink bool
}

// SetPlanModeReq changes a session's reasoning mode live.
type SetPlanModeReq struct {
	AgentID string
	Mode    string // off | plan-only | always
}

// ContextReq asks for a session's assembled-context breakdown.
type ContextReq struct{ AgentID string }

// SessionStopReq terminates one session, or all of them.
type SessionStopReq struct {
	AgentID string
	All     bool
}

// SessionDeleteReq erases one session: the conversation and everything keyed to
// it. Distinct from SessionStopReq, which ends a session and keeps its history.
type SessionDeleteReq struct{ AgentID string }

// StandingShowReq asks for one standing run in detail.
type StandingShowReq struct {
	ID    string
	Limit int
}

// StandingControlReq stops or starts a standing run.
type StandingControlReq struct {
	ID     string
	Action string // "stop" | "start"
}

// ToolDeleteReq deletes a tool Nine wrote; any other tool is refused.
type ToolDeleteReq struct {
	Tool string
}

// ToolCallReq invokes one tool once, for testing. LiveState opts out of the
// scratch state namespace a test call gets by default.
type ToolCallReq struct {
	Tool      string
	Args      json.RawMessage
	LiveState bool
}

// WorkflowStopReq cancels an active workflow.
type WorkflowStopReq struct{ ID string }

// WorkflowFailReq marks one workflow, or all of them, failed.
type WorkflowFailReq struct {
	ID  string
	All bool
}

// ListNotificationsReq reads the notification feed. All includes seen entries.
type ListNotificationsReq struct{ All bool }

// PluginCallReq invokes a tool directly, bypassing the agent loop.
type PluginCallReq struct {
	Tool string
	Args json.RawMessage
}

// HumanAnswerReq delivers a human's answer to a pending ask_human.
type HumanAnswerReq struct {
	AgentID   string
	RequestID string
	Answer    string
}

// GrantsDecideReq settles one capability request, or revokes one grant in force.
// Action is "approve", "deny" or "revoke"; ID names a request for the first two
// and a grant for the third.
type GrantsDecideReq struct {
	ID     string
	Action string
}

// GoalCreateReq creates a goal on the operator's behalf. ParentID names a parent
// goal; empty makes a top-level goal, which gets a pursue session.
type GoalCreateReq struct {
	Description string
	ParentID    string
}

// GoalDeleteReq deletes a goal, its sub-goals, and stops its pursue session.
type GoalDeleteReq struct{ ID string }

// SessionHistoryReq asks for a session's transcript.
type SessionHistoryReq struct{ AgentID string }

// SessionEventsReq asks for a session's raw journal. Turn is 0 for every turn,
// N for turn N, -1 for the latest.
type SessionEventsReq struct {
	AgentID string
	Turn    int
}

// WatchReq follows a session's progress events for as long as the connection
// stays open.
type WatchReq struct{ AgentID string }

// QueryReq is every request that carries no fields of its own — status and the
// list/reload verbs. They share a struct rather than each having an empty one:
// an empty struct per type would be eleven names that differ in nothing, and a
// handler that needs to tell them apart still has Kind.
type QueryReq struct{ Kind MsgType }

func (NewConversationReq) Type() MsgType   { return TypeNewConversation }
func (AttachReq) Type() MsgType            { return TypeAttach }
func (UserTurnReq) Type() MsgType          { return TypeUserTurn }
func (SetPlanModeReq) Type() MsgType       { return TypeSetPlanMode }
func (ContextReq) Type() MsgType           { return TypeContext }
func (SessionStopReq) Type() MsgType       { return TypeSessionStop }
func (SessionDeleteReq) Type() MsgType     { return TypeSessionDelete }
func (StandingShowReq) Type() MsgType      { return TypeStandingShow }
func (StandingControlReq) Type() MsgType   { return TypeStandingControl }
func (ToolCallReq) Type() MsgType          { return TypeToolCall }
func (ToolDeleteReq) Type() MsgType        { return TypeToolDelete }
func (WorkflowStopReq) Type() MsgType      { return TypeWorkflowStop }
func (WorkflowFailReq) Type() MsgType      { return TypeWorkflowFail }
func (ListNotificationsReq) Type() MsgType { return TypeListNotifications }
func (PluginCallReq) Type() MsgType        { return TypePluginCall }
func (HumanAnswerReq) Type() MsgType       { return TypeHumanInputAnswer }
func (GrantsDecideReq) Type() MsgType      { return TypeGrantsDecide }
func (GoalCreateReq) Type() MsgType        { return TypeGoalCreate }
func (GoalDeleteReq) Type() MsgType        { return TypeGoalDelete }
func (SessionHistoryReq) Type() MsgType    { return TypeSessionHistory }
func (SessionEventsReq) Type() MsgType     { return TypeSessionEvents }
func (WatchReq) Type() MsgType             { return TypeWatch }
func (q QueryReq) Type() MsgType           { return q.Kind }

// queryKinds are the field-less requests QueryReq stands for.
var queryKinds = map[MsgType]bool{
	TypeStatus: true, TypeListGoals: true, TypeListWorkflows: true,
	TypeListTools: true, TypePluginsList: true, TypePluginsReload: true,
	TypeToolsList: true, TypeToolsReload: true, TypeSessionsList: true,
	TypeStandingList: true, TypeGrantsList: true, TypeListSkills: true,
}

// DecodeRequest turns a wire message into the typed request it represents.
//
// It runs ValidateClient first, so a handler never receives a request missing a
// field its type requires (R-PROTO.11) — the decoded struct is not merely typed
// but populated. An unroutable type is reported here rather than reaching a
// handler, which is what lets the caller's type switch have no default case for
// "unknown".
func DecodeRequest(m Msg) (Request, error) {
	if err := m.ValidateClient(); err != nil {
		return nil, err
	}
	if queryKinds[m.Type] {
		return QueryReq{Kind: m.Type}, nil
	}
	switch m.Type {
	case TypeNewConversation:
		return NewConversationReq{Interactive: m.Interactive}, nil
	case TypeAttach:
		return AttachReq{AgentID: m.AgentID}, nil
	case TypeUserTurn:
		return UserTurnReq{AgentID: m.AgentID, Text: m.Text, ForceThink: m.ForceThink}, nil
	case TypeSetPlanMode:
		return SetPlanModeReq{AgentID: m.AgentID, Mode: m.Text}, nil
	case TypeContext:
		return ContextReq{AgentID: m.AgentID}, nil
	case TypeSessionStop:
		// "--all" is a sentinel in Text on the wire; the struct says what it means.
		return SessionStopReq{AgentID: m.AgentID, All: m.Text == "--all"}, nil
	case TypeSessionDelete:
		return SessionDeleteReq{AgentID: m.AgentID}, nil
	case TypeStandingShow:
		return StandingShowReq{ID: m.AgentID, Limit: m.Limit}, nil
	case TypeStandingControl:
		return StandingControlReq{ID: m.AgentID, Action: m.Text}, nil
	case TypeToolDelete:
		return ToolDeleteReq{Tool: m.ToolName}, nil
	case TypeToolCall:
		return ToolCallReq{Tool: m.ToolName, Args: m.ToolInput, LiveState: m.Text == "--live-state"}, nil
	case TypeWorkflowStop:
		return WorkflowStopReq{ID: m.Text}, nil
	case TypeWorkflowFail:
		if m.Text == "--all" {
			return WorkflowFailReq{All: true}, nil
		}
		return WorkflowFailReq{ID: m.Text}, nil
	case TypeListNotifications:
		return ListNotificationsReq{All: m.Text == "--all"}, nil
	case TypePluginCall:
		return PluginCallReq{Tool: m.ToolName, Args: m.ToolInput}, nil
	case TypeHumanInputAnswer:
		return HumanAnswerReq{AgentID: m.AgentID, RequestID: m.RequestID, Answer: m.Answer}, nil
	case TypeGrantsDecide:
		return GrantsDecideReq{ID: m.RequestID, Action: m.Text}, nil
	case TypeGoalCreate:
		return GoalCreateReq{Description: m.Text, ParentID: m.ID}, nil
	case TypeGoalDelete:
		return GoalDeleteReq{ID: m.Text}, nil
	case TypeSessionHistory:
		return SessionHistoryReq{AgentID: m.AgentID}, nil
	case TypeSessionEvents:
		return SessionEventsReq{AgentID: m.AgentID, Turn: m.Turn}, nil
	case TypeWatch:
		return WatchReq{AgentID: m.AgentID}, nil
	}
	return nil, fmt.Errorf("unknown message type: %s", m.Type)
}
