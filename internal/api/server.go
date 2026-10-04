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
	"slices"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/config"
	"nine/internal/protocol"
)

// Server is the main API server that handles HTTP requests and communicates with the daemon.
type Server struct {
	config     config.APIConfig
	socketPath string
	connMu     sync.Mutex

	// rateLimiters maps client IPs to their rate limiters, swept by
	// limiterFor so it does not grow one entry per client forever.
	rateLimiters map[string]*rateLimiter
	lastSweep    time.Time
	rateMu       sync.Mutex

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

// idleSince reports when this limiter last saw a request.
func (rl *rateLimiter) idleSince() time.Time {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.lastUpdate
}

// Bounds on the per-client limiter map.
const (
	// rateLimiterTTL is how long an idle limiter is kept. Tokens refill fully
	// within one minute, so past that a kept limiter and a fresh one behave
	// identically — which is what makes eviction lossless rather than a way of
	// handing someone a clean budget early.
	rateLimiterTTL = 2 * time.Minute

	// maxRateLimiters caps the map. Without a cap it grows one entry per
	// distinct client address forever: a slow leak in normal use, and the
	// memory cost of a wide client base.
	maxRateLimiters = 10000

	// rateLimiterSweepEvery is how often the sweep runs at most, so a busy
	// server does not walk the map on every request.
	rateLimiterSweepEvery = 30 * time.Second
)

// sweepRateLimiters drops limiters idle beyond the TTL, and if the map is still
// over the cap, the most idle of what remains. Callers must hold rateMu.
//
// Eviction is safe for the same reason in both cases: a limiter whose tokens
// have fully refilled carries no state a fresh one would not.
func (s *Server) sweepRateLimiters(now time.Time) {
	for ip, limiter := range s.rateLimiters {
		if now.Sub(limiter.idleSince()) > rateLimiterTTL {
			delete(s.rateLimiters, ip)
		}
	}

	if len(s.rateLimiters) <= maxRateLimiters {
		return
	}

	// Still over the cap: evict most-idle first until it fits.
	type entry struct {
		ip   string
		idle time.Time
	}
	entries := make([]entry, 0, len(s.rateLimiters))
	for ip, limiter := range s.rateLimiters {
		entries = append(entries, entry{ip: ip, idle: limiter.idleSince()})
	}
	slices.SortFunc(entries, func(a, b entry) int { return a.idle.Compare(b.idle) })

	for i := 0; i < len(entries) && len(s.rateLimiters) > maxRateLimiters; i++ {
		delete(s.rateLimiters, entries[i].ip)
	}
}

// limiterFor returns the limiter for ip, creating it if needed, and sweeps the
// map on the way when enough time has passed.
func (s *Server) limiterFor(ip string) *rateLimiter {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	now := time.Now()
	if now.Sub(s.lastSweep) >= rateLimiterSweepEvery {
		s.sweepRateLimiters(now)
		s.lastSweep = now
	}

	limiter, ok := s.rateLimiters[ip]
	if !ok {
		limiter = newRateLimiter(s.config.RequestsPerMinute(), s.config.BurstSize())
		s.rateLimiters[ip] = limiter
	}
	return limiter
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

// Unwrap exposes the underlying writer to http.ResponseController. Without it
// the wrapper hides http.Flusher and the write deadline, and the event stream
// can neither flush an event nor outlive the server's WriteTimeout.
func (rw *responseWriter) Unwrap() http.ResponseWriter { return rw.ResponseWriter }

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

// isLoopbackHost reports whether host reaches only the local machine. An
// unresolvable name is treated as non-loopback: the exposure warning below
// should err toward warning, not toward silence.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// warnIfExposed says so when the server is reachable from the network with no
// authentication in front of it.
//
// The API can start conversations, and a conversation can run shell commands,
// so an unauthenticated listener on a routable address is a remote shell. Auth
// exists — `[api] auth_token`, `--auth-token`, `NINE_API_AUTH_TOKEN` — it is
// simply off until set, and "auth_enabled=false" in a structured startup line
// is not a thing anyone reads. This is a warning rather than a refusal because
// the published image binds 0.0.0.0 by default (docker/s6/runtime), and a
// refusal would stop it booting.
func warnIfExposed(host string, authToken string) {
	if authToken != "" || isLoopbackHost(host) {
		return
	}
	slog.Warn("API is listening on a non-loopback address with NO authentication — "+
		"anyone who can reach this port can start conversations, which can run shell commands. "+
		"Set [api] auth_token (or --auth-token / NINE_API_AUTH_TOKEN), "+
		"or publish the port to loopback only (-p 127.0.0.1:8080:8080)",
		"host", host)
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
	warnIfExposed(s.config.GetHost(), s.config.AuthToken)

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

// apiBasePath prefixes every operation the document declares. The generated
// router and the request validator both need it, and they must agree.
const apiBasePath = "/api/v1"

// registerRoutes mounts the generated router plus the document routes.
//
// Every operation is routed from internal/api/openapi.yaml: apigen derives the
// patterns and the parameter binding from the document, so a route cannot drift
// from it by hand. Only the OpenAPI document itself is registered separately,
// since it is not an operation the document describes.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	apigen.HandlerWithOptions(apigen.NewStrictHandler(s, nil), apigen.StdHTTPServerOptions{
		BaseURL:    apiBasePath,
		BaseRouter: mux,
		// Validation is scoped to the generated routes, so the routes that
		// serve the document are not checked against it.
		Middlewares: s.validationMiddleware(),
		// Without this a binding failure returns net/http's plain-text
		// default, which a client parsing our error envelope cannot decode.
		ErrorHandlerFunc: requestBindingError,
	})

	s.registerSpecRoutes(mux)

	// The WebSocket transport is outside the document (OpenAPI cannot describe
	// one) and outside /api/v1 (spec/contracts/api.md API-STREAM-1).
	mux.HandleFunc("GET "+wsPath, s.handleWebSocket)
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

// writeError writes an error response.
func writeError(w http.ResponseWriter, statusCode int, code, message string, details map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(errorBody(code, message, details)) //nolint:errcheck
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
