package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"nine/internal/config"
	"nine/internal/protocol"
)

// serveAPIAndExit runs the HTTP API server as a separate process.
// It connects to the daemon via Unix socket and provides HTTP/REST access to
// Nine's functionality with feature parity to the CLI.
//
// This is the entry point for `nine api serve` (spec/contracts/api.md).
func serveAPIAndExit() {
	// API process logs belong on stderr, which is the only output stream
	// available to the operator when running as a separate process.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	// Load configuration
	cfg := config.LoadDefault()

	// Parse API-specific command-line flags
	apiCfg := parseAPIFlags(os.Args[1:])

	// Merge with config file, with flags taking precedence
	mergedCfg := mergeAPIConfig(cfg.API, apiCfg)

	// Log configuration
	slog.Info("starting nine API server",
		"version", Version,
		"host", mergedCfg.Host(),
		"port", mergedCfg.Port(),
		"auth_enabled", mergedCfg.AuthToken != "",
		"tls_enabled", mergedCfg.TLSEnabled(),
		"rate_limit_enabled", mergedCfg.RateLimitEnabled())

	// Verify daemon is running and we can connect
	socketPath := cfg.SocketPath()
	if !protocol.CanConnect(socketPath) {
		// Try to start the daemon if it's not running
		if _, err := protocol.EnsureDaemon(socketPath, os.Args[0]); err != nil {
			slog.Error("daemon not running and could not be started", "err", err)
			os.Exit(1)
		}
	}

	// Create the API server
	apiServer, err := newAPIServer(mergedCfg, socketPath)
	if err != nil {
		slog.Error("failed to create API server", "err", err)
		os.Exit(1)
	}

	// Start the server in a goroutine so we can handle graceful shutdown
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := apiServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("API server error", "err", err)
		}
	}()

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutdown signal received; stopping API server...")

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Shutdown the server
	if err := apiServer.Shutdown(ctx); err != nil {
		slog.Error("API server shutdown error", "err", err)
	}

	wg.Wait()
	slog.Info("API server stopped")
	os.Exit(0)
}

// apiFlags holds command-line flags for the API server.
type apiFlags struct {
	port        int
	host        string
	authToken   string
	timeout     int
	maxConn     int
	corsOrigins []string
	tlsEnabled  bool
	tlsCert     string
	tlsKey      string
}

// parseAPIFlags parses API-specific command-line flags.
// Note: args[0] is intentionally skipped as it contains the command name ("serve").
// The actual flags start from args[1] onwards.
func parseAPIFlags(args []string) apiFlags {
	var flags apiFlags
	flags.port = config.DefaultAPIPort
	flags.host = config.DefaultAPIHost
	flags.timeout = config.DefaultAPITimeoutSeconds
	flags.maxConn = config.DefaultAPIMaxConnections

	// Start from index 1 to skip the command name ("serve")
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				if p, err := strconv.Atoi(args[i+1]); err == nil {
					if p >= 1 && p <= 65535 {
						flags.port = p
					} else {
						slog.Warn("invalid port number, using default", "port", args[i+1], "default", config.DefaultAPIPort)
					}
				} else {
					slog.Warn("invalid port value, using default", "value", args[i+1], "default", config.DefaultAPIPort)
				}
				i++
			}
		case "--host":
			if i+1 < len(args) {
				host := args[i+1]
				// Basic validation: non-empty string
				if host != "" {
					flags.host = host
				} else {
					slog.Warn("empty host value, using default", "default", config.DefaultAPIHost)
				}
				i++
			}
		case "--auth-token":
			if i+1 < len(args) {
				flags.authToken = args[i+1]
				i++
			}
		case "--timeout":
			if i+1 < len(args) {
				if t, err := strconv.Atoi(args[i+1]); err == nil {
					if t > 0 {
						flags.timeout = t
					} else {
						slog.Warn("invalid timeout value, using default", "value", args[i+1], "default", config.DefaultAPITimeoutSeconds)
					}
				} else {
					slog.Warn("invalid timeout value, using default", "value", args[i+1], "default", config.DefaultAPITimeoutSeconds)
				}
				i++
			}
		case "--max-connections":
			if i+1 < len(args) {
				if m, err := strconv.Atoi(args[i+1]); err == nil {
					if m > 0 {
						flags.maxConn = m
					} else {
						slog.Warn("invalid max-connections value, using default", "value", args[i+1], "default", config.DefaultAPIMaxConnections)
					}
				} else {
					slog.Warn("invalid max-connections value, using default", "value", args[i+1], "default", config.DefaultAPIMaxConnections)
				}
				i++
			}
		case "--cors-origins":
			if i+1 < len(args) {
				flags.corsOrigins = strings.Split(args[i+1], ",")
				i++
			}
		case "--tls":
			flags.tlsEnabled = true
		case "--tls-cert":
			if i+1 < len(args) {
				flags.tlsCert = args[i+1]
				i++
			}
		case "--tls-key":
			if i+1 < len(args) {
				flags.tlsKey = args[i+1]
				i++
			}
		}
	}

	// Check environment variables
	if v := os.Getenv("NINE_API_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			if p >= 1 && p <= 65535 {
				flags.port = p
			} else {
				slog.Warn("invalid NINE_API_PORT value, using default", "value", v, "default", config.DefaultAPIPort)
			}
		} else {
			slog.Warn("invalid NINE_API_PORT value, using default", "value", v, "default", config.DefaultAPIPort)
		}
	}
	if v := os.Getenv("NINE_API_HOST"); v != "" {
		if v != "" {
			flags.host = v
		} else {
			slog.Warn("empty NINE_API_HOST value, using default", "default", config.DefaultAPIHost)
		}
	}
	if v := os.Getenv("NINE_API_AUTH_TOKEN"); v != "" {
		flags.authToken = v
	}
	if v := os.Getenv("NINE_API_TIMEOUT_SECONDS"); v != "" {
		if t, err := strconv.Atoi(v); err == nil {
			if t > 0 {
				flags.timeout = t
			} else {
				slog.Warn("invalid NINE_API_TIMEOUT_SECONDS value, using default", "value", v, "default", config.DefaultAPITimeoutSeconds)
			}
		} else {
			slog.Warn("invalid NINE_API_TIMEOUT_SECONDS value, using default", "value", v, "default", config.DefaultAPITimeoutSeconds)
		}
	}
	if v := os.Getenv("NINE_API_MAX_CONNECTIONS"); v != "" {
		if m, err := strconv.Atoi(v); err == nil {
			if m > 0 {
				flags.maxConn = m
			} else {
				slog.Warn("invalid NINE_API_MAX_CONNECTIONS value, using default", "value", v, "default", config.DefaultAPIMaxConnections)
			}
		} else {
			slog.Warn("invalid NINE_API_MAX_CONNECTIONS value, using default", "value", v, "default", config.DefaultAPIMaxConnections)
		}
	}
	if v := os.Getenv("NINE_API_CORS_ORIGINS"); v != "" {
		flags.corsOrigins = strings.Split(v, ",")
	}

	return flags
}

// mergeAPIConfig merges config file settings with command-line flags,
// with flags taking precedence.
func mergeAPIConfig(fileCfg config.APIConfig, flags apiFlags) config.APIConfig {
	merged := fileCfg

	// Override from flags
	if flags.port != 0 {
		merged.Port = flags.port
	}
	if flags.host != "" {
		merged.Host = flags.host
	}
	if flags.authToken != "" {
		merged.AuthToken = flags.authToken
	}
	if flags.timeout != 0 {
		merged.TimeoutSeconds = flags.timeout
	}
	if flags.maxConn != 0 {
		merged.MaxConnections = flags.maxConn
	}
	if len(flags.corsOrigins) > 0 {
		merged.CORSOrigins = flags.corsOrigins
	}
	if flags.tlsEnabled {
		merged.TLS.Enabled = true
		if flags.tlsCert != "" {
			merged.TLS.CertPath = flags.tlsCert
		}
		if flags.tlsKey != "" {
			merged.TLS.KeyPath = flags.tlsKey
		}
	}

	return merged
}

// apiServer wraps the HTTP server with additional state.
type apiServer struct {
	server        *http.Server
	config        config.APIConfig
	socketPath    string
	daemonConn    *protocol.Client
	connMu        sync.Mutex
	rateLimiters  map[string]*rateLimiter
	rateMu        sync.Mutex
}

// newAPIServer creates a new API server instance.
func newAPIServer(cfg config.APIConfig, socketPath string) (*apiServer, error) {
	// Create the HTTP server
	addr := fmt.Sprintf("%s:%d", cfg.Host(), cfg.Port())
	server := &http.Server{
		Addr:         addr,
		ReadTimeout:  time.Duration(cfg.TimeoutSeconds()) * time.Second,
		WriteTimeout: time.Duration(cfg.TimeoutSeconds()) * time.Second,
		IdleTimeout:  time.Duration(cfg.TimeoutSeconds()+5) * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	// Create the API server wrapper
	apiSrv := &apiServer{
		server:        server,
		config:        cfg,
		socketPath:    socketPath,
		rateLimiters:  make(map[string]*rateLimiter),
	}

	// Create the HTTP handler with middleware
	handler := apiSrv.createHandler()
	server.Handler = handler

	return apiSrv, nil
}

// createHandler creates the HTTP handler with all middleware and routes.
func (s *apiServer) createHandler() http.Handler {
	// Create the base router
	mux := http.NewServeMux()

	// Register routes
	s.registerRoutes(mux)

	// Wrap with middleware
	handler := s.rateLimitMiddleware(mux)
	handler = s.authMiddleware(handler)
	handler = s.corsMiddleware(handler)
	handler = s.loggingMiddleware(handler)
	handler = s.recoveryMiddleware(handler)

	return handler
}

// registerRoutes registers all API routes.
func (s *apiServer) registerRoutes(mux *http.ServeMux) {
	// Health and status
	mux.HandleFunc("GET /api/v1/health", s.handleHealth)
	mux.HandleFunc("GET /api/v1/status", s.handleStatus)

	// Conversations (sessions)
	mux.HandleFunc("POST /api/v1/conversations", s.handleCreateConversation)
	mux.HandleFunc("GET /api/v1/conversations", s.handleListConversations)
	mux.HandleFunc("GET /api/v1/conversations/{id}", s.handleGetConversation)
	mux.HandleFunc("POST /api/v1/conversations/{id}/messages", s.handleSendMessage)
	mux.HandleFunc("GET /api/v1/conversations/{id}/context", s.handleGetContext)
	mux.HandleFunc("GET /api/v1/conversations/{id}/history", s.handleGetHistory)
	mux.HandleFunc("GET /api/v1/conversations/{id}/trace", s.handleGetTrace)
	mux.HandleFunc("POST /api/v1/conversations/{id}/replay", s.handleReplay)
	mux.HandleFunc("DELETE /api/v1/conversations/{id}", s.handleDeleteConversation)
	mux.HandleFunc("POST /api/v1/conversations/{id}/stop", s.handleStopConversation)

	// Goals
	mux.HandleFunc("GET /api/v1/goals", s.handleListGoals)
	mux.HandleFunc("POST /api/v1/goals", s.handleCreateGoal)
	mux.HandleFunc("GET /api/v1/goals/{id}", s.handleGetGoal)
	mux.HandleFunc("DELETE /api/v1/goals/{id}", s.handleDeleteGoal)

	// Workflows
	mux.HandleFunc("GET /api/v1/workflows", s.handleListWorkflows)
	mux.HandleFunc("POST /api/v1/workflows/{id}/stop", s.handleStopWorkflow)
	mux.HandleFunc("POST /api/v1/workflows/{id}/fail", s.handleFailWorkflow)

	// Tools
	mux.HandleFunc("GET /api/v1/tools", s.handleListTools)
	mux.HandleFunc("GET /api/v1/tools/{name}", s.handleGetTool)
	mux.HandleFunc("POST /api/v1/tools/{name}/call", s.handleCallTool)
	mux.HandleFunc("POST /api/v1/tools/reload", s.handleReloadTools)

	// Plugins
	mux.HandleFunc("GET /api/v1/plugins", s.handleListPlugins)
	mux.HandleFunc("POST /api/v1/plugins/reload", s.handleReloadPlugins)

	// Notifications
	mux.HandleFunc("GET /api/v1/notifications", s.handleListNotifications)

	// Skills
	mux.HandleFunc("GET /api/v1/skills", s.handleListSkills)

	// Sessions
	mux.HandleFunc("POST /api/v1/sessions/attach", s.handleAttachSession)

	// System
	mux.HandleFunc("GET /api/v1/docs", s.handleListDocs)
	mux.HandleFunc("GET /api/v1/docs/{topic}", s.handleGetDocs)
	mux.HandleFunc("GET /api/v1/spec", s.handleListSpec)
	mux.HandleFunc("GET /api/v1/spec/{topic}", s.handleGetSpec)

	// Streaming endpoints (SSE)
	mux.HandleFunc("GET /api/v1/conversations/{id}/messages/stream", s.handleStreamMessages)
}

// ListenAndServe starts the API server.
func (s *apiServer) ListenAndServe() error {
	addr := s.server.Addr

	// Check if TLS is enabled
	if s.config.TLSEnabled() {
		slog.Info("starting API server with TLS", "addr", addr)
		return s.server.ListenAndServeTLS(s.config.TLS.CertPath, s.config.TLS.KeyPath)
	}

	slog.Info("starting API server", "addr", addr)
	return s.server.ListenAndServe()
}

// Shutdown stops the API server.
func (s *apiServer) Shutdown(ctx context.Context) error {
	return s.server.Shutdown(ctx)
}

// getDaemonClient returns a connected client to the daemon.
// It maintains a connection pool and handles reconnection.
func (s *apiServer) getDaemonClient() (*protocol.Client, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	// Check if we have a valid connection
	if s.daemonConn != nil {
		// Test if daemon is still responsive by attempting a new connection
		// If this succeeds, the daemon is alive and our connection should be valid
		if _, err := protocol.Connect(s.socketPath); err == nil {
			return s.daemonConn, nil
		}
		// Daemon is not responsive, close stale connection
		s.daemonConn.Close()
		s.daemonConn = nil
	}

	// Create new connection
	cl, err := protocol.Connect(s.socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}
	s.daemonConn = cl
	return cl, nil
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, statusCode int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if data != nil {
		json.NewEncoder(w).Encode(data)
	}
}

// writeError writes an error response.
func writeError(w http.ResponseWriter, statusCode int, code, message string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
			"details": details,
		},
	})
}

// authMiddleware handles authentication.
func (s *apiServer) authMiddleware(next http.Handler) http.Handler {
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
func (s *apiServer) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins := s.config.CORSOrigins()

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
func (s *apiServer) loggingMiddleware(next http.Handler) http.Handler {
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

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// recoveryMiddleware recovers from panics.
func (s *apiServer) recoveryMiddleware(next http.Handler) http.Handler {
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
func (s *apiServer) rateLimitMiddleware(next http.Handler) http.Handler {
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

// rateLimiter implements a simple token bucket rate limiter.
type rateLimiter struct {
	limit       int
	burst      int
	tokens     int
	lastUpdate time.Time
	mu         sync.Mutex
}

func newRateLimiter(limit, burst int) *rateLimiter {
	return &rateLimiter{
		limit:       limit,
		burst:      burst,
		tokens:     burst,
		lastUpdate: time.Now(),
	}
}

func (rl *rateLimiter) allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	elapsed := now.Sub(rl.lastUpdate)

	// Add tokens based on elapsed time
	rl.tokens += int(elapsed.Seconds() * float64(rl.limit) / 60.0)
	if rl.tokens > rl.burst {
		rl.tokens = rl.burst
	}
	rl.lastUpdate = now

	if rl.tokens > 0 {
		rl.tokens--
		return true
	}
	return false
}

func (rl *rateLimiter) remaining() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.tokens
}

func (rl *rateLimiter) retryAfterSeconds() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return 60 / rl.limit
}

func (rl *rateLimiter) resetSeconds() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return 60
}

// getClientIP extracts the client IP from the request.
func getClientIP(r *http.Request) string {
	// Check X-Forwarded-For header (for reverse proxies)
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Take the first IP in the list
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	// Check X-Real-IP header
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	// Fall back to RemoteAddr
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// Handler implementations

func (s *apiServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	// Check if daemon is connected
	_, err := s.getDaemonClient()
	daemonConnected := err == nil

	status := "healthy"
	if !daemonConnected {
		status = "degraded"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":           status,
		"daemon_connected": daemonConnected,
		"uptime_seconds":   int(time.Since(startTime).Seconds()),
		"version":          Version,
	})
}

var startTime = time.Now()

func (s *apiServer) handleStatus(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, info)
}

func (s *apiServer) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Interactive bool `json:"interactive"`
	}
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

	writeJSON(w, http.StatusCreated, map[string]any{
		"id":             id,
		"role":           role,
		"instance_name":   instanceName,
		"created_at":     time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *apiServer) handleListConversations(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"data": sessions,
	})
}

func (s *apiServer) handleGetConversation(w http.ResponseWriter, r *http.Request) {
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
	var contextData any
	if err := json.Unmarshal([]byte(ctxJSON), &contextData); err != nil {
		// If parsing fails, return raw JSON as a string
		contextData = ctxJSON
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":           id,
		"context":      contextData,
		"created_at":   time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *apiServer) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req struct {
		Text       string `json:"text"`
		ForceThink bool   `json:"force_think"`
	}
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

	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":    id,
		"text":       resp,
		"completed_at": time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *apiServer) handleGetContext(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req struct {
		Verbose bool `json:"verbose"`
	}
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

	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": id,
		"context":  ctxJSON,
	})
}

func (s *apiServer) handleDeleteConversation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req struct {
		Force bool `json:"force"`
	}
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

	writeJSON(w, http.StatusOK, map[string]any{
		"message": msg,
		"id":      id,
	})
}

func (s *apiServer) handleStopConversation(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"message": msg,
		"status": "stopped",
	})
}

func (s *apiServer) handleListGoals(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"data": raw,
	})
}

func (s *apiServer) handleListWorkflows(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"data": raw,
	})
}

func (s *apiServer) handleStopWorkflow(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "workflow cancelled",
		"id":      id,
		"status":  "cancelled",
	})
}

func (s *apiServer) handleFailWorkflow(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "workflow marked as failed",
		"id":      id,
		"status":  "failed",
	})
}

func (s *apiServer) handleListTools(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"data": tools,
	})
}

func (s *apiServer) handleCallTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing tool name", nil)
		return
	}

	var req struct {
		Args      json.RawMessage `json:"args"`
		LiveState bool            `json:"live_state"`
	}
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

	result, err := cl.CallTool(name, req.Args, req.LiveState)
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

	writeJSON(w, http.StatusOK, map[string]any{
		"tool_name": name,
		"output":   result,
	})
}

func (s *apiServer) handleListPlugins(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"data": plugins,
	})
}

func (s *apiServer) handleReloadPlugins(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"message":    "plugins reloaded",
		"plugins":    plugins,
		"loaded_count": len(plugins),
	})
}

func (s *apiServer) handleReloadTools(w http.ResponseWriter, r *http.Request) {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"message": "tools reloaded",
		"tools":   tools,
	})
}

func (s *apiServer) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	var req struct {
		All bool `json:"all"`
	}
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

	writeJSON(w, http.StatusOK, map[string]any{
		"data": raw,
	})
}

func (s *apiServer) handleListSkills(w http.ResponseWriter, r *http.Request) {
	// Skills are accessed through the memory store, not directly via socket
	// TODO: Implement proper skills listing via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"data": []string{},
	})
}

func (s *apiServer) handleListDocs(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement proper docs listing.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"topics": []string{"overview", "usage", "configuration", "plugins", "architecture"},
	})
}

func (s *apiServer) handleGetDocs(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing topic", nil)
		return
	}

	// TODO: Implement proper docs retrieval.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"topic":   topic,
		"content": "Documentation for " + topic + " would appear here.",
	})
}

func (s *apiServer) handleListSpec(w http.ResponseWriter, r *http.Request) {
	// TODO: Implement proper spec listing.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"topics": []string{"wire-protocol", "plugin", "toolvm", "api"},
	})
}

func (s *apiServer) handleGetSpec(w http.ResponseWriter, r *http.Request) {
	topic := r.PathValue("topic")
	if topic == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing topic", nil)
		return
	}

	// TODO: Implement proper spec retrieval.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"topic":   topic,
		"content": "Specification for " + topic + " would appear here.",
	})
}

func (s *apiServer) handleGetHistory(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	// TODO: Implement proper history retrieval via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": id,
		"data":    []string{},
	})
}

func (s *apiServer) handleGetTrace(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req struct {
		Turn       int  `json:"turn"`
		SubAgents bool `json:"sub_agents"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	// TODO: Implement proper trace retrieval via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":    id,
		"turn":       req.Turn,
		"sub_agents": req.SubAgents,
	})
}

func (s *apiServer) handleReplay(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

	var req struct {
		Turn int `json:"turn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"invalid request body", nil)
		return
	}

	// TODO: Implement proper replay via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id": id,
		"turn":    req.Turn,
	})
}

func (s *apiServer) handleGetTool(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing tool name", nil)
		return
	}

	// TODO: Implement proper tool info retrieval via daemon socket.
	// For now, return a placeholder.
	writeJSON(w, http.StatusOK, map[string]any{
		"name": name,
	})
}

func (s *apiServer) handleCreateGoal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Priority    int    `json:"priority"`
	}
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
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":          "goal-" + fmt.Sprintf("%d", time.Now().UnixNano()),
		"name":        req.Name,
		"description": req.Description,
		"priority":    req.Priority,
		"status":     "created",
	})
}

func (s *apiServer) handleGetGoal(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id,
	})
}

func (s *apiServer) handleDeleteGoal(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"message": "goal deleted",
		"id":      id,
	})
}

func (s *apiServer) handleAttachSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentID string `json:"agent_id"`
	}
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

	writeJSON(w, http.StatusOK, map[string]any{
		"agent_id":         result.AgentID,
		"name":            result.Name,
		"role":            result.Role,
		"instance_name":    result.InstanceName,
		"replay_events":    result.ReplayEvents,
		"pending_response": result.PendingResponse,
		"history":         result.History,
	})
}

func (s *apiServer) handleStreamMessages(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request",
			"missing conversation id", nil)
		return
	}

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
