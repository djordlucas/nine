// Package api provides the HTTP API server for Nine.
// It runs as a separate process that communicates with the daemon via Unix socket.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"nine/internal/config"
	"nine/internal/protocol"
)

// Server is the main API server that handles HTTP requests and communicates with the daemon.
type Server struct {
	config     config.APIConfig
	socketPath string
	daemonConn *protocol.Client
	connMu     sync.Mutex

	// rateLimiters maps client IPs to their rate limiters
	rateLimiters  map[string]*rateLimiter
	rateMu        sync.Mutex

	httpServer *http.Server
	version    string
	startTime  time.Time
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

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Config holds the API server configuration.
type Config struct {
	APIConfig  config.APIConfig
	SocketPath string
	Version   string
	StartTime time.Time
}

// NewServer creates a new API server instance.
func NewServer(cfg Config) *Server {
	addr := fmt.Sprintf("%s:%d", cfg.APIConfig.Host(), cfg.APIConfig.Port())
	server := &http.Server{
		Addr:           addr,
		ReadTimeout:    time.Duration(cfg.APIConfig.TimeoutSeconds()) * time.Second,
		WriteTimeout:   time.Duration(cfg.APIConfig.TimeoutSeconds()) * time.Second,
		IdleTimeout:    time.Duration(cfg.APIConfig.TimeoutSeconds()+5) * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	return &Server{
		config:     cfg.APIConfig,
		socketPath: cfg.SocketPath,
		httpServer: server,
		rateLimiters: make(map[string]*rateLimiter),
		version:    cfg.Version,
		startTime:  cfg.StartTime,
	}
}

// Run starts the API server.
func (s *Server) Run() error {
	slog.Info("starting nine API server",
		"version", s.version,
		"host", s.config.Host(),
		"port", s.config.Port(),
		"auth_enabled", s.config.AuthToken != "",
		"tls_enabled", s.config.TLSEnabled(),
		"rate_limit_enabled", s.config.RateLimitEnabled())

	// Verify daemon is running and we can connect
	if !protocol.CanConnect(s.socketPath) {
		// Try to start the daemon if it's not running
		if _, err := protocol.EnsureDaemon(s.socketPath, os.Args[0]); err != nil {
			slog.Error("daemon not running and could not be started", "err", err)
			return err
		}
	}

	// Create the HTTP handler with middleware
	handler := s.createHandler()
	s.httpServer.Handler = handler

	// Start the server in a goroutine so we can handle graceful shutdown
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := s.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
	if err := s.Shutdown(ctx); err != nil {
		slog.Error("API server shutdown error", "err", err)
	}

	wg.Wait()
	slog.Info("API server stopped")
	return nil
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	addr := s.httpServer.Addr

	// Check if TLS is enabled
	if s.config.TLSEnabled() {
		slog.Info("starting API server with TLS", "addr", addr)
		return s.httpServer.ListenAndServeTLS(s.config.TLS.CertPath, s.config.TLS.KeyPath)
	}

	slog.Info("starting API server", "addr", addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown stops the API server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

// createHandler creates the HTTP handler with all middleware and routes.
func (s *Server) createHandler() http.Handler {
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
func (s *Server) registerRoutes(mux *http.ServeMux) {
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

// getDaemonClient returns a connected client to the daemon.
// It maintains a connection pool and handles reconnection.
func (s *Server) getDaemonClient() (*protocol.Client, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	// Check if we have a valid connection
	if s.daemonConn != nil {
		// Test if daemon is still responsive by attempting a new connection
		// If this succeeds, the daemon is alive and our connection should be valid
		testCl, err := protocol.Connect(s.socketPath)
		if err == nil {
			testCl.Close() // Don't leak the test connection
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
	json.NewEncoder(w).Encode(ErrorResponse{
		Error: ErrorDetails{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
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
