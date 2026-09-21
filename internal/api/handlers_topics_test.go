package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// =============================================================================
// Docs and Spec Endpoints
// =============================================================================

// Both bundles are embedded at build time, so these four endpoints need no
// daemon — which is why returning a placeholder was never necessary.
func TestListDocs_ReturnsRealTopics(t *testing.T) {
	s := bareServer(t)
	w := httptest.NewRecorder()
	s.handleListDocs(w, httptest.NewRequest("GET", "/api/v1/docs", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp ListDocsResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Topics) < 10 {
		t.Fatalf("Expected the real docs bundle, got %d topics: %v", len(resp.Topics), resp.Topics)
	}

	// The placeholder this replaced returned exactly these five invented names.
	placeholder := []string{"overview", "usage", "configuration", "plugins", "architecture"}
	if len(resp.Topics) == len(placeholder) {
		t.Errorf("Topic list matches the old placeholder length: %v", resp.Topics)
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

func TestGetDocs_ReturnsRealContent(t *testing.T) {
	s := bareServer(t)
	req := httptest.NewRequest("GET", "/api/v1/docs/api", nil)
	req.SetPathValue("topic", "api")
	w := httptest.NewRecorder()

	s.handleGetDocs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var resp GetDocsResponse
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

func TestGetDocs_UnknownTopicIs404(t *testing.T) {
	s := bareServer(t)
	req := httptest.NewRequest("GET", "/api/v1/docs/nope", nil)
	req.SetPathValue("topic", "nope")
	w := httptest.NewRecorder()

	s.handleGetDocs(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("Expected 404 for an unknown topic, got %d", w.Code)
	}
}

func TestListSpec_ReturnsRealTopics(t *testing.T) {
	s := bareServer(t)
	w := httptest.NewRecorder()
	s.handleListSpec(w, httptest.NewRequest("GET", "/api/v1/spec", nil))

	var resp ListSpecResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Topics) < 5 {
		t.Fatalf("Expected the real spec bundle, got %v", resp.Topics)
	}
}

// Resolve accepts an explicit relative path as well as a short name, so the
// contracts are addressable both ways.
func TestGetSpec_ResolvesContractPath(t *testing.T) {
	for _, topic := range []string{"api", "contracts/api"} {
		req := httptest.NewRequest("GET", "/api/v1/spec/"+topic, nil)
		req.SetPathValue("topic", topic)
		w := httptest.NewRecorder()

		bareServer(t).handleGetSpec(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%q: expected 200, got %d", topic, w.Code)
		}
		var resp GetSpecResponse
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatalf("%q: decoding: %v", topic, err)
		}
		if !strings.Contains(resp.Content, "API-HTTP-") {
			t.Errorf("%q: expected the real contract text, got %.60q", topic, resp.Content)
		}
	}
}

// =============================================================================
// Unimplemented Endpoints
// =============================================================================

// These returned 200 with invented data — an empty list, an echo of the
// request, or a fabricated id for a goal that was never created. A client could
// not tell that from a real answer.
func TestUnimplementedEndpoints_Return501(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		target  string
		handler func(*Server) http.HandlerFunc
	}{
		{"history", "GET", "/api/v1/conversations/abc/history",
			func(s *Server) http.HandlerFunc { return s.handleGetHistory }},
		{"trace", "GET", "/api/v1/conversations/abc/trace",
			func(s *Server) http.HandlerFunc { return s.handleGetTrace }},
		{"replay", "POST", "/api/v1/conversations/abc/replay",
			func(s *Server) http.HandlerFunc { return s.handleReplay }},
		{"create goal", "POST", "/api/v1/goals",
			func(s *Server) http.HandlerFunc { return s.handleCreateGoal }},
		{"delete goal", "DELETE", "/api/v1/goals/g1",
			func(s *Server) http.HandlerFunc { return s.handleDeleteGoal }},
		{"skills", "GET", "/api/v1/skills",
			func(s *Server) http.HandlerFunc { return s.handleListSkills }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := bareServer(t)
			req := httptest.NewRequest(tc.method, tc.target, nil)
			req.SetPathValue("id", "abc")
			w := httptest.NewRecorder()

			tc.handler(s)(w, req)

			if w.Code != http.StatusNotImplemented {
				t.Fatalf("Expected 501, got %d", w.Code)
			}

			var resp ErrorResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decoding: %v", err)
			}
			if resp.Error.Code != "not_implemented" {
				t.Errorf("Expected code 'not_implemented', got %q", resp.Error.Code)
			}
			// The detail is what makes the 501 actionable rather than a shrug.
			if detail, ok := resp.Error.Details["detail"].(string); !ok || detail == "" {
				t.Errorf("Expected a detail explaining the gap, got %v", resp.Error.Details)
			}
		})
	}
}

// A 501 must not be reachable only after a daemon dial: the gap is structural,
// not a runtime condition, so it is reported whether or not the daemon is up.
func TestUnimplementedEndpoints_DoNotDialDaemon(t *testing.T) {
	s := bareServer(t)
	req := httptest.NewRequest("GET", "/api/v1/skills", nil)
	w := httptest.NewRecorder()

	s.handleListSkills(w, req)

	if w.Code == http.StatusServiceUnavailable {
		t.Fatal("Expected 501 without dialling the daemon, got 503")
	}
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("Expected 501, got %d", w.Code)
	}
}
