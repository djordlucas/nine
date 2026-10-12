package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The console is a static shell; the tests below pin the two properties an
// operator relies on: /ui answers with the embedded page, and nothing under
// the API prefix regresses into serving HTML.

func TestWebUI_ServesIndex(t *testing.T) {
	w := do(t, http.MethodGet, "/ui", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /ui: status %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<!DOCTYPE html>") {
		t.Error("GET /ui body does not start with the HTML document")
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("GET /ui Content-Type %q, want text/html", ct)
	}
}

func TestWebUI_ServesIndexWithTrailingSlash(t *testing.T) {
	w := do(t, http.MethodGet, "/ui/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /ui/: status %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<!DOCTYPE html>") {
		t.Error("GET /ui/ body does not start with the HTML document")
	}
}

func TestWebUI_DoesNotLeakOntoAPIPrefix(t *testing.T) {
	w := do(t, http.MethodGet, apiBasePath+"/ui", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("GET %s/ui: status %d, want 404 (the API prefix answers JSON only)",
			apiBasePath, w.Code)
	}
	if strings.Contains(w.Header().Get("Content-Type"), "text/html") {
		t.Error("API prefix served HTML for an unknown path; it must answer JSON")
	}
}

func TestWebUI_RejectsNonGET(t *testing.T) {
	w := httptest.NewRecorder()
	apiServer(t).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ui", strings.NewReader("x")))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /ui: status %d, want 405", w.Code)
	}
}
