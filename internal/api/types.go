// Package api provides types for the Nine HTTP API layer.
// These types define the request and response structures for all API endpoints,
// replacing map[string]any with strongly-typed structs.
package api

import "time"

// ErrorResponse is the standard error response format.
type ErrorResponse struct {
	Error ErrorDetails `json:"error"`
}

// ErrorDetails contains error information.
type ErrorDetails struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// =============================================================================
// Health and Status Types
// =============================================================================

// HealthResponse is the response for GET /api/v1/health
type HealthResponse struct {
	Status          string `json:"status"`
	DaemonConnected bool   `json:"daemon_connected"`
	UptimeSeconds   int    `json:"uptime_seconds"`
	Version         string `json:"version"`
}

// StatusResponse is the response for GET /api/v1/status
type StatusResponse struct {
	Status      string                 `json:"status"`
	Version     string                 `json:"version"`
	Uptime      string                 `json:"uptime"`
	Sessions    int                    `json:"active_sessions"`
	Plugins     []PluginInfo           `json:"plugins"`
	Tools       []ToolInfo             `json:"tools"`
	MemoryStats MemoryStats            `json:"memory"`
	Config      map[string]interface{} `json:"config"`
}

// MemoryStats contains memory database statistics.
type MemoryStats struct {
	EventsCount    int `json:"events_count"`
	MemoriesCount  int `json:"memories_count"`
	WorkflowsCount int `json:"workflows_count"`
}

// =============================================================================
// Conversation Types
// =============================================================================

// CreateConversationRequest is the request for POST /api/v1/conversations
type CreateConversationRequest struct {
	Interactive bool `json:"interactive"`
}

// CreateConversationResponse is the response for POST /api/v1/conversations
type CreateConversationResponse struct {
	ID           string    `json:"id"`
	Role         string    `json:"role"`
	InstanceName string    `json:"instance_name"`
	CreatedAt    time.Time `json:"created_at"`
}

// ConversationInfo contains information about a conversation/session
type ConversationInfo struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	PlanMode      string    `json:"plan_mode"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	EventsCount   int       `json:"events_count"`
	Protected     bool      `json:"protected"`
	Attached      bool      `json:"attached"`
	AgeSeconds    int       `json:"age_seconds"`
}

// ListConversationsResponse is the response for GET /api/v1/conversations
type ListConversationsResponse struct {
	Data       []ConversationInfo `json:"data"`
	Pagination Pagination         `json:"pagination,omitempty"`
}

// GetConversationResponse is the response for GET /api/v1/conversations/{id}
type GetConversationResponse struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Role        string          `json:"role"`
	PlanMode    string          `json:"plan_mode"`
	Status      string          `json:"status"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
	EventsCount int             `json:"events_count"`
	Protected   bool            `json:"protected"`
	Context     map[string]any  `json:"context,omitempty"`
}

// SendMessageRequest is the request for POST /api/v1/conversations/{id}/messages
type SendMessageRequest struct {
	Text       string `json:"text"`
	ForceThink bool   `json:"force_think"`
}

// SendMessageResponse is the response for POST /api/v1/conversations/{id}/messages
type SendMessageResponse struct {
	AgentID     string    `json:"agent_id"`
	Text        string    `json:"text"`
	CompletedAt time.Time `json:"completed_at"`
	TurnID      string    `json:"turn_id,omitempty"`
}

// GetContextResponse is the response for GET /api/v1/conversations/{id}/context
type GetContextResponse struct {
	AgentID  string    `json:"agent_id"`
	Context any       `json:"context"`
}

// DeleteConversationResponse is the response for DELETE /api/v1/conversations/{id}
type DeleteConversationResponse struct {
	Message    string `json:"message"`
	ID         string `json:"id"`
	DeletedEvents int    `json:"deleted_events,omitempty"`
}

// StopConversationResponse is the response for POST /api/v1/conversations/{id}/stop
type StopConversationResponse struct {
	Message string `json:"message"`
	Status  string `json:"status"`
	ID      string `json:"id"`
}

// =============================================================================
// History and Trace Types
// =============================================================================

// HistoryEntry represents a single history entry
type HistoryEntry struct {
	Type        string    `json:"type"` // user_turn, response, tool_start, tool_end
	AgentID     string    `json:"agent_id"`
	Text        string    `json:"text,omitempty"`
	ToolName    string    `json:"tool_name,omitempty"`
	ToolInput   any       `json:"tool_input,omitempty"`
	ToolOutput  string    `json:"tool_output,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	TurnNumber  int       `json:"turn_number"`
}

// GetHistoryResponse is the response for GET /api/v1/conversations/{id}/history
type GetHistoryResponse struct {
	AgentID    string         `json:"agent_id"`
	Data       []HistoryEntry `json:"data"`
	Pagination Pagination      `json:"pagination,omitempty"`
}

// GetTraceResponse is the response for GET /api/v1/conversations/{id}/trace
type GetTraceResponse struct {
	AgentID    string `json:"agent_id"`
	Turn       int    `json:"turn"`
	SubAgents bool   `json:"sub_agents"`
	Trace     any    `json:"trace"`
}

// ReplayRequest is the request for POST /api/v1/conversations/{id}/replay
type ReplayRequest struct {
	Turn int `json:"turn"`
}

// ReplayResponse is the response for POST /api/v1/conversations/{id}/replay
type ReplayResponse struct {
	AgentID string `json:"agent_id"`
	Turn    int    `json:"turn"`
	Result  any    `json:"result"`
}

// =============================================================================
// Goal Types
// =============================================================================

// GoalInfo is a goal as the daemon stores it. The fields mirror memory.Goal —
// a goal carries no name, priority, owning session or progress note, and
// earlier revisions of this type advertised all four.
type GoalInfo struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Status      string    `json:"status"`
	ParentID    string    `json:"parent_id,omitempty"`
	ParentType  string    `json:"parent_type,omitempty"`
	Subtree     []string  `json:"subtree,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// CreateGoalRequest is the request for POST /api/v1/goals
type CreateGoalRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Priority    int    `json:"priority"`
}

// CreateGoalResponse is the response for POST /api/v1/goals
type CreateGoalResponse struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	SessionID string `json:"session_id,omitempty"`
}

// ListGoalsResponse is the response for GET /api/v1/goals
type ListGoalsResponse struct {
	Data       []GoalInfo   `json:"data"`
	Pagination Pagination   `json:"pagination,omitempty"`
}

// GetGoalResponse is the response for GET /api/v1/goals/{id}. One goal has the
// same shape as a goal in a list.
type GetGoalResponse = GoalInfo

// DeleteGoalResponse is the response for DELETE /api/v1/goals/{id}
type DeleteGoalResponse struct {
	Message string `json:"message"`
	ID      string `json:"id"`
}

// =============================================================================
// Workflow Types
// =============================================================================

// WorkflowInfo is a workflow as the daemon stores it (workflow.Workflow).
// Steps and CompletedSteps summarise the stored step list, which the daemon
// sends in full; CurrentStep is not a thing the store records.
type WorkflowInfo struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Status         string    `json:"status"` // active, done, failed, cancelled
	AgentID        string    `json:"agent_id,omitempty"`
	Steps          int       `json:"steps"`
	CompletedSteps int       `json:"completed_steps"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ListWorkflowsResponse is the response for GET /api/v1/workflows
type ListWorkflowsResponse struct {
	Data       []WorkflowInfo `json:"data"`
	Pagination Pagination      `json:"pagination,omitempty"`
}

// StopWorkflowResponse is the response for POST /api/v1/workflows/{id}/stop
type StopWorkflowResponse struct {
	Message string `json:"message"`
	ID      string `json:"id"`
	Status  string `json:"status"`
}

// FailWorkflowResponse is the response for POST /api/v1/workflows/{id}/fail
type FailWorkflowResponse struct {
	Message string `json:"message"`
	ID      string `json:"id"`
	Status  string `json:"status"`
}

// =============================================================================
// Tool Types
// =============================================================================

// ToolInfo contains information about a tool
type ToolInfo struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Plugin       string   `json:"plugin,omitempty"`
	Kind         string   `json:"kind"` // plugin, sandboxed, generated
	Loaded       bool     `json:"loaded"`
	Capabilities []string `json:"capabilities,omitempty"`
	Error        string   `json:"error,omitempty"`
	ManifestPath string   `json:"manifest_path,omitempty"`
	Generated    bool     `json:"generated,omitempty"`
	InputSchema  any      `json:"input_schema,omitempty"`
}

// ListToolsResponse is the response for GET /api/v1/tools
type ListToolsResponse struct {
	Data       []ToolInfo `json:"data"`
	Pagination Pagination  `json:"pagination,omitempty"`
}

// CallToolRequest is the request for POST /api/v1/tools/{name}/call
type CallToolRequest struct {
	Args      any `json:"args"`
	LiveState bool `json:"live_state"`
}

// CallToolResponse is the response for POST /api/v1/tools/{name}/call
type CallToolResponse struct {
	ToolName  string `json:"tool_name"`
	Output    string `json:"output"`
	DurationMS int    `json:"duration_ms,omitempty"`
	Success   bool   `json:"success"`
}

// GetToolResponse is the response for GET /api/v1/tools/{name}
type GetToolResponse struct {
	Name         string   `json:"name"`
	Description  string   `json:"description"`
	Plugin       string   `json:"plugin,omitempty"`
	Kind         string   `json:"kind"`
	Loaded       bool     `json:"loaded"`
	InputSchema  any      `json:"input_schema,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	ManifestPath string   `json:"manifest_path,omitempty"`
	Generated    bool     `json:"generated,omitempty"`
}

// ReloadToolsResponse is the response for POST /api/v1/tools/reload
type ReloadToolsResponse struct {
	Message string   `json:"message"`
	Tools   []ToolInfo `json:"tools"`
}

// =============================================================================
// Plugin Types
// =============================================================================

// PluginInfo contains information about a plugin
type PluginInfo struct {
	Name     string   `json:"name"`
	Source   string   `json:"source"` // builtin, user
	Loaded   bool     `json:"loaded"`
	Tools    []string `json:"tools,omitempty"`
	Error    string   `json:"error,omitempty"`
	Disabled bool     `json:"disabled"`
}

// ListPluginsResponse is the response for GET /api/v1/plugins
type ListPluginsResponse struct {
	Data       []PluginInfo `json:"data"`
	Pagination Pagination    `json:"pagination,omitempty"`
}

// ReloadPluginsResponse is the response for POST /api/v1/plugins/reload
type ReloadPluginsResponse struct {
	Message      string   `json:"message"`
	LoadedCount  int      `json:"loaded_count"`
	FailedCount  int      `json:"failed_count"`
	Plugins      []PluginInfo `json:"plugins"`
}

// =============================================================================
// Notification Types
// =============================================================================

// Notification is one entry of the human-facing feed
// (memory.UserNotification). The store records no title, severity or type.
type Notification struct {
	ID        string    `json:"id"`
	AgentID   string    `json:"agent_id,omitempty"`
	Message   string    `json:"message"`
	Seen      bool      `json:"seen"`
	CreatedAt time.Time `json:"created_at"`
}

// ListNotificationsResponse is the response for GET /api/v1/notifications
type ListNotificationsResponse struct {
	Data       []Notification `json:"data"`
	Pagination Pagination      `json:"pagination,omitempty"`
}

// =============================================================================
// Skill Types
// =============================================================================

// SkillInfo contains information about a skill
type SkillInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Source      string   `json:"source"` // builtin, user, generated
	CreatedAt   time.Time `json:"created_at"`
}

// ListSkillsResponse is the response for GET /api/v1/skills
type ListSkillsResponse struct {
	Data       []SkillInfo `json:"data"`
	Pagination Pagination   `json:"pagination,omitempty"`
}

// =============================================================================
// Session Types
// =============================================================================

// AttachSessionRequest is the request for POST /api/v1/sessions/attach
type AttachSessionRequest struct {
	AgentID string `json:"agent_id"`
}

// AttachSessionResponse is the response for POST /api/v1/sessions/attach
type AttachSessionResponse struct {
	AgentID        string `json:"agent_id"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	InstanceName  string `json:"instance_name"`
	ReplayEvents  []any   `json:"replay_events,omitempty"`
	PendingResponse string `json:"pending_response,omitempty"`
	History       []any   `json:"history,omitempty"`
}

// =============================================================================
// Documentation Types
// =============================================================================

// ListDocsResponse is the response for GET /api/v1/docs
type ListDocsResponse struct {
	Topics []string `json:"topics"`
}

// GetDocsResponse is the response for GET /api/v1/docs/{topic}
type GetDocsResponse struct {
	Topic   string `json:"topic"`
	Content string `json:"content"`
}

// ListSpecResponse is the response for GET /api/v1/spec
type ListSpecResponse struct {
	Topics []string `json:"topics"`
}

// GetSpecResponse is the response for GET /api/v1/spec/{topic}
type GetSpecResponse struct {
	Topic   string `json:"topic"`
	Content string `json:"content"`
}

// =============================================================================
// Pagination Types
// =============================================================================

// Pagination describes the page a list response carries. Paging is offset
// based: the daemon materialises a full result set per call, so there is no
// server-side stream for an opaque cursor to point into.
type Pagination struct {
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	Total   int  `json:"total"`
	HasMore bool `json:"has_more"`
}

// =============================================================================
// Response Wrapper Types
// =============================================================================

// ResponseWrapper wraps successful responses with metadata
type ResponseWrapper struct {
	Data any            `json:"data,omitempty"`
	Meta ResponseMeta  `json:"meta,omitempty"`
}

// ResponseMeta contains response metadata
type ResponseMeta struct {
	RequestID   string `json:"request_id,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
	DurationMS  int    `json:"duration_ms,omitempty"`
}
