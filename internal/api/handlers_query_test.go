package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
)

// bareServer is a server with no daemon behind it. Every assertion below is
// about request parsing, which happens before the daemon is dialled.
func bareServer(t *testing.T) *Server {
	t.Helper()
	return NewServer(Config{
		APIConfig: config.APIConfig{},
		Version:   "test",
		StartTime: time.Now(),
	})
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) ErrorResponse {
	t.Helper()
	var resp ErrorResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}
	return resp
}

// =============================================================================
// Query Parameters Replace Request Bodies
// =============================================================================

// The four operations below took their arguments in a JSON body on GET/DELETE,
// which OpenAPI 3.x gives no defined semantics and generated clients drop. Each
// now reads query parameters, and a malformed one is a 400.
func TestQueryParams_RejectMalformedValues(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		target  string
		handler func(*Server) http.HandlerFunc
	}{
		{
			"context verbose", "GET", "/api/v1/conversations/abc/context?verbose=maybe",
			func(s *Server) http.HandlerFunc { return s.handleGetContext },
		},
		{
			"delete force", "DELETE", "/api/v1/conversations/abc?force=perhaps",
			func(s *Server) http.HandlerFunc { return s.handleDeleteConversation },
		},
		{
			"notifications all", "GET", "/api/v1/notifications?all=sometimes",
			func(s *Server) http.HandlerFunc { return s.handleListNotifications },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := bareServer(t)
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.SetPathValue("id", "abc")
			w := httptest.NewRecorder()

			tc.handler(s)(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400 for a malformed value, got %d", w.Code)
			}
			if code := decodeError(t, w).Error.Code; code != "invalid_request" {
				t.Errorf("Expected code 'invalid_request', got %q", code)
			}
		})
	}
}

// A JSON body on these operations is now inert. Previously it carried the
// arguments; sending one must not resurrect that path.
//
// Asserted without a daemon by pitting the two against each other: the body is
// well-formed and would be accepted, the query value is not. A 400 can only
// come from the query being what the handler reads.
func TestQueryParams_QueryBeatsBody(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		target  string
		body    string
		handler func(*Server) http.HandlerFunc
	}{
		{
			"context", "GET", "/api/v1/conversations/abc/context?verbose=bogus",
			`{"verbose": true}`,
			func(s *Server) http.HandlerFunc { return s.handleGetContext },
		},
		{
			"delete", "DELETE", "/api/v1/conversations/abc?force=bogus",
			`{"force": true}`,
			func(s *Server) http.HandlerFunc { return s.handleDeleteConversation },
		},
		{
			"notifications", "GET", "/api/v1/notifications?all=bogus",
			`{"all": true}`,
			func(s *Server) http.HandlerFunc { return s.handleListNotifications },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := bareServer(t)
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(tc.body))
			req.SetPathValue("id", "abc")
			w := httptest.NewRecorder()

			tc.handler(s)(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400 from the query value, got %d — the body is still being read", w.Code)
			}
		})
	}
}

// The mirror of the above: a malformed body with a valid query must not fail.
// Reaching the daemon (503, none running) proves the body was never parsed.
func TestQueryParams_MalformedBodyIsNotParsed(t *testing.T) {
	s := bareServer(t)

	req := httptest.NewRequest("GET", "/api/v1/conversations/abc/context?verbose=true",
		strings.NewReader(`{ this is not json`))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()

	s.handleGetContext(w, req)

	if w.Code == http.StatusBadRequest {
		t.Fatal("Expected the malformed body to be ignored, got 400 — it is still being parsed")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("Expected 503 (daemon path reached), got %d", w.Code)
	}
}

// =============================================================================
// Pagination On List Endpoints
// =============================================================================

// Every list endpoint validates paging before it reaches the daemon, so a bad
// limit is a 400 rather than a surprise page size.
func TestPagination_RejectedBeforeDaemonDial(t *testing.T) {
	tests := []struct {
		name    string
		handler func(*Server) http.HandlerFunc
	}{
		{"conversations", func(s *Server) http.HandlerFunc { return s.handleListConversations }},
		{"goals", func(s *Server) http.HandlerFunc { return s.handleListGoals }},
		{"workflows", func(s *Server) http.HandlerFunc { return s.handleListWorkflows }},
		{"tools", func(s *Server) http.HandlerFunc { return s.handleListTools }},
		{"plugins", func(s *Server) http.HandlerFunc { return s.handleListPlugins }},
		{"notifications", func(s *Server) http.HandlerFunc { return s.handleListNotifications }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := bareServer(t)
			req := httptest.NewRequest("GET", "/api/v1/x?limit=99999", nil)
			req.SetPathValue("id", "abc")
			w := httptest.NewRecorder()

			tc.handler(s)(w, req)

			// 503 would mean the handler dialled the daemon before validating.
			if w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400 for limit=99999, got %d", w.Code)
			}
		})
	}
}

// The response envelope must not carry a cursor field: paging is offset based,
// and a permanently empty next_cursor is exactly the kind of field that
// advertises capability the server does not have.
func TestPagination_NoCursorInEnvelope(t *testing.T) {
	body, err := json.Marshal(Pagination{Limit: 50, Offset: 0, Total: 3, HasMore: false})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(body), "cursor") {
		t.Errorf("Expected no cursor field in the pagination envelope, got %s", body)
	}
}
