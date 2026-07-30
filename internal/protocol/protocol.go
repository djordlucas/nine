package protocol

import (
	"encoding/json"
	"time"
)

// Msg is the flat JSON envelope for all daemon ↔ client communication.
// Messages are exchanged as newline-delimited JSON (one object per line).
//
// Client → daemon:
//
//	"new_conversation" — create a new conversation; AgentID empty
//	"attach"           — reconnect to an existing conversation; AgentID set
//	"set_plan_mode"    — change reasoning mode live; AgentID + Text (off|plan-only|always)
//	"user_turn"        — send a message; AgentID + Text set
//	"status"           — request daemon status; no extra fields
//	"list_goals"        — request goal list; no extra fields
//	"list_reflections"  — request reflection history; no extra fields
//	"list_workflows"    — request workflow list; no extra fields
//	"workflow_stop"    — cancel an active workflow; Text = workflow ID
//	"workflow_fail"    — mark workflow(s) as failed; Text = workflow ID or "--all"
//	"session_stop"     — terminate a session; AgentID set, or Text = "--all"
//	"plugins_list"     — request the plugin roster; no extra fields
//	"plugins_reload"   — re-scan the user-plugin dir and reload; no extra fields
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
//	"list_reflections"  — reflection history; Text carries JSON-encoded reflection array
//	"list_workflows"    — workflow list; Text carries JSON-encoded workflow array
//	"workflow_stop"    — stop result; Text = "stopped"
//	"workflow_fail"    — fail result; Text = "failed"
//	"plugins_list"     — plugin roster; Text carries JSON-encoded PluginStatus array
//	"plugins_reload"   — reload result; Text carries the JSON-encoded PluginStatus array
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
	Type            string          `json:"type"`
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
	Type            string // "tool_start" | "tool_end" | "context_update" | "response_chunk" | "thinking_chunk" | "sub_agent_start" | "sub_agent_end" | "thinking" | "plan_start" | "plan_end" | "notice" | "set_instance_name"
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
// "new_conversation", "status", "list_goals", "list_reflections",
// "list_workflows", "list_tools", "plugins_list", and "plugins_reload".
func NewQueryMsg(msgType string) Msg { return Msg{Type: msgType} }

// NewAttachMsg requests reattachment to an existing conversation.
func NewAttachMsg(agentID string) Msg {
	return Msg{Type: "attach", AgentID: agentID}
}

// NewContextMsg requests a breakdown of a session's assembled context. The
// daemon replies with a "context" text message carrying a JSON ninectx.Report.
// It performs no LLM call.
func NewContextMsg(agentID string) Msg {
	return Msg{Type: "context", AgentID: agentID}
}

// NewUserTurnMsg sends a user message to agentID.
func NewUserTurnMsg(agentID, text string) Msg {
	return Msg{Type: "user_turn", AgentID: agentID, Text: text}
}

// NewWorkflowStopMsg requests cancellation of workflow id.
func NewWorkflowStopMsg(id string) Msg {
	return Msg{Type: "workflow_stop", Text: id}
}

// NewWorkflowFailMsg requests marking workflow(s) as failed; text is a
// workflow ID or "--all".
func NewWorkflowFailMsg(text string) Msg {
	return Msg{Type: "workflow_fail", Text: text}
}

// NewSessionStopMsg requests termination of a session. Pass an agent ID to stop
// one session, or all=true to stop every active session.
func NewSessionStopMsg(agentID string, all bool) Msg {
	if all {
		return Msg{Type: "session_stop", Text: "--all"}
	}
	return Msg{Type: "session_stop", AgentID: agentID}
}

// NewPluginCallMsg invokes a plugin tool directly, bypassing the LLM agent.
func NewPluginCallMsg(tool string, args json.RawMessage) Msg {
	return Msg{Type: "plugin_call", ToolName: tool, ToolInput: args}
}

// --- Daemon → client response constructors ---

// NewErrorMsg builds a generic error response.
func NewErrorMsg(text string) Msg {
	return Msg{Type: "error", Text: text}
}

// NewAgentErrorMsg builds an error response scoped to one conversation.
func NewAgentErrorMsg(agentID, text string) Msg {
	return Msg{Type: "error", AgentID: agentID, Text: text}
}

// NewConversationIDMsg announces a newly created conversation.
func NewConversationIDMsg(id string) Msg {
	return Msg{Type: "conversation_id", ID: id}
}

// NewOKMsg acknowledges a successful attach.
func NewOKMsg(agentID, name string) Msg {
	return Msg{Type: "ok", AgentID: agentID, Name: name}
}

// NewOKMsgWithReplay acknowledges a successful attach and includes recent
// session events and the last completed response for the TUI to replay.
func NewOKMsgWithReplay(agentID, name string, replay []Msg, pendingResponse string) Msg {
	return Msg{
		Type:            "ok",
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
	return Msg{Type: "history_user", AgentID: agentID, Text: text}
}

// NewTextMsg builds a response carrying msgType and a text/JSON payload —
// used for "status", "list_goals", "list_reflections", "list_workflows",
// "list_tools", "plugin_call", "workflow_stop", and "workflow_fail" responses.
func NewTextMsg(msgType, text string) Msg {
	return Msg{Type: msgType, Text: text}
}

// NewSetNameMsg announces the inferred display name for a conversation.
func NewSetNameMsg(agentID, name string) Msg {
	return Msg{Type: "set_name", AgentID: agentID, Name: name}
}

// NewSetInstanceNameMsg announces the daemon's display name (instance name),
// broadcast to active sessions when it is resolved or updated asynchronously.
func NewSetInstanceNameMsg(name string) Msg {
	return Msg{Type: "set_instance_name", InstanceName: name}
}

// NewResponseMsg carries the assistant's final reply for a turn.
func NewResponseMsg(agentID, text string) Msg {
	return Msg{Type: "response", AgentID: agentID, Text: text}
}

// NewDoneMsg signals that a turn has completed.
func NewDoneMsg(agentID string) Msg {
	return Msg{Type: "done", AgentID: agentID}
}

// --- Streaming progress constructors (session worker) ---

// NewToolStartMsg announces that a tool call has started.
func NewToolStartMsg(agentID, toolName, toolDisplayName string, toolInput json.RawMessage) Msg {
	return Msg{
		Type:            "tool_start",
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
		Type:            "tool_end",
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
		Type:          "context_update",
		AgentID:       agentID,
		ContextUsed:   used,
		ContextBudget: budget,
	}
}

// NewResponseChunkMsg carries one streamed token of the assistant's reply.
func NewResponseChunkMsg(agentID, text string) Msg {
	return Msg{
		Type:      "response_chunk",
		AgentID:   agentID,
		Text:      text,
		Timestamp: time.Now().UnixMilli(),
	}
}

// NewThinkingChunkMsg carries one streamed reasoning ("thinking") token. Like
// response_chunk it is ephemeral — a live trace, never journaled.
func NewThinkingChunkMsg(agentID, text string) Msg {
	return Msg{
		Type:      "thinking_chunk",
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
		Type:       "sub_agent_start",
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
		Type:       "sub_agent_end",
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
		Type:           "human_input_required",
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
		Type:      "human_input_answer",
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
		Type:     "thinking",
		AgentID:  agentID,
		LLMCallN: llmCallN,
		Think:    think,
	}
}

// NewPlanStartMsg announces that the no-tool request-analysis (planning) pass
// has begun — the graceful-degradation substitute for native thinking on models
// that lack it. Surfaced in the TUI like thinking; ephemeral, never journaled.
func NewPlanStartMsg(agentID string) Msg {
	return Msg{Type: "plan_start", AgentID: agentID, Timestamp: time.Now().UnixMilli()}
}

// NewStageMsg announces that the turn entered a named waiting phase — memory
// recall, context assembly, a queue wait, or waiting on the model's first
// token. It fills the dead air before reasoning starts. An empty label clears
// the current phase. Ephemeral, never journaled; clients that don't recognise
// it ignore it.
func NewStageMsg(agentID, label string) Msg {
	return Msg{Type: "stage", AgentID: agentID, Text: label, Timestamp: time.Now().UnixMilli()}
}

// NewPlanEndMsg announces that the request-analysis pass has finished.
func NewPlanEndMsg(agentID string) Msg {
	return Msg{Type: "plan_end", AgentID: agentID, Timestamp: time.Now().UnixMilli()}
}

// NewNoticeMsg carries a session-level notice for the user — e.g. a capability
// downgrade (native thinking unavailable, planning pass used instead).
// Informational and ephemeral; never journaled. Distinct from the user
// notification tool (RegisterNotifyUser), which is a durable, model-invoked feed.
func NewNoticeMsg(agentID, text string) Msg {
	return Msg{Type: "notice", AgentID: agentID, Text: text, Timestamp: time.Now().UnixMilli()}
}

// ToProgressEvent converts a streaming Msg ("tool_start", "tool_end",
// "context_update", "response_chunk", "thinking_chunk", "set_name",
// "sub_agent_start", "sub_agent_end") into a ProgressEvent for the TUI client.
// ok is false for message types that aren't progress events.
func (m Msg) ToProgressEvent() (ProgressEvent, bool) {
	switch m.Type {
	case "tool_start", "tool_end":
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
	case "context_update":
		return ProgressEvent{
			Type:          m.Type,
			ContextUsed:   m.ContextUsed,
			ContextBudget: m.ContextBudget,
		}, true
	case "response_chunk", "thinking_chunk":
		return ProgressEvent{Type: m.Type, Text: m.Text}, true
	case "set_name":
		return ProgressEvent{Type: m.Type, Text: m.Name}, true
	case "set_instance_name":
		return ProgressEvent{Type: m.Type, Text: m.InstanceName}, true
	case "sub_agent_start", "sub_agent_end":
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
	case "thinking":
		return ProgressEvent{Type: m.Type, LLMCallN: m.LLMCallN, Think: m.Think}, true
	case "plan_start", "plan_end":
		return ProgressEvent{Type: m.Type}, true
	case "notice", "stage":
		return ProgressEvent{Type: m.Type, Text: m.Text}, true
	case "human_input_required":
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
	return Msg{Type: "set_plan_mode", AgentID: agentID, Text: mode}
}
