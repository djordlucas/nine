package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nine/internal/docindex"
)

// =============================================================================
// Middleware Handlers
// =============================================================================

// authMiddleware handles authentication.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth if no token is configured
		if s.config.AuthToken == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Extract token from Authorization header
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing authorization header", nil)
			return
		}

		// Check for Bearer token
		const bearerPrefix = "Bearer "
		if len(authHeader) < len(bearerPrefix) || !strings.EqualFold(authHeader[:len(bearerPrefix)], bearerPrefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid authorization header format", nil)
			return
		}

		token := authHeader[len(bearerPrefix):]

		// Constant-time comparison to prevent timing attacks
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.config.AuthToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid token", nil)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// corsMiddleware handles CORS.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins := s.config.GetCORSOrigins()

		// Set CORS headers
		w.Header().Set("Access-Control-Allow-Origin", strings.Join(origins, ", "))
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")

		// Handle preflight requests
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs requests.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap the response writer to capture the status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		slog.Info("API request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent())
	})
}

// recoveryMiddleware recovers from panics.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic in API handler", "err", err, "path", r.URL.Path)
				writeError(w, http.StatusInternalServerError, "server_error",
					fmt.Sprintf("internal server error: %v", err), nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware implements rate limiting.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	// For now, implement a simple token bucket rate limiter
	// In production, consider using a more sophisticated implementation
	if !s.config.RateLimitEnabled() {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip rate limiting for excluded paths
		for _, path := range s.config.ExcludedPaths() {
			if r.URL.Path == path {
				next.ServeHTTP(w, r)
				return
			}
		}

		// Get client IP
		ip := s.clientIP(r)

		s.rateMu.Lock()
		limiter, exists := s.rateLimiters[ip]
		if !exists {
			limiter = newRateLimiter(s.config.RequestsPerMinute(), s.config.BurstSize())
			s.rateLimiters[ip] = limiter
		}
		s.rateMu.Unlock()

		// Check rate limit
		if !limiter.allow() {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", limiter.retryAfterSeconds()))
			writeError(w, http.StatusTooManyRequests, "too_many_requests",
				"rate limit exceeded", map[string]any{
					"retry_after": limiter.retryAfterSeconds(),
				})
			return
		}

		// Add rate limit headers
		w.Header().Set("X-RateLimit-Limit", fmt.Sprintf("%d", s.config.RequestsPerMinute()))
		w.Header().Set("X-RateLimit-Remaining", fmt.Sprintf("%d", limiter.remaining()))
		w.Header().Set("X-RateLimit-Reset", fmt.Sprintf("%d", limiter.resetSeconds()))

		next.ServeHTTP(w, r)
	})
}

// =============================================================================
// Health and Status Handlers
// =============================================================================

// handleHealth checks API server health.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Check if daemon is connected. The probe opens a real socket, so it has to
	// be closed again: health is the most frequently polled endpoint on the
	// server, and leaking one descriptor per call exhausts the process under any
	// ordinary liveness probe.
	cl, err := s.getDaemonClient()
	daemonConnected := err == nil
	if daemonConnected {
		defer cl.Close()
	}

	status := "healthy"
	if !daemonConnected {
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, HealthResponse{
		Status:          status,
		DaemonConnected: daemonConnected,
		UptimeSeconds:   int(time.Since(s.startTime).Seconds()),
		Version:         s.version,
	})
}

// handleStatus returns daemon status.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	info, err := cl.Status()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Convert protocol StatusInfo to API StatusResponse
	// Convert plugin names to PluginInfo
	var pluginInfos []PluginInfo
	for _, pluginName := range info.Plugins {
		pluginInfos = append(pluginInfos, PluginInfo{
			Name:   pluginName,
			Source: "",
			Loaded: true,
		})
	}

	statusResp := StatusResponse{
		Status:      "healthy",
		Version:     s.version,
		Uptime:      info.Uptime,
		Sessions:    len(info.Agents),
		Plugins:     pluginInfos,
		Tools:       []ToolInfo{},
		MemoryStats: MemoryStats{},
		Config:      map[string]interface{}{},
	}

	writeJSON(w, http.StatusOK, statusResp)
}

// =============================================================================
// Conversation Handlers
// =============================================================================

// handleCreateConversation creates a new conversation.
func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var req CreateConversationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	id, role, instanceName, err := cl.NewConversationInteractive(req.Interactive)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	createdAt := time.Now().UTC()
	writeJSON(w, http.StatusCreated, CreateConversationResponse{
		ID:           id,
		Role:         role,
		InstanceName: instanceName,
		CreatedAt:    createdAt,
	})
}

// handleListConversations lists all conversations.
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	page, err := parsePageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	sessions, err := cl.ListSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Convert protocol SessionInfo to API ConversationInfo
	var conversations []ConversationInfo
	for _, sess := range sessions {
		conversations = append(conversations, ConversationInfo{
			ID:           sess.ID,
			Name:         sess.Name,
			Role:         "", // Role not available in SessionInfo
			PlanMode:     "", // PlanMode not available in SessionInfo
			Status:       sess.Status,
			CreatedAt:    time.Time{}, // CreatedAt not available in SessionInfo
			UpdatedAt:    time.Time{}, // UpdatedAt not available in SessionInfo
			EventsCount:  sess.Events,
			Protected:    sess.Protected,
			Attached:     sess.Attached,
			AgeSeconds:   sess.AgeSeconds,
		})
	}

	data, pagination := paginate(conversations, page)
	writeJSON(w, http.StatusOK, ListConversationsResponse{
		Data:       data,
		Pagination: pagination,
	})
}

// handleGetConversation returns conversation details.
func (s *Server) handleGetConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	// Get context to extract conversation details
	ctxJSON, err := cl.Context(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"conversation not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Parse the context JSON so it can be properly included in the response
	var contextData map[string]any
	if err := json.Unmarshal([]byte(ctxJSON), &contextData); err != nil {
		// If parsing fails, log error and return structured representation
		slog.Warn("failed to parse context JSON", "err", err, "raw_length", len(ctxJSON))
		contextData = map[string]any{
			"error": "failed to parse context",
			"raw":   string(ctxJSON),
		}
	}

	createdAt := time.Now().UTC()
	writeJSON(w, http.StatusOK, GetConversationResponse{
		ID:        id,
		Context:   contextData,
		CreatedAt: createdAt,
	})
}

// handleSendMessage sends a message to a conversation (executes a turn).
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req SendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	if req.Text == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"message text is required", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	resp, err := cl.Turn(id, req.Text)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"conversation not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	completedAt := time.Now().UTC()
	writeJSON(w, http.StatusOK, SendMessageResponse{
		AgentID:     id,
		Text:        resp,
		CompletedAt: completedAt,
	})
}

// handleGetContext returns conversation context breakdown.
func (s *Server) handleGetContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	if _, err := queryBool(r, "verbose"); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	ctxJSON, err := cl.Context(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"conversation not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Parse the context JSON so it can be properly included in the response
	var contextData any
	if err := json.Unmarshal([]byte(ctxJSON), &contextData); err != nil {
		// If parsing fails, log error and return structured representation
		slog.Warn("failed to parse context JSON", "err", err, "raw_length", len(ctxJSON))
		contextData = map[string]any{
			"error": "failed to parse context",
			"raw":   string(ctxJSON),
		}
	}

	writeJSON(w, http.StatusOK, GetContextResponse{
		AgentID: id,
		Context: contextData,
	})
}

// handleDeleteConversation deletes a conversation and all its data.
func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	if _, err := queryBool(r, "force"); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	msg, err := cl.DeleteSession(id)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"conversation not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, DeleteConversationResponse{
		Message: msg,
		ID:      id,
	})
}

// handleStopConversation stops a conversation (end session but keep history).
func (s *Server) handleStopConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	msg, err := cl.StopSession(id, false)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"conversation not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, StopConversationResponse{
		Message: msg,
		Status:  "stopped",
		ID:      id,
	})
}

// =============================================================================
// History and Trace Handlers
// =============================================================================

// handleGetHistory returns conversation message history.
func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	writeNotImplemented(w, "the wire protocol exposes no journal query; "+
		"attach carries a transcript but registers the caller as attached, "+
		"which a read must not do")
}

// handleGetTrace returns a detailed trace of a specific turn.
func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	writeNotImplemented(w, "the wire protocol exposes no per-turn trace; "+
		"`nine trace` reads the memory store directly, which the API process "+
		"must not do (API-A-1)")
}

// handleReplay replays a specific turn.
func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	writeNotImplemented(w, "the wire protocol exposes no replay message")
}

// =============================================================================
// Goal Handlers
// =============================================================================

// handleListGoals lists all goals.
func (s *Server) handleListGoals(w http.ResponseWriter, r *http.Request) {
	page, err := parsePageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	raw, err := cl.ListGoals()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	rows, err := decodeList[wireGoal](raw, "goals")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error(), nil)
		return
	}

	goals := make([]GoalInfo, 0, len(rows))
	for _, row := range rows {
		goals = append(goals, toGoalInfo(row))
	}

	data, pagination := paginate(goals, page)
	writeJSON(w, http.StatusOK, ListGoalsResponse{
		Data:       data,
		Pagination: pagination,
	})
}

// handleCreateGoal creates a new goal.
func (s *Server) handleCreateGoal(w http.ResponseWriter, r *http.Request) {
	writeNotImplemented(w, "goal creation exists only as the agent tool "+
		"goal_create, which is gated behind a role's Delegates flag; routing "+
		"the API through it would have to decide what role an HTTP caller has")
}

// handleGetGoal returns goal details.
func (s *Server) handleGetGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing goal id", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	// The wire protocol exposes the goal list but no single-goal query, so
	// select from the list rather than adding a message type for one reader.
	raw, err := cl.ListGoals()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	rows, err := decodeList[wireGoal](raw, "goals")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error(), nil)
		return
	}

	for _, row := range rows {
		if row.ID == id {
			writeJSON(w, http.StatusOK, GetGoalResponse(toGoalInfo(row)))
			return
		}
	}

	writeError(w, http.StatusNotFound, "not_found",
		"goal not found", map[string]any{"id": id})
}

// handleDeleteGoal deletes a goal.
func (s *Server) handleDeleteGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing goal id", nil)
		return
	}

	writeNotImplemented(w, "no goal deletion exists to call: the tool surface "+
		"has goal_create, goal_get, goal_list and goal_update_status, and the "+
		"wire protocol has no goal mutation message")
}

// =============================================================================
// Workflow Handlers
// =============================================================================

// handleListWorkflows lists all workflows.
func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
	page, err := parsePageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	raw, err := cl.ListWorkflows()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	rows, err := decodeList[wireWorkflow](raw, "workflows")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error(), nil)
		return
	}

	workflows := make([]WorkflowInfo, 0, len(rows))
	for _, row := range rows {
		workflows = append(workflows, toWorkflowInfo(row))
	}

	data, pagination := paginate(workflows, page)
	writeJSON(w, http.StatusOK, ListWorkflowsResponse{
		Data:       data,
		Pagination: pagination,
	})
}

// handleStopWorkflow stops/cancels a workflow.
func (s *Server) handleStopWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing workflow id", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	if err := cl.StopWorkflow(id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"workflow not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, StopWorkflowResponse{
		Message: "workflow cancelled",
		ID:      id,
		Status:  "cancelled",
	})
}

// handleFailWorkflow marks a workflow as failed.
func (s *Server) handleFailWorkflow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing workflow id", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	if err := cl.FailWorkflow(id, false); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"workflow not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, FailWorkflowResponse{
		Message: "workflow marked as failed",
		ID:      id,
		Status:  "failed",
	})
}

// =============================================================================
// Tool Handlers
// =============================================================================

// handleListTools lists all available tools.
func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
	page, err := parsePageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	tools, err := cl.ListTools()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Convert protocol ToolSummary to API ToolInfo
	var toolInfos []ToolInfo
	for _, tool := range tools {
		toolInfos = append(toolInfos, ToolInfo{
			Name:        tool.Name,
			Description: tool.Description,
			Plugin:      tool.Plugin,
			Kind:        "plugin",
			Loaded:      true,
			Capabilities: []string{},
			ManifestPath: "",
			Generated:   false,
		})
	}

	data, pagination := paginate(toolInfos, page)
	writeJSON(w, http.StatusOK, ListToolsResponse{
		Data:       data,
		Pagination: pagination,
	})
}

// handleCallTool calls a tool directly (bypassing the LLM).
func (s *Server) handleCallTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing tool name", nil)
		return
	}

	var req CallToolRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	// Convert args to json.RawMessage for the protocol
	var argsRaw json.RawMessage
	if req.Args != nil {
		argsJSON, err := json.Marshal(req.Args)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request",
				"invalid args format", nil)
			return
		}
		argsRaw = argsJSON
	} else {
		// Ensure argsRaw is a valid JSON null value, not an empty byte slice
		argsRaw = []byte("null")
	}

	result, err := cl.CallTool(name, argsRaw, req.LiveState)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"tool not found", map[string]any{"name": name})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	writeJSON(w, http.StatusOK, CallToolResponse{
		ToolName: name,
		Output:   result,
		Success:  true,
	})
}

// handleGetTool returns tool details.
func (s *Server) handleGetTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing tool name", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	tools, err := cl.ListTools()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	for _, tool := range tools {
		if tool.Name != name {
			continue
		}
		writeJSON(w, http.StatusOK, GetToolResponse{
			Name:        tool.Name,
			Description: tool.Description,
			Plugin:      tool.Plugin,
			Kind:        "plugin",
			Loaded:      true,
		})
		return
	}

	writeError(w, http.StatusNotFound, "not_found",
		"tool not found", map[string]any{"name": name})
}

// handleReloadTools reloads sandboxed tools.
func (s *Server) handleReloadTools(w http.ResponseWriter, r *http.Request) {
	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	tools, err := cl.ReloadSandboxedTools()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Convert protocol SandboxedToolStatus to API ToolInfo
	var toolInfos []ToolInfo
	for _, tool := range tools {
		capabilities := []string{}
		if tool.Capabilities != "" {
			capabilities = strings.Split(tool.Capabilities, ",")
		}
		toolInfos = append(toolInfos, ToolInfo{
			Name:         tool.Name,
			Description:  tool.Description,
			Plugin:       "",
			Kind:         tool.Kind,
			Loaded:       tool.Loaded,
			Capabilities: capabilities,
			ManifestPath: tool.ManifestPath,
			Generated:    tool.Generated,
		})
	}

	writeJSON(w, http.StatusOK, ReloadToolsResponse{
		Message: "tools reloaded",
		Tools:   toolInfos,
	})
}

// =============================================================================
// Plugin Handlers
// =============================================================================

// handleListPlugins lists all plugins.
func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	page, err := parsePageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	plugins, err := cl.ListPlugins()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Convert protocol PluginStatus to API PluginInfo
	var pluginInfos []PluginInfo
	for _, plugin := range plugins {
		pluginInfos = append(pluginInfos, PluginInfo{
			Name:     plugin.Name,
			Source:   plugin.Source,
			Loaded:   plugin.Loaded,
			Tools:    plugin.Tools,
			Error:    plugin.Error,
			Disabled: plugin.Disabled,
		})
	}

	data, pagination := paginate(pluginInfos, page)
	writeJSON(w, http.StatusOK, ListPluginsResponse{
		Data:       data,
		Pagination: pagination,
	})
}

// handleReloadPlugins reloads user plugins.
func (s *Server) handleReloadPlugins(w http.ResponseWriter, r *http.Request) {
	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	plugins, err := cl.ReloadPlugins()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Convert protocol PluginStatus to API PluginInfo
	var pluginInfos []PluginInfo
	for _, plugin := range plugins {
		pluginInfos = append(pluginInfos, PluginInfo{
			Name:     plugin.Name,
			Source:   plugin.Source,
			Loaded:   plugin.Loaded,
			Tools:    plugin.Tools,
			Error:    plugin.Error,
			Disabled: plugin.Disabled,
		})
	}

	writeJSON(w, http.StatusOK, ReloadPluginsResponse{
		Message:      "plugins reloaded",
		Plugins:      pluginInfos,
		LoadedCount: len(pluginInfos),
	})
}

// =============================================================================
// Notification Handlers
// =============================================================================

// handleListNotifications returns user notifications.
func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	all, err := queryBool(r, "all")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	page, err := parsePageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	raw, err := cl.ListNotifications(all)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	rows, err := decodeList[wireNotification](raw, "notifications")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error(), nil)
		return
	}

	notifications := make([]Notification, 0, len(rows))
	for _, row := range rows {
		notifications = append(notifications, toNotification(row))
	}

	data, pagination := paginate(notifications, page)
	writeJSON(w, http.StatusOK, ListNotificationsResponse{
		Data:       data,
		Pagination: pagination,
	})
}

// =============================================================================
// Skill Handlers
// =============================================================================

// handleListSkills lists all skills.
func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	writeNotImplemented(w, "skills live in the memory store and the wire "+
		"protocol exposes no skills query")
}

// =============================================================================
// Session Handlers
// =============================================================================

// handleAttachSession attaches to an existing session for streaming.
func (s *Server) handleAttachSession(w http.ResponseWriter, r *http.Request) {
	var req AttachSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"agent_id is required", nil)
		return
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	result, err := cl.Attach(req.AgentID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, "not_found",
				"session not found", map[string]any{"agent_id": req.AgentID})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Convert Msg slices to []any
	replayEvents := make([]any, len(result.ReplayEvents))
	for i, msg := range result.ReplayEvents {
		replayEvents[i] = msg
	}

	history := make([]any, len(result.History))
	for i, msg := range result.History {
		history[i] = msg
	}

	writeJSON(w, http.StatusOK, AttachSessionResponse{
		AgentID:        result.AgentID,
		Name:          result.Name,
		Role:          result.Role,
		InstanceName:  result.InstanceName,
		ReplayEvents:  replayEvents,
		PendingResponse: result.PendingResponse,
		History:       history,
	})
}

// =============================================================================
// Documentation Handlers
// =============================================================================

// handleListDocs lists documentation topics.
func (s *Server) handleListDocs(w http.ResponseWriter, r *http.Request) {
	writeTopics(w, docindex.Docs())
}

// handleGetDocs returns specific documentation.
func (s *Server) handleGetDocs(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing topic", nil)
		return
	}

	content, ok := readTopic(docindex.Docs(), topic)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			"documentation topic not found", map[string]any{"topic": topic})
		return
	}

	writeJSON(w, http.StatusOK, GetDocsResponse{Topic: topic, Content: content})
}

// handleListSpec lists specification topics.
func (s *Server) handleListSpec(w http.ResponseWriter, r *http.Request) {
	writeTopics(w, docindex.Spec())
}

// handleGetSpec returns specific specification.
func (s *Server) handleGetSpec(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing topic", nil)
		return
	}

	content, ok := readTopic(docindex.Spec(), topic)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found",
			"specification topic not found", map[string]any{"topic": topic})
		return
	}

	writeJSON(w, http.StatusOK, GetSpecResponse{Topic: topic, Content: content})
}

// writeNotImplemented reports an endpoint the daemon cannot yet serve.
//
// The alternative these replace was a 200 carrying invented data — an empty
// list, an echo of the request, or a fabricated id for a goal that was never
// created. A client cannot tell that apart from a real answer, so the failure
// stayed invisible. 501 names the gap, and `detail` says what the daemon would
// have to expose to close it.
func writeNotImplemented(w http.ResponseWriter, detail string) {
	writeError(w, http.StatusNotImplemented, "not_implemented",
		"endpoint not implemented", map[string]any{"detail": detail})
}

// writeTopics lists a bundle's topics. The bundles are embedded at build time,
// so this needs no daemon.
func writeTopics(w http.ResponseWriter, bundle docindex.Bundle) {
	topics, err := bundle.Topics()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error(), nil)
		return
	}

	names := make([]string, 0, len(topics))
	for _, t := range topics {
		names = append(names, t.Name)
	}

	writeJSON(w, http.StatusOK, ListDocsResponse{Topics: names})
}

// readTopic resolves a topic name to its Markdown body. Resolve accepts both
// the short name and an explicit relative path such as "contracts/api".
func readTopic(bundle docindex.Bundle, topic string) (string, bool) {
	path, ok := bundle.Resolve(topic)
	if !ok {
		return "", false
	}
	body, err := fs.ReadFile(bundle.FS, path)
	if err != nil {
		return "", false
	}
	return string(body), true
}

// =============================================================================
// Streaming Handlers
// =============================================================================

// handleStreamMessages streams conversation messages via SSE.
func (s *Server) handleStreamMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	// Check daemon connection first
	cl, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			err.Error(), nil)
		return
	}
	defer cl.Close()

	// Set SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Write headers and initial status
	w.WriteHeader(http.StatusOK)

	// Send a connection event. Marshal the payload so a path-supplied id is
	// escaped rather than interpolated raw into the SSE stream (gosec G705).
	connPayload, err := json.Marshal(struct {
		Message string `json:"message"`
		AgentID string `json:"agent_id"`
	}{Message: "stream connected", AgentID: id})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			fmt.Sprintf("marshal connection event: %v", err), nil)
		return
	}
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", connPayload)

	// Flush the response
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Wait for client disconnect or context cancellation
	// Removed the 30-second timeout to allow long-lived connections
	<-r.Context().Done()
}
