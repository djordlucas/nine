package protocol

import (
	"encoding/json"
	"time"
)

// MsgType names a wire message. It is a distinct type rather than a bare string
// so that a mistyped literal is a compile error rather than a message the daemon
// silently fails to route — the switch in runtime.Daemon dispatches on these, and
// an unrecognized type reaches the default branch at runtime with no other signal.
//
// The underlying type is string, so the JSON encoding is unchanged: a MsgType
// marshals and unmarshals exactly as the literal it replaces. This is a
// compile-time change only and does not move the wire format.
//
// Note that these are *wire* message types. The event journal has its own type
// vocabulary (`turn_start`, `llm_request`, …) which overlaps this one by
// coincidence on a few names and is not interchangeable with it; see
// spec/contracts/event-journal.md.
type MsgType string

// Client → daemon.
const (
	TypeNewConversation MsgType = "new_conversation"
	TypeAttach          MsgType = "attach"
	TypeSetPlanMode     MsgType = "set_plan_mode"
	TypeUserTurn        MsgType = "user_turn"
	TypeSessionStop     MsgType = "session_stop"
	TypeSessionDelete   MsgType = "session_delete"
	TypeStandingShow    MsgType = "standing_show"
	TypeStandingControl MsgType = "standing_control"
	TypeToolCall        MsgType = "tool_call"
	TypeWorkflowStop    MsgType = "workflow_stop"
	TypeWorkflowFail    MsgType = "workflow_fail"
	TypePluginCall      MsgType = "plugin_call"

	// Queries. Each is echoed back as the type of its own reply, with the
	// payload in Text — so these appear in both directions.
	TypeStatus            MsgType = "status"
	TypeContext           MsgType = "context"
	TypeListGoals         MsgType = "list_goals"
	TypeListWorkflows     MsgType = "list_workflows"
	TypeListNotifications MsgType = "list_notifications"
	TypeListTools         MsgType = "list_tools"
	TypePluginsList       MsgType = "plugins_list"
	TypePluginsReload     MsgType = "plugins_reload"
	TypeSessionsList      MsgType = "sessions_list"
	TypeStandingList      MsgType = "standing_list"
	TypeToolsList         MsgType = "tools_list"
	TypeToolsReload       MsgType = "tools_reload"
)

// Daemon → client.
const (
	TypeConversationID  MsgType = "conversation_id"
	TypeOK              MsgType = "ok"
	TypeResponse        MsgType = "response"
	TypeDone            MsgType = "done"
	TypeError           MsgType = "error"
	TypeHistoryUser     MsgType = "history_user"
	TypeSetName         MsgType = "set_name"
	TypeSetInstanceName MsgType = "set_instance_name"

	// Turn progress.
	TypeToolStart     MsgType = "tool_start"
	TypeToolEnd       MsgType = "tool_end"
	TypeContextUpdate MsgType = "context_update"
	TypeResponseChunk MsgType = "response_chunk"
	TypeThinkingChunk MsgType = "thinking_chunk"
	TypeThinking      MsgType = "thinking"
	TypeSubAgentStart MsgType = "sub_agent_start"
	TypeSubAgentEnd   MsgType = "sub_agent_end"
	TypeStage         MsgType = "stage"
	TypePlanStart     MsgType = "plan_start"
	TypePlanEnd       MsgType = "plan_end"
	TypeNotice        MsgType = "notice"
)

// ServerMsgTypes is every message type the daemon may send to a client.
//
// The mirror of ClientMsgTypes, and it exists for the mirror-image failure: a
// type the daemon emits that no client renders is invisible in exactly the way
// an unhandled client message is not. The daemon gets an "unknown message type"
// reply; a client that ignores a progress event just quietly shows nothing, and
// the feature looks unimplemented rather than unwired.
//
// Kept in step with the const block above by the TUI's coverage test.
var ServerMsgTypes = []MsgType{
	TypeConversationID,
	TypeOK,
	TypeResponse,
	TypeDone,
	TypeError,
	TypeHistoryUser,
	TypeSetName,
	TypeSetInstanceName,
	TypeToolStart,
	TypeToolEnd,
	TypeContextUpdate,
	TypeResponseChunk,
	TypeThinkingChunk,
	TypeThinking,
	TypeSubAgentStart,
	TypeSubAgentEnd,
	TypeStage,
	TypePlanStart,
	TypePlanEnd,
	TypeNotice,
	TypeHumanInputRequired,
}

// ClientMsgTypes is every message type a client may send to the daemon. The
// daemon's dispatch switch must handle all of them; anything else reaches its
// default branch and comes back as "unknown message type".
//
// This exists so that requirement is testable rather than assumed: a constant
// added above but never wired into the switch is otherwise invisible until a
// client sends it. Keep it in step with the two const blocks above — the
// dispatch test is what makes forgetting it fail.
var ClientMsgTypes = []MsgType{
	TypeNewConversation,
	TypeAttach,
	TypeSetPlanMode,
	TypeUserTurn,
	TypeSessionStop,
	TypeSessionDelete,
	TypeStandingShow,
	TypeStandingControl,
	TypeToolCall,
	TypeWorkflowStop,
	TypeWorkflowFail,
	TypePluginCall,
	TypeStatus,
	TypeContext,
	TypeListGoals,
	TypeListWorkflows,
	TypeListNotifications,
	TypeListTools,
	TypePluginsList,
	TypePluginsReload,
	TypeSessionsList,
	TypeStandingList,
	TypeToolsList,
	TypeToolsReload,
	TypeHumanInputAnswer,
}

// Human-in-the-loop travels in both directions: the daemon asks, the client answers.
const (
	TypeHumanInputRequired MsgType = "human_input_required"
	TypeHumanInputAnswer   MsgType = "human_input_answer"
)

// Msg is the flat JSON envelope for all daemon ↔ client communication.
// Messages are exchanged as newline-delimited JSON (one object per line).
//
// The types below are named by the MsgType constants above; the list here
// documents which fields each one carries, which the envelope cannot express.
//
// Client → daemon:
//
//	"new_conversation" — create a new conversation; AgentID empty
//	"attach"           — reconnect to an existing conversation; AgentID set
//	"set_plan_mode"    — change reasoning mode live; AgentID + Text (off|plan-only|always)
//	"user_turn"        — send a message; AgentID + Text set
//	"status"           — request daemon status; no extra fields
//	"list_goals"        — request goal list; no extra fields
//	"list_workflows"    — request workflow list; no extra fields
//	"workflow_stop"    — cancel an active workflow; Text = workflow ID
//	"workflow_fail"    — mark workflow(s) as failed; Text = workflow ID or "--all"
//	"session_stop"     — terminate a session; AgentID set, or Text = "--all"
//	"plugins_list"     — request the plugin roster; no extra fields
//	"plugins_reload"   — re-scan the user-plugin dir and reload; no extra fields
//	"tools_list"       — request the sandboxed-tool roster; no extra fields
//	"tools_reload"     — re-scan the sandboxed-tool dir and reload; no extra fields
//
// Daemon → client:
//
//	"conversation_id"  — new conversation created; ID set
//	"ok"               — attach succeeded; AgentID echoed
//	"response"         — assistant reply; AgentID + Text set
//	"done"             — turn complete; AgentID set
//	"error"            — failure; Text carries the error message
//	"status"           — daemon status; Text carries JSON-encoded StatusInfo
//	"list_goals"        — goal list; Text carries JSON-encoded goal array
//	"list_workflows"    — workflow list; Text carries JSON-encoded workflow array
//	"workflow_stop"    — stop result; Text = "stopped"
//	"workflow_fail"    — fail result; Text = "failed"
//	"plugins_list"     — plugin roster; Text carries JSON-encoded PluginStatus array
//	"plugins_reload"   — reload result; Text carries the JSON-encoded PluginStatus array
//	"tools_list"       — sandboxed-tool roster; Text carries JSON-encoded SandboxedToolStatus array
//	"tools_reload"     — reload result; Text carries the JSON-encoded SandboxedToolStatus array
//	"tool_start"       — tool call started; ToolName + ToolInput + Timestamp set
//	"tool_end"         — tool call finished; ToolName + ToolInput + ToolOutput + Timestamp set
//	"context_update"   — context assembled; ContextUsed + ContextBudget set
//	"response_chunk"   — streamed text token; Text carries the chunk
//	"thinking_chunk"   — streamed reasoning token; Text carries the chunk
//	"sub_agent_start"  — a sub-agent was spawned; SubAgentID + Text (task) + Role + Timestamp set
//	"sub_agent_end"    — a sub-agent finished; SubAgentID + Text (task) + Status + Role + Timestamp set
//	"stage"            — the turn entered a named waiting phase; Text carries the label
type Msg struct {
	AgentID         string          `json:"agent_id,omitempty"`
	Type            MsgType         `json:"type"`
	Text            string          `json:"text,omitempty"`
	ID              string          `json:"id,omitempty"`
	Name            string          `json:"name,omitempty"`
	InstanceName    string          `json:"instance_name,omitempty"` // conversation_id/ok reply and set_instance_name event: the daemon's display name
	Role            string          `json:"role,omitempty"`          // conversation_id/ok reply: session's role; sub_agent_start/end: sub-agent's resolved leaf role
	ToolName        string          `json:"tool_name,omitempty"`
	ToolDisplayName string          `json:"tool_display_name,omitempty"`
	ToolInput       json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput      string          `json:"tool_output,omitempty"`
	Timestamp       int64           `json:"ts,omitempty"` // unix millis
	ContextUsed     int             `json:"context_used,omitempty"`
	ContextBudget   int             `json:"context_budget,omitempty"`
	SubAgentID      string          `json:"sub_agent_id,omitempty"`
	Status          string          `json:"status,omitempty"`     // sub_agent_end: "done" | "failed" | "timed_out"
	LLMCallN        int             `json:"llm_call_n,omitempty"` // thinking: 1-based LLM call count within the current turn
	Think           bool            `json:"think,omitempty"`      // thinking: this call requests native thinking and will stream thinking chunks
	// Limit bounds a listing reply — how many recent log lines standing_show
	// returns. Additive and omitempty, so a client that never sets it produces
	// exactly the bytes it produced before (R-PROTO.1).
	Limit int `json:"limit,omitempty"`

	// attach ok: recent tool events and last completed response for replay on reattach.
	ReplayEvents    []Msg  `json:"replay_events,omitempty"`
	PendingResponse string `json:"pending_response,omitempty"`

	// attach ok: the full multi-turn transcript (user prompts, tool activity, and
	// assistant responses, in order) so a reattaching client shows the whole
	// conversation as it was, not a blank screen. Takes precedence over
	// ReplayEvents/PendingResponse when present.
	History []Msg `json:"history,omitempty"`

	// new_conversation: marks the session interactive (HITL-eligible).
	Interactive bool `json:"interactive,omitempty"`

	// user_turn: forces native thinking / the analysis pass for this turn (/think)
	ForceThink bool `json:"force_think,omitempty"`

	// human-in-the-loop (see internal/runtime/hitl.go):
	//   human_input_required (daemon → client): RequestID, Question, Options, TimeoutSeconds, Origin
	//   human_input_answer   (client → daemon): AgentID, RequestID, Answer
	//
	// Origin attributes a question raised by a sub-agent rather than by the
	// session itself ("sub-agent \"researcher\" · <task>"). Empty when the
	// session's own loop is asking. AgentID stays the *owning* session — the one
	// whose progress stream carries the message and whose ID answers it — so a
	// client needs no knowledge of sub-agent IDs to reply.
	RequestID      string   `json:"request_id,omitempty"`
	Question       string   `json:"question,omitempty"`
	Options        []string `json:"options,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Answer         string   `json:"answer,omitempty"`
	Origin         string   `json:"origin,omitempty"`
}

// HumanRequest is a pending request for human input, surfaced to the TUI as
// part of a ProgressEvent. Origin is empty for a question from the session's
// own loop and names the sub-agent for a delegated one.
type HumanRequest struct {
	RequestID      string
	Question       string
	Options        []string
	TimeoutSeconds int
	Origin         string
}

// ProgressEvent is a structured progress update delivered to the TUI client
// during a turn. It is constructed by the client from streaming Msgs.
type ProgressEvent struct {
	Type            MsgType // "tool_start" | "tool_end" | "context_update" | "response_chunk" | "thinking_chunk" | "sub_agent_start" | "sub_agent_end" | "thinking" | "plan_start" | "plan_end" | "notice" | "set_instance_name"
	ToolName        string
	ToolDisplayName string
	ToolInput       json.RawMessage
	ToolOutput      string
	At              time.Time
	ContextUsed     int           // set when Type == "context_update"
	ContextBudget   int           // set when Type == "context_update"
	Text            string        // set when Type == "response_chunk"/"thinking_chunk" or "sub_agent_start"/"sub_agent_end" (task description)
	SubAgentID      string        // set when Type == "sub_agent_start" or "sub_agent_end"
	Status          string        // set when Type == "sub_agent_end"
	Role            string        // set when Type == "sub_agent_start"/"sub_agent_end": the sub-agent's resolved leaf role
	LLMCallN        int           // set when Type == "thinking"; 1-based LLM call count within the current turn
	Think           bool          // set when Type == "thinking"; the call streams reasoning (thinking_chunk)
	HumanRequest    *HumanRequest // set when Type == "human_input_required"
}

// SessionInfo describes one session for the "sessions_list" response.
//
// Protected says the retention reaper may not take it — the session has an
// active goal or an active session plan, so it is idle by design rather than
// abandoned. It is on the wire because an operator reading `nine sessions`
// needs to know why an old session is not being reaped.
type SessionInfo struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status"`
	AgeSeconds int    `json:"age_seconds"`
	Events     int    `json:"events,omitempty"`
	Protected  bool   `json:"protected,omitempty"`
	Attached   bool   `json:"attached,omitempty"`
}

// StandingInfo describes one standing run for the "standing_list" and
// "standing_show" responses. Recent is filled by show only — the roster stays
// one line per run.
type StandingInfo struct {
	ID         string            `json:"id"`
	Tool       string            `json:"tool"`
	State      string            `json:"state"`
	Trigger    string            `json:"trigger"`
	Calls      int               `json:"calls"`
	Cycles     int               `json:"cycles"`
	Failures   int               `json:"failures,omitempty"`
	LastError  string            `json:"last_error,omitempty"`
	LastCallAt string            `json:"last_call_at,omitempty"`
	NextAt     string            `json:"next_at,omitempty"`
	Generated  bool              `json:"generated,omitempty"`
	Recent     []StandingLogLine `json:"recent,omitempty"`
}

// StandingLogLine is one line of a standing run's recent activity.
type StandingLogLine struct {
	At      string `json:"at"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// ToolSummary describes one tool for the "list_tools" response.
type ToolSummary struct {
	Plugin      string `json:"plugin"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// PluginStatus describes one plugin for the "plugins_list" / "plugins_reload"
// responses. Source is "builtin" or "user". A loaded plugin carries its
// advertised tool names; a user plugin that failed to load carries Loaded=false
// and the skip reason in Error.
type PluginStatus struct {
	Name   string   `json:"name"`
	Source string   `json:"source"`
	Loaded bool     `json:"loaded"`
	Tools  []string `json:"tools,omitempty"`
	Error  string   `json:"error,omitempty"`

	// Disabled marks a plugin the operator switched off in [plugins].disabled
	// rather than one that failed to load — the difference between a decision
	// and a fault, which a roster must not blur. Additive and optional; a client
	// that does not know it sees an unloaded plugin with a reason in Error.
	Disabled bool `json:"disabled,omitempty"`
}

// SandboxedToolStatus describes one sandboxed tool for the "tools_list" /
// "tools_reload" responses (spec/contracts/toolvm.md).
//
// A tool that failed to load carries Loaded=false and the reason in Error, and
// that is the point of the message: §6.3 promises a loud failure rather than a
// tool that half-works, and this is where an operator reads it. Capabilities is
// the *resolved* grant — what the tool actually runs with, never what its
// manifest asked for.
type SandboxedToolStatus struct {
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"`
	Loaded bool   `json:"loaded"`
	// Generated marks a tool Nine wrote itself (docs/sandboxed-tools.md §5.2)
	// rather than one an operator installed from a file. It has no ManifestPath —
	// its code lives in the store — so the roster names the provenance directly.
	Generated    bool   `json:"generated,omitempty"`
	Capabilities string `json:"capabilities,omitempty"`
	Description  string `json:"description,omitempty"`
	ManifestPath string `json:"manifest_path,omitempty"`
	Error        string `json:"error,omitempty"`
	// Timeout is this tool's per-call deadline, as a duration string, set only
	// when `[tool.<name>] timeout` overrides the global one. Empty means it
	// inherits `[tools] timeout` — reported rather than inferred, because the
	// deadline is the only CPU bound the host has and an operator checking it
	// should not have to cross-reference two tables. Additive/optional field.
	Timeout string `json:"timeout,omitempty"`
	// Deps is the external npm packages a generated tool's bundle carries, as
	// "name@version" entries (docs/sandboxed-tools.md §4.4). Empty for a tool with
	// no external dependencies — the common case. Additive/optional field.
	Deps []string `json:"deps,omitempty"`
}

// SubAgentInfo describes one currently-running sub-agent.
type SubAgentInfo struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Role        string `json:"role,omitempty"` // resolved leaf role the sub-agent runs
}

// AgentInfo describes one active conversation in a StatusInfo response.
type AgentInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role,omitempty"` // resolved role the session runs
	PlanMode string `json:"plan_mode,omitempty"`
}

// QueueStat is the shared LLM queue's load, surfaced in daemon status.
type QueueStat struct {
	Pending       int `json:"pending"`        // requests waiting for a slot
	Inflight      int `json:"inflight"`       // requests executing now
	MaxConcurrent int `json:"max_concurrent"` // slot limit
}

// StatusInfo is the payload returned by the "status" protocol message.
type StatusInfo struct {
	Agents    []AgentInfo    `json:"agents"`
	SubAgents []SubAgentInfo `json:"sub_agents,omitempty"`
	Plugins   []string       `json:"plugins"`
	Uptime    string         `json:"uptime"`
	LLMQueue  *QueueStat     `json:"llm_queue,omitempty"`
}

// --- Client → daemon request constructors ---

// NewQueryMsg builds a request consisting of only a Type field — used for
// "new_conversation", "status", "list_goals",
// "list_workflows", "list_tools", "plugins_list", "plugins_reload",
// "tools_list", and "tools_reload".
func NewQueryMsg(msgType MsgType) Msg { return Msg{Type: msgType} }

// NewAttachMsg requests reattachment to an existing conversation.
func NewAttachMsg(agentID string) Msg {
	return Msg{Type: TypeAttach, AgentID: agentID}
}

// NewContextMsg requests a breakdown of a session's assembled context. The
// daemon replies with a "context" text message carrying a JSON ninectx.Report.
// It performs no LLM call.
func NewContextMsg(agentID string) Msg {
	return Msg{Type: TypeContext, AgentID: agentID}
}

// NewUserTurnMsg sends a user message to agentID.
func NewUserTurnMsg(agentID, text string) Msg {
	return Msg{Type: TypeUserTurn, AgentID: agentID, Text: text}
}

// NewWorkflowStopMsg requests cancellation of workflow id.
func NewWorkflowStopMsg(id string) Msg {
	return Msg{Type: TypeWorkflowStop, Text: id}
}

// NewWorkflowFailMsg requests marking workflow(s) as failed; text is a
// workflow ID or "--all".
func NewWorkflowFailMsg(text string) Msg {
	return Msg{Type: TypeWorkflowFail, Text: text}
}

// NewSessionStopMsg requests termination of a session. Pass an agent ID to stop
// one session, or all=true to stop every active session.
func NewSessionStopMsg(agentID string, all bool) Msg {
	if all {
		return Msg{Type: TypeSessionStop, Text: "--all"}
	}
	return Msg{Type: TypeSessionStop, AgentID: agentID}
}

// NewSessionDeleteMsg requests erasure of a session: the conversation and
// everything keyed to it.
//
// Deliberately a separate message from session_stop rather than a flag on it.
// Stop ends a session and archives it — the transcript and the journal survive.
// Delete destroys them, and an operation that irreversibly removes history
// should not be reachable by mistyping a boolean.
func NewSessionDeleteMsg(agentID string) Msg {
	return Msg{Type: TypeSessionDelete, AgentID: agentID}
}

// NewSessionsListMsg asks for the session roster.
func NewSessionsListMsg() Msg { return Msg{Type: TypeSessionsList} }

// NewStandingShowMsg asks for one standing run in detail, with up to n recent
// log lines.
func NewStandingShowMsg(id string, n int) Msg {
	return Msg{Type: TypeStandingShow, AgentID: id, Limit: n}
}

// NewStandingControlMsg stops or starts a standing run. action is "stop" or
// "start".
func NewStandingControlMsg(id, action string) Msg {
	return Msg{Type: TypeStandingControl, AgentID: id, Text: action}
}

// NewStandingListMsg asks for the standing-run roster.
func NewStandingListMsg() Msg { return Msg{Type: TypeStandingList} }

// NewToolCallMsg invokes one tool once, for testing.
//
// Distinct from plugin_call: this returns the *whole* envelope, including a
// `continue` a resumable tool produced, and it runs against scratch state unless
// liveState is set — testing a tool must not be able to overwrite the cursor of
// a live standing run.
func NewToolCallMsg(tool string, args json.RawMessage, liveState bool) Msg {
	m := Msg{Type: TypeToolCall, ToolName: tool, ToolInput: args}
	if liveState {
		m.Text = "--live-state"
	}
	return m
}

// NewPluginCallMsg invokes a tool directly, bypassing the LLM agent. Despite
// the name it is not plugin-only: the daemon resolves the name against the
// core-intercepted tools (memory/file/skill/doc) first, then the plugin roster.
func NewPluginCallMsg(tool string, args json.RawMessage) Msg {
	return Msg{Type: TypePluginCall, ToolName: tool, ToolInput: args}
}

// --- Daemon → client response constructors ---

// NewErrorMsg builds a generic error response.
func NewErrorMsg(text string) Msg {
	return Msg{Type: TypeError, Text: text}
}

// NewAgentErrorMsg builds an error response scoped to one conversation.
func NewAgentErrorMsg(agentID, text string) Msg {
	return Msg{Type: TypeError, AgentID: agentID, Text: text}
}

// NewConversationIDMsg announces a newly created conversation.
func NewConversationIDMsg(id string) Msg {
	return Msg{Type: TypeConversationID, ID: id}
}

// NewOKMsg acknowledges a successful attach.
func NewOKMsg(agentID, name string) Msg {
	return Msg{Type: TypeOK, AgentID: agentID, Name: name}
}

// NewOKMsgWithReplay acknowledges a successful attach and includes recent
// session events and the last completed response for the TUI to replay.
func NewOKMsgWithReplay(agentID, name string, replay []Msg, pendingResponse string) Msg {
	return Msg{
		Type:            TypeOK,
		AgentID:         agentID,
		Name:            name,
		ReplayEvents:    replay,
		PendingResponse: pendingResponse,
	}
}

// AttachResult is returned by Client.Attach. It carries the resolved agent ID,
// display name, and any session events that occurred while no client was
// connected (replay) plus the last completed response. History, when present,
// is the full prior transcript for the client to render on reattach.
type AttachResult struct {
	AgentID         string
	Name            string
	InstanceName    string
	Role            string
	ReplayEvents    []Msg
	PendingResponse string
	History         []Msg
}

// NewHistoryUserMsg marks a past user prompt in an attach transcript. It is
// distinct from "user_turn" (client → daemon) so the client can tell a
// replayed prompt from a live one.
func NewHistoryUserMsg(agentID, text string) Msg {
	return Msg{Type: TypeHistoryUser, AgentID: agentID, Text: text}
}

// NewTextMsg builds a response carrying msgType and a text/JSON payload —
// used for "status", "list_goals", "list_workflows",
// "list_tools", "plugin_call", "workflow_stop", and "workflow_fail" responses.
func NewTextMsg(msgType MsgType, text string) Msg {
	return Msg{Type: msgType, Text: text}
}

// NewSetNameMsg announces the inferred display name for a conversation.
func NewSetNameMsg(agentID, name string) Msg {
	return Msg{Type: TypeSetName, AgentID: agentID, Name: name}
}

// NewSetInstanceNameMsg announces the daemon's display name (instance name),
// broadcast to active sessions when it is resolved or updated asynchronously.
func NewSetInstanceNameMsg(name string) Msg {
	return Msg{Type: TypeSetInstanceName, InstanceName: name}
}

// NewResponseMsg carries the assistant's final reply for a turn.
func NewResponseMsg(agentID, text string) Msg {
	return Msg{Type: TypeResponse, AgentID: agentID, Text: text}
}

// NewDoneMsg signals that a turn has completed.
func NewDoneMsg(agentID string) Msg {
	return Msg{Type: TypeDone, AgentID: agentID}
}

// --- Streaming progress constructors (session worker) ---

// NewToolStartMsg announces that a tool call has started.
func NewToolStartMsg(agentID, toolName, toolDisplayName string, toolInput json.RawMessage) Msg {
	return Msg{
		Type:            TypeToolStart,
		AgentID:         agentID,
		ToolName:        toolName,
		ToolDisplayName: toolDisplayName,
		ToolInput:       toolInput,
		Timestamp:       time.Now().UnixMilli(),
	}
}

// NewToolEndMsg announces that a tool call has finished.
func NewToolEndMsg(agentID, toolName, toolDisplayName string, toolInput json.RawMessage, toolOutput string) Msg {
	return Msg{
		Type:            TypeToolEnd,
		AgentID:         agentID,
		ToolName:        toolName,
		ToolDisplayName: toolDisplayName,
		ToolInput:       toolInput,
		ToolOutput:      toolOutput,
		Timestamp:       time.Now().UnixMilli(),
	}
}

// NewContextUpdateMsg reports updated context-window usage.
func NewContextUpdateMsg(agentID string, used, budget int) Msg {
	return Msg{
		Type:          TypeContextUpdate,
		AgentID:       agentID,
		ContextUsed:   used,
		ContextBudget: budget,
	}
}

// NewResponseChunkMsg carries one streamed token of the assistant's reply.
func NewResponseChunkMsg(agentID, text string) Msg {
	return Msg{
		Type:      TypeResponseChunk,
		AgentID:   agentID,
		Text:      text,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewThinkingChunkMsg carries one streamed reasoning ("thinking") token. Like
// response_chunk it is ephemeral — a live trace, never journaled.
func NewThinkingChunkMsg(agentID, text string) Msg {
	return Msg{
		Type:      TypeThinkingChunk,
		AgentID:   agentID,
		Text:      text,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewSubAgentStartMsg announces that a sub-agent was spawned to work on task.
// role is the sub-agent's resolved leaf role (e.g. "software-dev"), shown in
// the TUI so the user can see which kind of agent is doing the work.
func NewSubAgentStartMsg(agentID, subAgentID, task, role string) Msg {
	return Msg{
		Type:       TypeSubAgentStart,
		AgentID:    agentID,
		SubAgentID: subAgentID,
		Text:       task,
		Role:       role,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewSubAgentEndMsg announces that a sub-agent finished working on task.
// status is "done", "failed", or "timed_out"; role is its resolved leaf role.
func NewSubAgentEndMsg(agentID, subAgentID, task, status, role string) Msg {
	return Msg{
		Type:       TypeSubAgentEnd,
		AgentID:    agentID,
		SubAgentID: subAgentID,
		Text:       task,
		Status:     status,
		Role:       role,
		Timestamp:  time.Now().UnixMilli(),
	}
}

// NewHumanInputRequiredMsg announces that the agent is blocked waiting for
// human input. Delivered on the owning session's progress stream; agentID is
// that session, even when a sub-agent of it is the one asking (origin names
// the sub-agent then, and is empty otherwise).
func NewHumanInputRequiredMsg(agentID, requestID, question string, options []string, timeoutSeconds int, origin string) Msg {
	return Msg{
		Type:           TypeHumanInputRequired,
		AgentID:        agentID,
		RequestID:      requestID,
		Question:       question,
		Options:        options,
		TimeoutSeconds: timeoutSeconds,
		Origin:         origin,
		Timestamp:      time.Now().UnixMilli(),
	}
}

// NewHumanInputAnswerMsg carries a human's answer back to the daemon. It is a
// top-level message, not a turn — the daemon routes it to the waiting
// ask_human call and does not start a new agent loop.
func NewHumanInputAnswerMsg(agentID, requestID, answer string) Msg {
	return Msg{
		Type:      TypeHumanInputAnswer,
		AgentID:   agentID,
		RequestID: requestID,
		Answer:    answer,
	}
}

// NewThinkingMsg announces the start of an inner-loop LLM call. llmCallN is
// 1-based: 1 on the first call of a turn, 2 on the second, and so on. think
// reports whether this call will stream reasoning — false means the model was
// asked not to think, or can't.
func NewThinkingMsg(agentID string, llmCallN int, think bool) Msg {
	return Msg{
		Type:     TypeThinking,
		AgentID:  agentID,
		LLMCallN: llmCallN,
		Think:    think,
	}
}

// NewPlanStartMsg announces that the no-tool request-analysis (planning) pass
// has begun — the graceful-degradation substitute for native thinking on models
// that lack it. Surfaced in the TUI like thinking; ephemeral, never journaled.
func NewPlanStartMsg(agentID string) Msg {
	return Msg{Type: TypePlanStart, AgentID: agentID, Timestamp: time.Now().UnixMilli()}
}

// NewStageMsg announces that the turn entered a named waiting phase — memory
// recall, context assembly, a queue wait, or waiting on the model's first
// token. It fills the dead air before reasoning starts. An empty label clears
// the current phase. Ephemeral, never journaled; clients that don't recognise
// it ignore it.
func NewStageMsg(agentID, label string) Msg {
	return Msg{Type: TypeStage, AgentID: agentID, Text: label, Timestamp: time.Now().UnixMilli()}
}

// NewPlanEndMsg announces that the request-analysis pass has finished.
func NewPlanEndMsg(agentID string) Msg {
	return Msg{Type: TypePlanEnd, AgentID: agentID, Timestamp: time.Now().UnixMilli()}
}

// NewNoticeMsg carries a session-level notice for the user — e.g. a capability
// downgrade (native thinking unavailable, planning pass used instead).
// Informational and ephemeral; never journaled. Distinct from the user
// notification tool (RegisterNotifyUser), which is a durable, model-invoked feed.
func NewNoticeMsg(agentID, text string) Msg {
	return Msg{Type: TypeNotice, AgentID: agentID, Text: text, Timestamp: time.Now().UnixMilli()}
}

// ToProgressEvent converts a streaming Msg ("tool_start", "tool_end",
// "context_update", "response_chunk", "thinking_chunk", "set_name",
// "sub_agent_start", "sub_agent_end") into a ProgressEvent for the TUI client.
// ok is false for message types that aren't progress events.
func (m Msg) ToProgressEvent() (ProgressEvent, bool) {
	switch m.Type {
	case TypeToolStart, TypeToolEnd:
		at := time.Now()
		if m.Timestamp > 0 {
			at = time.UnixMilli(m.Timestamp)
		}
		return ProgressEvent{
			Type:            m.Type,
			ToolName:        m.ToolName,
			ToolDisplayName: m.ToolDisplayName,
			ToolInput:       m.ToolInput,
			ToolOutput:      m.ToolOutput,
			At:              at,
		}, true
	case TypeContextUpdate:
		return ProgressEvent{
			Type:          m.Type,
			ContextUsed:   m.ContextUsed,
			ContextBudget: m.ContextBudget,
		}, true
	case TypeResponseChunk, TypeThinkingChunk:
		return ProgressEvent{Type: m.Type, Text: m.Text}, true
	case TypeSetName:
		return ProgressEvent{Type: m.Type, Text: m.Name}, true
	case TypeSetInstanceName:
		return ProgressEvent{Type: m.Type, Text: m.InstanceName}, true
	case TypeSubAgentStart, TypeSubAgentEnd:
		at := time.Now()
		if m.Timestamp > 0 {
			at = time.UnixMilli(m.Timestamp)
		}
		return ProgressEvent{
			Type:       m.Type,
			SubAgentID: m.SubAgentID,
			Text:       m.Text,
			Status:     m.Status,
			Role:       m.Role,
			At:         at,
		}, true
	case TypeThinking:
		return ProgressEvent{Type: m.Type, LLMCallN: m.LLMCallN, Think: m.Think}, true
	case TypePlanStart, TypePlanEnd:
		return ProgressEvent{Type: m.Type}, true
	case TypeNotice, TypeStage:
		return ProgressEvent{Type: m.Type, Text: m.Text}, true
	case TypeHumanInputRequired:
		return ProgressEvent{
			Type: m.Type,
			HumanRequest: &HumanRequest{
				RequestID:      m.RequestID,
				Question:       m.Question,
				Options:        m.Options,
				TimeoutSeconds: m.TimeoutSeconds,
				Origin:         m.Origin,
			},
		}, true
	default:
		return ProgressEvent{}, false
	}
}

// NewSetPlanModeMsg builds a set_plan_mode command; the new mode rides in Text.
func NewSetPlanModeMsg(agentID, mode string) Msg {
	return Msg{Type: TypeSetPlanMode, AgentID: agentID, Text: mode}
}
