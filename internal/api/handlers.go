package api

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
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
		ip := getClientIP(r)

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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Check if daemon is connected
	_, err := s.getDaemonClient()
	daemonConnected := err == nil

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

func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, ListConversationsResponse{
		Data: conversations,
	})
}

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

func (s *Server) handleGetContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req GetContextRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
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

func (s *Server) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req DeleteConversationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
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

func (s *Server) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	// TODO: Implement proper history retrieval via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, GetHistoryResponse{
		AgentID: id,
		Data:    []HistoryEntry{},
	})
}

func (s *Server) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req GetTraceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	// TODO: Implement proper trace retrieval via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, GetTraceResponse{
		AgentID:    id,
		Turn:       req.Turn,
		SubAgents: req.SubAgents,
	})
}

func (s *Server) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req ReplayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	// TODO: Implement proper replay via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, ReplayResponse{
		AgentID: id,
		Turn:    req.Turn,
	})
}

// =============================================================================
// Goal Handlers
// =============================================================================

func (s *Server) handleListGoals(w http.ResponseWriter, r *http.Request) {
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

	// Parse raw goals data
	var goals []GoalInfo
	if err := json.Unmarshal([]byte(raw), &goals); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": raw,
		})
		return
	}

	writeJSON(w, http.StatusOK, ListGoalsResponse{
		Data: goals,
	})
}

func (s *Server) handleCreateGoal(w http.ResponseWriter, r *http.Request) {
	var req CreateGoalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"goal name is required", nil)
		return
	}

	// TODO: Implement proper goal creation via daemon socket.
	// Currently, goals are managed through agent tools (goal_create, goal_get, etc.)
	// which are not directly accessible via the socket protocol.
	// For now, return a placeholder.
	// See: internal/agent/register_goals.go for the tool implementations.
	writeJSON(w, http.StatusCreated, CreateGoalResponse{
		ID:        "goal-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		Status:    "created",
		SessionID: "",
	})
}

func (s *Server) handleGetGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing goal id", nil)
		return
	}

	// TODO: Implement proper goal retrieval via daemon socket.
	// Currently, goals are managed through agent tools (goal_create, goal_get, etc.)
	// which are not directly accessible via the socket protocol.
	// For now, return a placeholder.
	// See: internal/agent/register_goals.go for the tool implementations.
	writeJSON(w, http.StatusOK, GetGoalResponse{
		ID: id,
	})
}

func (s *Server) handleDeleteGoal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing goal id", nil)
		return
	}

	// TODO: Implement proper goal deletion via daemon socket.
	// Currently, goals are managed through agent tools (goal_create, goal_get, etc.)
	// which are not directly accessible via the socket protocol.
	// For now, return a placeholder.
	// See: internal/agent/register_goals.go for the tool implementations.
	writeJSON(w, http.StatusOK, DeleteGoalResponse{
		Message: "goal deleted",
		ID:      id,
	})
}

// =============================================================================
// Workflow Handlers
// =============================================================================

func (s *Server) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
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

	// Parse raw workflows data
	var workflows []WorkflowInfo
	if err := json.Unmarshal([]byte(raw), &workflows); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": raw,
		})
		return
	}

	writeJSON(w, http.StatusOK, ListWorkflowsResponse{
		Data: workflows,
	})
}

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

func (s *Server) handleListTools(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, ListToolsResponse{
		Data: toolInfos,
	})
}

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
	argsJSON, err := json.Marshal(req.Args)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid args format", nil)
		return
	}

	result, err := cl.CallTool(name, json.RawMessage(argsJSON), req.LiveState)
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

func (s *Server) handleGetTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing tool name", nil)
		return
	}

	// TODO: Implement proper tool info retrieval via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, GetToolResponse{
		Name: name,
	})
}

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

func (s *Server) handleListPlugins(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, ListPluginsResponse{
		Data: pluginInfos,
	})
}

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

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	var req ListNotificationsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
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

	raw, err := cl.ListNotifications(req.All)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error",
			err.Error(), nil)
		return
	}

	// Parse notifications data
	var notifications []Notification
	if err := json.Unmarshal([]byte(raw), &notifications); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"data": raw,
		})
		return
	}

	writeJSON(w, http.StatusOK, ListNotificationsResponse{
		Data: notifications,
	})
}

// =============================================================================
// Skill Handlers
// =============================================================================

func (s *Server) handleListSkills(w http.ResponseWriter, r *http.Request) {
	// Skills are accessed through the memory store, not directly via socket
	// TODO: Implement proper skills listing via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, ListSkillsResponse{
		Data: []SkillInfo{},
	})
}

// =============================================================================
// Session Handlers
// =============================================================================

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

func (s *Server) handleListDocs(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement proper docs listing.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, ListDocsResponse{
		Topics: []string{"overview", "usage", "configuration", "plugins", "architecture"},
	})
}

func (s *Server) handleGetDocs(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing topic", nil)
		return
	}

	// TODO: Implement proper docs retrieval.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, GetDocsResponse{
		Topic:   topic,
		Content: "Documentation for " + topic + " would appear here.",
	})
}

func (s *Server) handleListSpec(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement proper spec listing.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, ListSpecResponse{
		Topics: []string{"wire-protocol", "plugin", "toolvm", "api"},
	})
}

func (s *Server) handleGetSpec(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing topic", nil)
		return
	}

	// TODO: Implement proper spec retrieval.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, GetSpecResponse{
		Topic:   topic,
		Content: "Specification for " + topic + " would appear here.",
	})
}

// =============================================================================
// Streaming Handlers
// =============================================================================

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

	// Send a connection event
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", `{"message": "stream connected", "agent_id": "`+id+`"}`)

	// Flush the response
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Wait for client disconnect or context cancellation
	// Removed the 30-second timeout to allow long-lived connections
	<-r.Context().Done()
}
