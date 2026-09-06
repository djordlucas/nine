package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nine/internal/config"
)

// =============================================================================
// Rate Limiter Tests
// =============================================================================

func TestRateLimiter_Allow(t *testing.T) {
	// Create a rate limiter with 60 requests per minute, burst of 10
	rl := newRateLimiter(60, 10)

	// First 10 requests should be allowed (burst)
	for i := 0; i < 10; i++ {
		if !rl.allow() {
			t.Errorf("Request %d should be allowed (within burst)", i+1)
		}
	}

	// 11th request should be denied (burst exhausted)
	if rl.allow() {
		t.Error("11th request should be denied (burst exhausted)")
	}
}

func TestRateLimiter_TokenReplenishment(t *testing.T) {
	// Create a rate limiter with 600 requests per minute (10 per second), burst of 5
	rl := newRateLimiter(600, 5)

	// Exhaust the burst
	for i := 0; i < 5; i++ {
		rl.allow()
	}

	// Should be denied
	if rl.allow() {
		t.Error("Request should be denied after burst exhausted")
	}

	// Wait for tokens to replenish (at least 100ms for 1 token at 600/min rate)
	time.Sleep(150 * time.Millisecond)

	// Should be allowed now
	if !rl.allow() {
		t.Error("Request should be allowed after token replenishment")
	}
}

func TestRateLimiter_Remaining(t *testing.T) {
	rl := newRateLimiter(60, 10)

	if remaining := rl.remaining(); remaining != 10 {
		t.Errorf("Expected 10 remaining tokens, got %d", remaining)
	}

	rl.allow()
	if remaining := rl.remaining(); remaining != 9 {
		t.Errorf("Expected 9 remaining tokens, got %d", remaining)
	}
}

func TestRateLimiter_RetryAfter(t *testing.T) {
	rl := newRateLimiter(60, 5)

	// Exhaust all tokens
	for i := 0; i < 5; i++ {
		rl.allow()
	}

	// After exhausting, retry_after should be based on rate
	retryAfter := rl.retryAfterSeconds()
	if retryAfter <= 0 {
		t.Errorf("Expected positive retry_after, got %d", retryAfter)
	}
}

func TestRateLimiter_ResetSeconds(t *testing.T) {
	rl := newRateLimiter(60, 10)

	resetSeconds := rl.resetSeconds()
	if resetSeconds != 60 {
		t.Errorf("Expected 60 reset seconds, got %d", resetSeconds)
	}
}

// =============================================================================
// Middleware Tests
// =============================================================================

func TestAuthMiddleware_NoAuthConfigured(t *testing.T) {
	cfg := config.APIConfig{
		AuthToken: "",
		RateLimit: config.APIRateLimitConfig{
			Enabled: true,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	// Create a simple handler that returns 200
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Wrap with auth middleware
	wrapped := s.authMiddleware(handler)

	// Create a request without auth header
	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_ValidToken(t *testing.T) {
	cfg := config.APIConfig{
		AuthToken: "valid-token-123",
		RateLimit: config.APIRateLimitConfig{
			Enabled: true,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := s.authMiddleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer valid-token-123")
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_InvalidToken(t *testing.T) {
	cfg := config.APIConfig{
		AuthToken: "valid-token-123",
		RateLimit: config.APIRateLimitConfig{
			Enabled: true,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := s.authMiddleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Authorization", "Bearer invalid-token")
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}

	// Check error response
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}

	if errResp.Error.Code != "unauthorized" {
		t.Errorf("Expected error code 'unauthorized', got '%s'", errResp.Error.Code)
	}
}

func TestAuthMiddleware_MissingAuthHeader(t *testing.T) {
	cfg := config.APIConfig{
		AuthToken: "valid-token-123",
		RateLimit: config.APIRateLimitConfig{
			Enabled: true,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := s.authMiddleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("Expected status 401, got %d", w.Code)
	}
}

func TestCORSMiddleware(t *testing.T) {
	cfg := config.APIConfig{
		CORSOrigins: []string{"http://localhost:3000", "http://example.com"},
		RateLimit: config.APIRateLimitConfig{
			Enabled: true,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := s.corsMiddleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	// Check CORS headers
	origin := w.Header().Get("Access-Control-Allow-Origin")
	if origin != "http://localhost:3000, http://example.com" {
		t.Errorf("Expected CORS origin header, got: %s", origin)
	}

	if w.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Error("Expected Access-Control-Allow-Methods header")
	}

	if w.Header().Get("Access-Control-Allow-Headers") == "" {
		t.Error("Expected Access-Control-Allow-Headers header")
	}
}

func TestCORSMiddleware_Preflight(t *testing.T) {
	cfg := config.APIConfig{
		CORSOrigins: []string{"http://localhost:3000"},
		RateLimit: config.APIRateLimitConfig{
			Enabled: true,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := s.corsMiddleware(handler)

	req := httptest.NewRequest("OPTIONS", "/test", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	w := httptest.NewRecorder()

	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("Expected status 204 for preflight, got %d", w.Code)
	}
}

func TestRecoveryMiddleware(t *testing.T) {
	cfg := config.APIConfig{
		RateLimit: config.APIRateLimitConfig{
			Enabled: true,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	// Handler that panics
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	wrapped := s.recoveryMiddleware(handler)

	req := httptest.NewRequest("GET", "/test", nil)
	w := httptest.NewRecorder()

	// Should recover and return 500
	wrapped.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("Expected status 500 after panic, got %d", w.Code)
	}

	// Check error response
	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}

	if errResp.Error.Code != "server_error" {
		t.Errorf("Expected error code 'server_error', got '%s'", errResp.Error.Code)
	}
}

func TestRateLimitMiddleware_Disabled(t *testing.T) {
	cfg := config.APIConfig{
		RateLimit: config.APIRateLimitConfig{
			Enabled: false,
		},
	}
	s := &Server{
		config:       cfg,
		rateLimiters: make(map[string]*rateLimiter),
		version:      "test",
		startTime:    time.Now(),
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	wrapped := s.rateLimitMiddleware(handler)

	// Make multiple requests
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("GET", "/test", nil)
		req.RemoteAddr = "192.168.1.1:12345"
		w := httptest.NewRecorder()

		wrapped.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("Request %d: Expected status 200, got %d", i+1, w.Code)
		}
	}
}

// =============================================================================
// Utility Function Tests
// =============================================================================

func TestGetClientIP_FromXForwardedFor(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.195, 70.41.3.18, 150.172.238.178")
	req.RemoteAddr = "192.168.1.1:12345"

	ip := getClientIP(req)
	if ip != "203.0.113.195" {
		t.Errorf("Expected '203.0.113.195', got '%s'", ip)
	}
}

func TestGetClientIP_FromXRealIP(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Real-IP", "203.0.113.50")
	req.RemoteAddr = "192.168.1.1:12345"

	ip := getClientIP(req)
	if ip != "203.0.113.50" {
		t.Errorf("Expected '203.0.113.50', got '%s'", ip)
	}
}

func TestGetClientIP_FromRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "203.0.113.1:12345"

	ip := getClientIP(req)
	if ip != "203.0.113.1" {
		t.Errorf("Expected '203.0.113.1', got '%s'", ip)
	}
}

func TestGetClientIP_FallbackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	ip := getClientIP(req)
	if ip != "192.168.1.1" {
		t.Errorf("Expected '192.168.1.1', got '%s'", ip)
	}
}

// =============================================================================
// Server Creation Tests
// =============================================================================

func TestNewServer(t *testing.T) {
	cfg := Config{
		APIConfig: config.APIConfig{
			RateLimit: config.APIRateLimitConfig{
				Enabled:        true,
				RequestsPerMinute: 60,
				BurstSize:      10,
			},
		},
		SocketPath: "/tmp/nine.sock",
		Version:   "1.0.0",
		StartTime: time.Now(),
	}

	server := NewServer(cfg)

	if server == nil {
		t.Fatal("NewServer returned nil")
	}

	if server.version != "1.0.0" {
		t.Errorf("Expected version '1.0.0', got '%s'", server.version)
	}

	if server.rateLimiters == nil {
		t.Error("Expected rateLimiters map to be initialized")
	}

	if server.httpServer == nil {
		t.Error("Expected httpServer to be initialized")
	}
}

// =============================================================================
// JSON Response Helper Tests
// =============================================================================

func TestWriteJSON(t *testing.T) {
	w := httptest.NewRecorder()

	data := map[string]string{"key": "value"}
	writeJSON(w, http.StatusOK, data)

	if w.Code != http.StatusOK {
		t.Errorf("Expected status 200, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
	}

	var result map[string]string
	if err := json.NewDecoder(w.Body).Decode(&result); err != nil {
		t.Fatalf("Failed to decode response: %v", err)
	}

	if result["key"] != "value" {
		t.Errorf("Expected 'value', got '%s'", result["key"])
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()

	writeError(w, http.StatusBadRequest, "invalid_request", "test error message", nil)

	if w.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400, got %d", w.Code)
	}

	contentType := w.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("Expected Content-Type 'application/json', got '%s'", contentType)
	}

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}

	if errResp.Error.Code != "invalid_request" {
		t.Errorf("Expected code 'invalid_request', got '%s'", errResp.Error.Code)
	}

	if errResp.Error.Message != "test error message" {
		t.Errorf("Expected message 'test error message', got '%s'", errResp.Error.Message)
	}
}

func TestWriteError_WithDetails(t *testing.T) {
	w := httptest.NewRecorder()

	details := map[string]any{
		"id":   "123",
		"name": "test",
	}
	writeError(w, http.StatusNotFound, "not_found", "resource not found", details)

	var errResp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&errResp); err != nil {
		t.Fatalf("Failed to decode error response: %v", err)
	}

	if errResp.Error.Details["id"] != "123" {
		t.Errorf("Expected detail id '123', got '%v'", errResp.Error.Details["id"])
	}

	if errResp.Error.Details["name"] != "test" {
		t.Errorf("Expected detail name 'test', got '%v'", errResp.Error.Details["name"])
	}
}

// =============================================================================
// Type Tests - JSON Serialization
// =============================================================================

func TestErrorResponse_JSON(t *testing.T) {
	errResp := ErrorResponse{
		Error: ErrorDetails{
			Code:    "test_error",
			Message: "Test error message",
			Details: map[string]any{
				"field": "value",
			},
		},
	}

	data, err := json.Marshal(errResp)
	if err != nil {
		t.Fatalf("Failed to marshal ErrorResponse: %v", err)
	}

	var decoded ErrorResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal ErrorResponse: %v", err)
	}

	if decoded.Error.Code != "test_error" {
		t.Errorf("Expected code 'test_error', got '%s'", decoded.Error.Code)
	}

	if decoded.Error.Message != "Test error message" {
		t.Errorf("Expected message 'Test error message', got '%s'", decoded.Error.Message)
	}

	if decoded.Error.Details["field"] != "value" {
		t.Errorf("Expected detail field 'value', got '%v'", decoded.Error.Details["field"])
	}
}

func TestHealthResponse_JSON(t *testing.T) {
	resp := HealthResponse{
		Status:          "healthy",
		DaemonConnected: true,
		UptimeSeconds:   120,
		Version:         "1.0.0",
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("Failed to marshal HealthResponse: %v", err)
	}

	var decoded HealthResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal HealthResponse: %v", err)
	}

	if decoded.Status != "healthy" {
		t.Errorf("Expected status 'healthy', got '%s'", decoded.Status)
	}

	if !decoded.DaemonConnected {
		t.Error("Expected DaemonConnected to be true")
	}

	if decoded.UptimeSeconds != 120 {
		t.Errorf("Expected uptime 120, got %d", decoded.UptimeSeconds)
	}
}

func TestCreateConversationRequest_JSON(t *testing.T) {
	req := CreateConversationRequest{
		Interactive: true,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Failed to marshal CreateConversationRequest: %v", err)
	}

	var decoded CreateConversationRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal CreateConversationRequest: %v", err)
	}

	if !decoded.Interactive {
		t.Error("Expected Interactive to be true")
	}
}

func TestConversationInfo_JSON(t *testing.T) {
	now := time.Now().UTC()
	info := ConversationInfo{
		ID:           "session-123",
		Name:         "Test Session",
		Role:         "assistant",
		PlanMode:     "auto",
		Status:       "running",
		CreatedAt:    now,
		UpdatedAt:    now,
		EventsCount:  5,
		Protected:    false,
		Attached:     true,
		AgeSeconds:   300,
	}

	data, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("Failed to marshal ConversationInfo: %v", err)
	}

	var decoded ConversationInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal ConversationInfo: %v", err)
	}

	if decoded.ID != "session-123" {
		t.Errorf("Expected ID 'session-123', got '%s'", decoded.ID)
	}

	if decoded.Name != "Test Session" {
		t.Errorf("Expected Name 'Test Session', got '%s'", decoded.Name)
	}

	if decoded.Status != "running" {
		t.Errorf("Expected Status 'running', got '%s'", decoded.Status)
	}
}

func TestSendMessageRequest_JSON(t *testing.T) {
	req := SendMessageRequest{
		Text:       "Hello, world!",
		ForceThink: true,
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Failed to marshal SendMessageRequest: %v", err)
	}

	var decoded SendMessageRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal SendMessageRequest: %v", err)
	}

	if decoded.Text != "Hello, world!" {
		t.Errorf("Expected Text 'Hello, world!', got '%s'", decoded.Text)
	}

	if !decoded.ForceThink {
		t.Error("Expected ForceThink to be true")
	}
}

func TestToolInfo_JSON(t *testing.T) {
	inputSchema := json.RawMessage(`{"type": "object"}`)
	tool := ToolInfo{
		Name:        "test_tool",
		Description: "A test tool",
		Plugin:      "test_plugin",
		Kind:        "plugin",
		Loaded:      true,
		InputSchema: inputSchema,
		Capabilities: []string{"read", "write"},
		ManifestPath: "/path/to/manifest.json",
		Generated:   false,
	}

	data, err := json.Marshal(tool)
	if err != nil {
		t.Fatalf("Failed to marshal ToolInfo: %v", err)
	}

	var decoded ToolInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal ToolInfo: %v", err)
	}

	if decoded.Name != "test_tool" {
		t.Errorf("Expected Name 'test_tool', got '%s'", decoded.Name)
	}

	if decoded.Kind != "plugin" {
		t.Errorf("Expected Kind 'plugin', got '%s'", decoded.Kind)
	}

	if len(decoded.Capabilities) != 2 {
		t.Errorf("Expected 2 capabilities, got %d", len(decoded.Capabilities))
	}
}

func TestPluginInfo_JSON(t *testing.T) {
	plugin := PluginInfo{
		Name:   "test_plugin",
		Source: "user",
		Loaded: true,
		Tools:  []string{"tool1", "tool2"},
		Error:  "",
	}

	data, err := json.Marshal(plugin)
	if err != nil {
		t.Fatalf("Failed to marshal PluginInfo: %v", err)
	}

	var decoded PluginInfo
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal PluginInfo: %v", err)
	}

	if decoded.Name != "test_plugin" {
		t.Errorf("Expected Name 'test_plugin', got '%s'", decoded.Name)
	}

	if len(decoded.Tools) != 2 {
		t.Errorf("Expected 2 tools, got %d", len(decoded.Tools))
	}
}

// =============================================================================
// Response Writer Tests
// =============================================================================

func TestResponseWriter_WriteHeader(t *testing.T) {
	w := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

	rw.WriteHeader(http.StatusCreated)

	if rw.statusCode != http.StatusCreated {
		t.Errorf("Expected status code 201, got %d", rw.statusCode)
	}

	if w.Code != http.StatusCreated {
		t.Errorf("Expected response code 201, got %d", w.Code)
	}
}
