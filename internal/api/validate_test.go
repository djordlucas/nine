package api

import (
	"net/http"
	"strings"
	"testing"
)

// =============================================================================
// Spec-Driven Request Validation
// =============================================================================

// These assert constraints the document states and no Go code re-asserts. Each
// would have reached a handler — or the daemon — before the validator was in
// the chain.

// text is required on a turn. The handler also checks it, but a client sending
// the wrong type entirely used to get there first.
func TestValidation_RejectsWrongBodyType(t *testing.T) {
	w := do(t, "POST", "/api/v1/conversations/abc/messages",
		strings.NewReader(`{"text": 123}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for a non-string text, got %d (body: %s)", w.Code, w.Body.String())
	}
	if code := decodeErr(t, w).Error.Code; code != "invalid_request" {
		t.Errorf("Expected 'invalid_request', got %q", code)
	}
}

func TestValidation_RejectsMissingRequiredBodyField(t *testing.T) {
	w := do(t, "POST", "/api/v1/conversations/abc/messages", strings.NewReader(`{}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for a body with no text, got %d", w.Code)
	}
}

// agent_id is required on attach. Nothing but the document says so for the
// wire format; the handler's own check is the backstop.
func TestValidation_RejectsMissingAgentID(t *testing.T) {
	w := do(t, "POST", "/api/v1/sessions/attach", strings.NewReader(`{}`))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("Expected 400 for an attach with no agent_id, got %d", w.Code)
	}
}

// The document bounds limit at 1000. The validator enforces it from the
// document; page() enforces it again, which is what keeps the bound in place
// if validation is ever unavailable.
func TestValidation_EnforcesDeclaredPageBounds(t *testing.T) {
	for _, target := range []string{
		"/api/v1/conversations?limit=5000",
		"/api/v1/conversations?limit=0",
		"/api/v1/conversations?offset=-5",
	} {
		t.Run(target, func(t *testing.T) {
			if w := do(t, "GET", target, nil); w.Code != http.StatusBadRequest {
				t.Fatalf("Expected 400, got %d", w.Code)
			}
		})
	}
}

// A path under the API prefix that the document does not declare is a 404 in
// the API's error shape, not net/http's plain-text default.
func TestValidation_UndeclaredPathIs404WithEnvelope(t *testing.T) {
	w := do(t, "GET", "/api/v1/not-an-operation", nil)

	if w.Code != http.StatusNotFound {
		t.Fatalf("Expected 404, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Expected a JSON error body, got Content-Type %q", ct)
	}
	if code := decodeErr(t, w).Error.Code; code != "not_found" {
		t.Errorf("Expected 'not_found', got %q", code)
	}
}

// Validation is scoped to the generated routes, so the routes that serve the
// document — which it does not declare — must still answer.
func TestValidation_DoesNotCoverTheDocumentRoutes(t *testing.T) {
	for _, target := range []string{
		"/api/v1/openapi.yaml",
		"/api/v1/openapi.json",
		"/api/v1/openapi/",
	} {
		t.Run(target, func(t *testing.T) {
			if w := do(t, "GET", target, nil); w.Code != http.StatusOK {
				t.Fatalf("Expected 200, got %d", w.Code)
			}
		})
	}
}

// Authentication stays the middleware's job: nine's bearer auth is optional,
// and the document declares operations secured unconditionally. If the
// validator enforced it, a server with auth off would reject everything.
func TestValidation_DoesNotEnforceOptionalAuth(t *testing.T) {
	if w := do(t, "GET", "/api/v1/docs", nil); w.Code != http.StatusOK {
		t.Fatalf("Expected 200 with no token on a server with auth off, got %d", w.Code)
	}
}
