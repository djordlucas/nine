package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"nine/internal/api/apigen"
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
	var errResp ErrorEnvelope
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
	var errResp ErrorEnvelope
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
// Daemon Connection Lifetime Tests
// =============================================================================

// newSocketProbe listens on a throwaway Unix socket and reports, on the
// returned channel, each connection the peer closes. It stands in for the
// daemon: protocol.Connect is a bare dial, so no handshake is needed.
func newSocketProbe(t *testing.T) (socketPath string, closed <-chan struct{}) {
	t.Helper()

	dir, err := os.MkdirTemp("", "nine-api")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	socketPath = filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	ch := make(chan struct{}, 64)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				// Blocks until the API side closes its half. A leaked client
				// never gets here, which is what the test detects.
				io.Copy(io.Discard, c) //nolint:errcheck
				ch <- struct{}{}
			}(conn)
		}
	}()

	return socketPath, ch
}

// Health is the most frequently polled endpoint on the server — a Kubernetes
// liveness probe hits it every few seconds. Probing the daemon without closing
// the socket leaks one descriptor per call until the process runs out.
func TestHandleHealth_ClosesDaemonConnection(t *testing.T) {
	socketPath, closed := newSocketProbe(t)

	s := NewServer(Config{
		APIConfig:  config.APIConfig{},
		SocketPath: socketPath,
		Version:    "test",
		StartTime:  time.Now(),
	})

	const probes = 5
	for i := 0; i < probes; i++ {
		req := httptest.NewRequest("GET", "/api/v1/health", nil)
		w := httptest.NewRecorder()
		resp, err := s.GetHealth(req.Context(), apigen.GetHealthRequestObject{})
		if err != nil {
			t.Fatalf("Probe %d: %v", i+1, err)
		}
		if err := resp.VisitGetHealthResponse(w); err != nil {
			t.Fatalf("Probe %d: %v", i+1, err)
		}

		if w.Code != http.StatusOK {
			t.Fatalf("Probe %d: expected 200, got %d", i+1, w.Code)
		}
	}

	deadline := time.After(5 * time.Second)
	for i := 0; i < probes; i++ {
		select {
		case <-closed:
		case <-deadline:
			t.Fatalf("Only %d of %d health probes closed their daemon connection — "+
				"the rest leaked", i, probes)
		}
	}
}

// =============================================================================
// Middleware Ordering Tests
// =============================================================================

// Rate limiting must apply to requests that fail authentication. When auth sits
// outside the limiter, a 401 short-circuits before any token is spent and
// bearer-token guessing runs unthrottled — so this asserts the chain order, not
// just that a limiter exists somewhere.
func TestMiddlewareOrder_RateLimitsUnauthenticatedRequests(t *testing.T) {
	const burst = 3

	s := NewServer(Config{
		APIConfig: config.APIConfig{
			AuthToken: "correct-horse-battery-staple",
			RateLimit: config.APIRateLimitConfig{
				Enabled:           true,
				RequestsPerMinute: 60,
				BurstSize:         burst,
			},
		},
		Version:   "test",
		StartTime: time.Now(),
	})

	handler := s.createHandler()

	var statuses []int
	for i := 0; i < burst+3; i++ {
		req := httptest.NewRequest("GET", "/api/v1/conversations", nil)
		req.Header.Set("Authorization", "Bearer wrong-guess")
		req.RemoteAddr = "203.0.113.9:12345"
		w := httptest.NewRecorder()

		handler.ServeHTTP(w, req)
		statuses = append(statuses, w.Code)
	}

	// The burst is spent on 401s, then the limiter takes over.
	for i := 0; i < burst; i++ {
		if statuses[i] != http.StatusUnauthorized {
			t.Errorf("Request %d: expected 401 while burst remains, got %d", i+1, statuses[i])
		}
	}
	if last := statuses[len(statuses)-1]; last != http.StatusTooManyRequests {
		t.Errorf("Expected 429 once the burst is exhausted, got %d (statuses: %v) — "+
			"failed auth is not counting against the rate limit", last, statuses)
	}
}

// CORS preflight has to answer before auth, since browsers send it without
// credentials. This guards the other half of the ordering change.
func TestMiddlewareOrder_PreflightSucceedsWithoutAuth(t *testing.T) {
	s := NewServer(Config{
		APIConfig: config.APIConfig{AuthToken: "secret"},
		Version:   "test",
		StartTime: time.Now(),
	})

	req := httptest.NewRequest("OPTIONS", "/api/v1/conversations", nil)
	req.RemoteAddr = "203.0.113.9:12345"
	w := httptest.NewRecorder()

	s.createHandler().ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Errorf("Expected preflight to return 204 without a token, got %d", w.Code)
	}
}

// =============================================================================
// Utility Function Tests
// =============================================================================

// serverWithTrustedProxies builds a bare server whose only configured behaviour
// is the trusted-proxy allowlist, for exercising clientIP.
func serverWithTrustedProxies(t *testing.T, entries ...string) *Server {
	t.Helper()
	return NewServer(Config{
		APIConfig: config.APIConfig{TrustedProxies: entries},
		Version:   "test",
		StartTime: time.Now(),
	})
}

// A request arriving straight from a client must be keyed on its peer address.
// Honouring X-Forwarded-For here is the spoofing bug: a client that varies the
// header gets a fresh rate-limit bucket on every request.
func TestClientIP_IgnoresForwardedHeadersFromUntrustedPeer(t *testing.T) {
	s := serverWithTrustedProxies(t)

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.195")
	req.Header.Set("X-Real-IP", "203.0.113.50")
	req.RemoteAddr = "192.168.1.1:12345"

	if ip := s.clientIP(req); ip != "192.168.1.1" {
		t.Errorf("Expected peer '192.168.1.1' (forwarding headers untrusted), got '%s'", ip)
	}
}

// The same spoofing attempt must not yield distinct rate-limit keys.
func TestClientIP_SpoofedHeaderCannotMintNewBuckets(t *testing.T) {
	s := serverWithTrustedProxies(t)

	seen := make(map[string]bool)
	for _, spoof := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		req := httptest.NewRequest("GET", "/test", nil)
		req.Header.Set("X-Forwarded-For", spoof)
		req.RemoteAddr = "192.168.1.1:12345"
		seen[s.clientIP(req)] = true
	}

	if len(seen) != 1 {
		t.Errorf("Expected all spoofed requests to share one rate-limit key, got %d keys: %v", len(seen), seen)
	}
}

func TestClientIP_TrustsForwardedHeaderFromConfiguredProxy(t *testing.T) {
	s := serverWithTrustedProxies(t, "192.168.1.0/24")

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.195")
	req.RemoteAddr = "192.168.1.1:12345"

	if ip := s.clientIP(req); ip != "203.0.113.195" {
		t.Errorf("Expected forwarded '203.0.113.195', got '%s'", ip)
	}
}

// With a chain of proxies, the rightmost untrusted hop is the last address the
// client could not have forged — everything to its left came from the client.
func TestClientIP_WalksChainToLastUntrustedHop(t *testing.T) {
	s := serverWithTrustedProxies(t, "192.168.1.1", "10.0.0.0/8")

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.195, 10.0.0.7")
	req.RemoteAddr = "192.168.1.1:12345"

	if ip := s.clientIP(req); ip != "203.0.113.195" {
		t.Errorf("Expected '203.0.113.195' (rightmost untrusted hop), got '%s'", ip)
	}
}

func TestClientIP_TrustedProxyFallsBackToXRealIP(t *testing.T) {
	s := serverWithTrustedProxies(t, "192.168.1.1")

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Real-IP", "203.0.113.50")
	req.RemoteAddr = "192.168.1.1:12345"

	if ip := s.clientIP(req); ip != "203.0.113.50" {
		t.Errorf("Expected '203.0.113.50', got '%s'", ip)
	}
}

// A trusted proxy that forwards nothing leaves the peer as the only address.
func TestClientIP_TrustedProxyWithoutHeaders(t *testing.T) {
	s := serverWithTrustedProxies(t, "192.168.1.1")

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "192.168.1.1:12345"

	if ip := s.clientIP(req); ip != "192.168.1.1" {
		t.Errorf("Expected '192.168.1.1', got '%s'", ip)
	}
}

func TestClientIP_FromRemoteAddr(t *testing.T) {
	s := serverWithTrustedProxies(t)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "203.0.113.1:12345"

	if ip := s.clientIP(req); ip != "203.0.113.1" {
		t.Errorf("Expected '203.0.113.1', got '%s'", ip)
	}
}

// A peer address with no port must still produce a usable key rather than an
// empty string, which would collapse every such client into one bucket.
func TestClientIP_RemoteAddrWithoutPort(t *testing.T) {
	s := serverWithTrustedProxies(t)

	req := httptest.NewRequest("GET", "/test", nil)
	req.RemoteAddr = "203.0.113.1"

	if ip := s.clientIP(req); ip != "203.0.113.1" {
		t.Errorf("Expected '203.0.113.1', got '%s'", ip)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	prefixes, rejected := parseTrustedProxies([]string{
		"10.0.0.0/8",
		" 192.168.1.7 ",
		"",
		"fd00::/8",
		"not-an-ip",
	})

	if len(prefixes) != 3 {
		t.Errorf("Expected 3 parsed prefixes, got %d: %v", len(prefixes), prefixes)
	}
	if len(rejected) != 1 || rejected[0] != "not-an-ip" {
		t.Errorf("Expected 'not-an-ip' to be rejected, got %v", rejected)
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
// Error Response Helper Tests
// =============================================================================

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

	var errResp ErrorEnvelope
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

	var errResp ErrorEnvelope
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

// =============================================================================
// Rate Limiter Map Bounds
// =============================================================================

// The map grew one entry per distinct client address and never shrank.
func TestRateLimiters_IdleEntriesAreEvicted(t *testing.T) {
	s := NewServer(Config{
		APIConfig: config.APIConfig{
			RateLimit: config.APIRateLimitConfig{Enabled: true, RequestsPerMinute: 60, BurstSize: 5},
		},
		Version:   "test",
		StartTime: time.Now(),
	})

	// Three clients, all last seen well beyond the TTL.
	stale := time.Now().Add(-2 * rateLimiterTTL)
	for _, ip := range []string{"198.51.100.1", "198.51.100.2", "198.51.100.3"} {
		limiter := newRateLimiter(60, 5)
		limiter.lastUpdate = stale
		s.rateLimiters[ip] = limiter
	}

	// A fresh client arrives; the sweep runs on the way.
	s.limiterFor("203.0.113.9")

	if len(s.rateLimiters) != 1 {
		t.Errorf("Expected the three idle entries evicted and only the new one kept, got %d: %v",
			len(s.rateLimiters), keysOf(s.rateLimiters))
	}
	if _, ok := s.rateLimiters["203.0.113.9"]; !ok {
		t.Error("Expected the active client's limiter to survive the sweep")
	}
}

// An entry still inside the TTL carries budget a client is actively spending,
// so evicting it would hand them a fresh allowance early.
func TestRateLimiters_ActiveEntriesSurvive(t *testing.T) {
	s := NewServer(Config{
		APIConfig: config.APIConfig{
			RateLimit: config.APIRateLimitConfig{Enabled: true, RequestsPerMinute: 60, BurstSize: 5},
		},
		Version:   "test",
		StartTime: time.Now(),
	})

	active := s.limiterFor("203.0.113.1")
	active.allow()

	// Force a sweep by backdating the last one.
	s.rateMu.Lock()
	s.lastSweep = time.Now().Add(-2 * rateLimiterSweepEvery)
	s.rateMu.Unlock()

	s.limiterFor("203.0.113.2")

	if _, ok := s.rateLimiters["203.0.113.1"]; !ok {
		t.Error("Expected a recently active limiter to survive the sweep")
	}
}

// Past the cap, the most idle entries go until the map fits.
func TestRateLimiters_CapIsEnforced(t *testing.T) {
	s := NewServer(Config{
		APIConfig: config.APIConfig{
			RateLimit: config.APIRateLimitConfig{Enabled: true, RequestsPerMinute: 60, BurstSize: 5},
		},
		Version:   "test",
		StartTime: time.Now(),
	})

	// All within the TTL, so the TTL pass cannot free anything and the cap
	// has to do the work.
	now := time.Now()
	for i := 0; i < maxRateLimiters+50; i++ {
		limiter := newRateLimiter(60, 5)
		limiter.lastUpdate = now.Add(-time.Duration(i) * time.Millisecond)
		s.rateLimiters[fmt.Sprintf("10.0.%d.%d", i/256, i%256)] = limiter
	}

	s.rateMu.Lock()
	s.sweepRateLimiters(now)
	s.rateMu.Unlock()

	if len(s.rateLimiters) > maxRateLimiters {
		t.Errorf("Expected the map trimmed to at most %d, got %d", maxRateLimiters, len(s.rateLimiters))
	}
}

// The sweep must not run on every request — it walks the whole map.
func TestRateLimiters_SweepIsThrottled(t *testing.T) {
	s := NewServer(Config{
		APIConfig: config.APIConfig{
			RateLimit: config.APIRateLimitConfig{Enabled: true, RequestsPerMinute: 60, BurstSize: 5},
		},
		Version:   "test",
		StartTime: time.Now(),
	})

	s.limiterFor("203.0.113.1")
	first := s.lastSweep

	s.limiterFor("203.0.113.2")
	if !s.lastSweep.Equal(first) {
		t.Error("Expected the sweep to be throttled, but it ran again immediately")
	}
}

func keysOf(m map[string]*rateLimiter) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
