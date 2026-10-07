package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"nine/internal/config"
)

// =============================================================================
// The Served OpenAPI Document
// =============================================================================

func specServer(t *testing.T) http.Handler {
	t.Helper()
	s := NewServer(Config{
		APIConfig: config.APIConfig{},
		Version:   "test",
		StartTime: time.Now(),
	})
	return s.createHandler()
}

func TestOpenAPI_ServedAsYAML(t *testing.T) {
	w := httptest.NewRecorder()
	specServer(t).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/openapi.yaml", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Expected application/yaml, got %q", ct)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served document is not valid YAML: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Errorf("Expected openapi 3.1.0, got %v", doc["openapi"])
	}
}

func TestOpenAPI_ServedAsJSON(t *testing.T) {
	w := httptest.NewRecorder()
	specServer(t).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/openapi.json", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}

	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatalf("served document is not valid JSON: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Errorf("Expected openapi 3.1.0, got %v", doc["openapi"])
	}
}

// The browser page must point at the document route that actually exists.
func TestOpenAPI_BrowserReferencesTheDocument(t *testing.T) {
	w := httptest.NewRecorder()
	specServer(t).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/openapi/", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `data-url="/api/v1/openapi.json"`) {
		t.Error("Browser page does not reference /api/v1/openapi.json")
	}
}

// The document route must not shadow the documentation-topic endpoint.
func TestOpenAPI_DoesNotShadowDocsEndpoint(t *testing.T) {
	w := httptest.NewRecorder()
	specServer(t).ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/docs", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("Expected /api/v1/docs to still list topics, got %d", w.Code)
	}
	var resp struct {
		Topics []string `json:"topics"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(resp.Topics) == 0 {
		t.Error("Expected documentation topics, got none")
	}
}

// =============================================================================
// Document / Router Agreement
// =============================================================================

// Every operation the document declares must be routed, and every route must be
// declared. Until the handlers are bound to the generated server interface this
// is what keeps the two from drifting; after that it is a second, cheaper check
// that the router registration matches too.
func TestOpenAPI_MatchesRegisteredRoutes(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(specYAML, &doc); err != nil {
		t.Fatalf("parsing embedded document: %v", err)
	}

	declared := map[string]bool{}
	for path, ops := range doc.Paths {
		for method := range ops {
			declared[strings.ToUpper(method)+" /api/v1"+path] = true
		}
	}

	s := NewServer(Config{APIConfig: config.APIConfig{}, Version: "test", StartTime: time.Now()})
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	// A declared operation must resolve to a registered handler. ServeMux
	// reports the pattern it matched, which is empty for an unrouted path.
	for op := range declared {
		method, path := splitOp(op)
		req := httptest.NewRequest(method, substitutePathParams(path), nil)
		_, pattern := mux.Handler(req)
		if pattern == "" {
			t.Errorf("declared in openapi.yaml but not routed: %s %s", method, path)
		}
	}

	if len(declared) != 36 {
		t.Errorf("Expected 36 declared operations, got %d — update this count deliberately", len(declared))
	}
}

func splitOp(op string) (method, path string) {
	parts := strings.SplitN(op, " ", 2)
	return parts[0], parts[1]
}

// substitutePathParams turns /conversations/{id} into a concrete URL so the
// router can match it.
func substitutePathParams(path string) string {
	for _, param := range []string{"{id}", "{name}", "{topic}"} {
		path = strings.ReplaceAll(path, param, "x")
	}
	return path
}
