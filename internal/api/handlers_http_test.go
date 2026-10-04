package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
)

// These tests drive the real router rather than calling handler methods
// directly: routing, path-parameter extraction and body/query binding are all
// generated from openapi.yaml, so exercising them is part of testing a handler.
//
// No daemon is running, so an operation that reaches the socket answers 503.
// That makes 503 the marker for "parsing and routing succeeded".

func apiServer(t *testing.T) http.Handler {
	t.Helper()
	return NewServer(Config{
		APIConfig: config.APIConfig{},
		Version:   "test",
		StartTime: time.Now(),
	}).createHandler()
}

func do(t *testing.T, method, target string, body io.Reader) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	apiServer(t).ServeHTTP(w, httptest.NewRequest(method, target, body))
	return w
}

func decodeErr(t *testing.T, w *httptest.ResponseRecorder) ErrorEnvelope {
	t.Helper()
	var resp ErrorEnvelope
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v (body: %s)", err, w.Body.String())
	}
	return resp
}

// ErrorEnvelope mirrors the document's error shape for assertions, so a test
// failure names the field rather than indexing into a map.
type ErrorEnvelope struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

// =============================================================================
// Query Parameters Replace Request Bodies
// =============================================================================

// A malformed query value is rejected. The generated binder rejects a value of
// the wrong type; the handler rejects one out of range. Both surface as the
// API's own 400 envelope rather than net/http's plain-text default.
func TestQuery_MalformedValuesAre400(t *testing.T) {
	tests := []struct{ name, method, target string }{
		{"context verbose", "GET", "/api/v1/conversations/abc/context?verbose=maybe"},
		{"delete force", "DELETE", "/api/v1/conversations/abc?force=perhaps"},
		{"notifications all", "GET", "/api/v1/notifications?all=sometimes"},
		{"limit not an integer", "GET", "/api/v1/conversations?limit=abc"},
		{"limit above the maximum", "GET", "/api/v1/conversations?limit=99999"},
		{"limit below the minimum", "GET", "/api/v1/conversations?limit=0"},
		{"negative offset", "GET", "/api/v1/conversations?offset=-1"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, tc.method, tc.target, nil)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400, got %d (body: %s)", w.Code, w.Body.String())
			}
			if code := decodeErr(t, w).Error.Code; code != "invalid_request" {
				t.Errorf("Expected code 'invalid_request', got %q", code)
			}
		})
	}
}

// The four operations below took their arguments in a JSON body on GET or
// DELETE, which OpenAPI 3.x leaves undefined. A body is now inert: the query
// value is malformed and the body well-formed, so a 400 can only come from the
// query being what is read.
func TestQuery_QueryBeatsBody(t *testing.T) {
	tests := []struct{ name, method, target, body string }{
		{"context", "GET", "/api/v1/conversations/abc/context?verbose=bogus", `{"verbose": true}`},
		{"delete", "DELETE", "/api/v1/conversations/abc?force=bogus", `{"force": true}`},
		{"notifications", "GET", "/api/v1/notifications?all=bogus", `{"all": true}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, tc.method, tc.target, strings.NewReader(tc.body))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400 from the query value, got %d — the body is still being read", w.Code)
			}
		})
	}
}

// The mirror of the above: a malformed body with a valid query must not fail.
// Reaching the daemon (503) proves the body was never parsed.
func TestQuery_MalformedBodyIsNotParsed(t *testing.T) {
	w := do(t, "GET", "/api/v1/conversations/abc/context?verbose=true",
		strings.NewReader(`{ this is not json`))

	if w.Code == http.StatusBadRequest {
		t.Fatal("Expected the malformed body to be ignored, got 400 — it is still being parsed")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 (daemon path reached), got %d", w.Code)
	}
}

// =============================================================================
// Pagination
// =============================================================================

// Paging is validated before the daemon is dialled, on every list endpoint.
// A 503 here would mean the socket was reached first.
func TestPagination_ValidatedBeforeDaemonDial(t *testing.T) {
	for _, target := range []string{
		"/api/v1/conversations",
		"/api/v1/goals",
		"/api/v1/workflows",
		"/api/v1/tools",
		"/api/v1/plugins",
		"/api/v1/notifications",
	} {
		t.Run(target, func(t *testing.T) {
			if w := do(t, "GET", target+"?limit=99999", nil); w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400 for limit=99999, got %d", w.Code)
			}
			// A valid limit must get past validation and reach the socket.
			if w := do(t, "GET", target+"?limit=10", nil); w.Code != http.StatusServiceUnavailable {
				t.Fatalf("Expected 503 for a valid limit, got %d", w.Code)
			}
		})
	}
}

func TestPagination_MaximumLimitIsAccepted(t *testing.T) {
	if w := do(t, "GET", "/api/v1/conversations?limit=1000", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("Expected limit=1000 to be accepted and reach the daemon (503), got %d", w.Code)
	}
}

// =============================================================================
// Documentation And Specification Endpoints
// =============================================================================

// Both bundles are embedded at build time, so these need no daemon — which is
// why returning a placeholder was never necessary.
func TestDocs_ListsRealTopics(t *testing.T) {
	w := do(t, "GET", "/api/v1/docs", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp struct {
		Topics []string `json:"topics"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Topics) < 10 {
		t.Fatalf("Expected the real docs bundle, got %d topics: %v", len(resp.Topics), resp.Topics)
	}

	var found bool
	for _, topic := range resp.Topics {
		if topic == "api" {
			found = true
		}
	}
	if !found {
		t.Errorf("Expected docs/api.md to appear as topic 'api', got %v", resp.Topics)
	}
}

func TestDocs_ReturnsRealContent(t *testing.T) {
	w := do(t, "GET", "/api/v1/docs/api", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp struct {
		Topic   string `json:"topic"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if strings.Contains(resp.Content, "would appear here") {
		t.Fatal("Still returning the placeholder body")
	}
	if !strings.HasPrefix(resp.Content, "# HTTP API") {
		t.Errorf("Expected the real docs/api.md, got: %.60q", resp.Content)
	}
}

func TestDocs_UnknownTopicIs404(t *testing.T) {
	if w := do(t, "GET", "/api/v1/docs/nope", nil); w.Code != http.StatusNotFound {
		t.Fatalf("Expected 404 for an unknown topic, got %d", w.Code)
	}
}

func TestSpec_ListsRealTopics(t *testing.T) {
	w := do(t, "GET", "/api/v1/spec", nil)
	var resp struct {
		Topics []string `json:"topics"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Topics) < 5 {
		t.Fatalf("Expected the real spec bundle, got %v", resp.Topics)
	}
}

// Resolve accepts a short name; an explicit contracts/ path is addressable via
// the same endpoint with an escaped separator.
func TestSpec_ReturnsRealContract(t *testing.T) {
	w := do(t, "GET", "/api/v1/spec/api", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !strings.Contains(resp.Content, "API-HTTP-") {
		t.Errorf("Expected the real contract text, got %.60q", resp.Content)
	}
}
