package api

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// HTTP middleware: authentication, CORS, logging, panic recovery and rate
// limiting. These wrap the generated router rather than being part of it.

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
