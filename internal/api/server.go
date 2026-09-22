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
	"net/netip"
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
	connMu     sync.Mutex

	// rateLimiters maps client IPs to their rate limiters
	rateLimiters  map[string]*rateLimiter
	rateMu        sync.Mutex

	// trustedProxies is config.APIConfig.TrustedProxies parsed once at
	// construction. Empty means forwarding headers are never believed.
	trustedProxies []netip.Prefix

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
	addr := fmt.Sprintf("%s:%d", cfg.APIConfig.GetHost(), cfg.APIConfig.GetPort())
	server := &http.Server{
		Addr:           addr,
		ReadTimeout:    time.Duration(cfg.APIConfig.GetTimeoutSeconds()) * time.Second,
		WriteTimeout:   time.Duration(cfg.APIConfig.GetTimeoutSeconds()) * time.Second,
		IdleTimeout:    time.Duration(cfg.APIConfig.GetTimeoutSeconds()+5) * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	trusted, rejected := parseTrustedProxies(cfg.APIConfig.GetTrustedProxies())
	for _, entry := range rejected {
		slog.Warn("ignoring unparseable api.trusted_proxies entry; expected an IP or CIDR", "entry", entry)
	}

	return &Server{
		config:     cfg.APIConfig,
		socketPath: cfg.SocketPath,
		httpServer: server,
		rateLimiters: make(map[string]*rateLimiter),
		trustedProxies: trusted,
		version:    cfg.Version,
		startTime:  cfg.StartTime,
	}
}

// parseTrustedProxies converts config entries into prefixes, accepting both
// CIDR blocks and bare addresses. Unparseable entries are returned rather than
// silently dropped so the caller can warn: a typo here fails open, quietly
// restoring the header spoofing the allowlist exists to prevent.
func parseTrustedProxies(entries []string) (prefixes []netip.Prefix, rejected []string) {
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil {
			addr = addr.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
			continue
		}
		rejected = append(rejected, entry)
	}
	return prefixes, rejected
}

// Run starts the API server.
func (s *Server) Run() error {
	slog.Info("starting nine API server",
		"version", s.version,
		"host", s.config.GetHost(),
		"port", s.config.GetPort(),
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

	// Wrap with middleware. Order matters: the chain is built inside-out, so
	// this runs recovery -> logging -> CORS -> rate limit -> auth -> mux.
	//
	// Rate limiting sits OUTSIDE auth deliberately. With auth outermost a
	// rejected request short-circuits before the limiter is consulted, which
	// leaves bearer-token guessing completely unthrottled — the one request
	// class that most needs a ceiling. CORS stays outside both so preflight
	// still answers without a token.
	handler := s.authMiddleware(mux)
	handler = s.rateLimitMiddleware(handler)
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

	// The OpenAPI document and a browser for it
	s.registerSpecRoutes(mux)
}

// getDaemonClient returns a connected client to the daemon.
// Each call returns a fresh connection — handlers call defer cl.Close(),
// so a shared pooled connection would be closed by the first handler and
// break every subsequent request.
func (s *Server) getDaemonClient() (*protocol.Client, error) {
	s.connMu.Lock()
	defer s.connMu.Unlock()

	cl, err := protocol.Connect(s.socketPath)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to daemon: %w", err)
	}
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

// clientIP returns the address rate limiting is keyed on.
//
// X-Forwarded-For and X-Real-IP are set by whoever sent the request, so they
// are only consulted when the transport peer is itself a configured trusted
// proxy. Believing them unconditionally hands any client a fresh rate-limit
// bucket per request for the cost of one varying header, which makes the
// limiter decorative. With no trusted proxies configured — the default — the
// headers are ignored and the peer address wins.
func (s *Server) clientIP(r *http.Request) string {
	peer, ok := peerAddr(r)
	if !ok || !s.trustsProxy(peer) {
		return remoteHost(r)
	}

	// The peer is a proxy we run, so the forwarding chain is credible up to the
	// first hop we do not run. Walking right to left stops at the last address
	// an untrusted client could not have forged: everything further left was
	// supplied by the client itself.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		hops := strings.Split(xff, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
			if err != nil {
				continue
			}
			if hop = hop.Unmap(); !s.trustsProxy(hop) {
				return hop.String()
			}
		}
	}

	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if addr, err := netip.ParseAddr(xri); err == nil {
			return addr.Unmap().String()
		}
	}

	return peer.String()
}

// trustsProxy reports whether addr is one of the configured reverse proxies.
func (s *Server) trustsProxy(addr netip.Addr) bool {
	for _, prefix := range s.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// peerAddr parses the transport peer address out of RemoteAddr. It reports
// false for anything unparseable (a Unix socket peer, say), which callers must
// treat as untrusted rather than as a match.
func peerAddr(r *http.Request) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(remoteHost(r))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// remoteHost strips the port from RemoteAddr, falling back to the raw value so
// a peer that carries no port still produces a stable rate-limit key.
func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
