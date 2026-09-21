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
			"trace turn", "GET", "/api/v1/conversations/abc/trace?turn=first",
			func(s *Server) http.HandlerFunc { return s.handleGetTrace },
		},
		{
			"trace sub_agents", "GET", "/api/v1/conversations/abc/trace?sub_agents=yes-please",
			func(s *Server) http.HandlerFunc { return s.handleGetTrace },
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
func TestQueryParams_BodyIsIgnored(t *testing.T) {
	s := bareServer(t)

	req := httptest.NewRequest("GET", "/api/v1/conversations/abc/trace?turn=7",
		strings.NewReader(`{"turn": 99, "sub_agents": true}`))
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()

	s.handleGetTrace(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp GetTraceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Turn != 7 {
		t.Errorf("Expected turn 7 from the query string, got %d — the body is still being read", resp.Turn)
	}
	if resp.SubAgents {
		t.Error("Expected sub_agents false (absent from the query), got true — the body is still being read")
	}
}

func TestQueryParams_TraceReadsQueryString(t *testing.T) {
	s := bareServer(t)

	req := httptest.NewRequest("GET", "/api/v1/conversations/abc/trace?turn=3&sub_agents=true", nil)
	req.SetPathValue("id", "abc")
	w := httptest.NewRecorder()

	s.handleGetTrace(w, req)

	var resp GetTraceResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Turn != 3 || !resp.SubAgents {
		t.Errorf("Expected turn=3 sub_agents=true, got turn=%d sub_agents=%v", resp.Turn, resp.SubAgents)
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
		{"skills", func(s *Server) http.HandlerFunc { return s.handleListSkills }},
		{"notifications", func(s *Server) http.HandlerFunc { return s.handleListNotifications }},
		{"history", func(s *Server) http.HandlerFunc { return s.handleGetHistory }},
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

// A stubbed list endpoint still reports a well-formed page rather than omitting
// the envelope.
func TestPagination_StubEndpointsReportAPage(t *testing.T) {
	s := bareServer(t)

	req := httptest.NewRequest("GET", "/api/v1/skills?limit=5&offset=0", nil)
	w := httptest.NewRecorder()
	s.handleListSkills(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp ListSkillsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Pagination.Limit != 5 {
		t.Errorf("Expected limit 5 echoed back, got %d", resp.Pagination.Limit)
	}
	if resp.Pagination.HasMore {
		t.Error("Expected has_more false for an empty list")
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
