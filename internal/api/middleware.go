package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nine/internal/logsafe"
)

// HTTP middleware: authentication, CORS, logging, panic recovery and rate
// limiting. These wrap the generated router rather than being part of it.

// authMiddleware handles authentication: the bearer token, or a browser
// session's cookie (auth_session.go). The session operations themselves are
// open, since signing in is how a browser gets a cookie.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth if no token is configured
		if s.config.AuthToken == "" || r.URL.Path == authSessionPath {
			next.ServeHTTP(w, r)
			return
		}

		if r.Header.Get("Authorization") == "" {
			sess, ok := s.sessionFromRequest(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing authorization header", nil)
				return
			}
			if reason := checkCookieRequest(r, sess); reason != "" {
				writeError(w, http.StatusForbidden, "forbidden", reason, nil)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Check for Bearer token
		authHeader := r.Header.Get("Authorization")
		const bearerPrefix = "Bearer "
		if len(authHeader) < len(bearerPrefix) || !strings.EqualFold(authHeader[:len(bearerPrefix)], bearerPrefix) {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid authorization header format", nil)
			return
		}

		// Constant-time comparison to prevent timing attacks
		if !s.validBearer(r) {
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
		//nolint:gosec // G706: every request-derived value goes through
		// logsafe.Value, which strips the control characters a forged log line
		// needs. gosec's taint analysis does not recognise a sanitiser.
		slog.Info("API request",
			"method", logsafe.Value(r.Method),
			"path", logsafe.Value(r.URL.Path),
			"status", wrapped.statusCode,
			"duration_ms", duration.Milliseconds(),
			"remote_addr", logsafe.Value(r.RemoteAddr),
			"user_agent", logsafe.Value(r.UserAgent()))
	})
}

// recoveryMiddleware recovers from panics.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				//nolint:gosec // G706: the path is sanitised by logsafe.Value; see above.
				slog.Error("panic in API handler", "err", err, "path", logsafe.Value(r.URL.Path))
				writeError(w, http.StatusInternalServerError, "server_error",
					fmt.Sprintf("internal server error: %v", err), nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// rateLimitMiddleware implements rate limiting.
func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
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

		limiter := s.limiterFor(s.clientIP(r))

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
