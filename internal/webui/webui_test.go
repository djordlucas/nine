package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

var testApp = fstest.MapFS{
	"index.html":           {Data: []byte("<!doctype html><title>Nine</title>")},
	"favicon.svg":          {Data: []byte("<svg/>")},
	"assets/index-abc.js":  {Data: []byte("console.log(1)")},
	"assets/index-abc.css": {Data: []byte("body{}")},
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func TestHandler_ServesAssetsImmutable(t *testing.T) {
	w := get(t, HandlerFS(testApp), "GET", "/assets/index-abc.js")
	if w.Code != http.StatusOK || w.Body.String() != "console.log(1)" {
		t.Fatalf("status %d body %q", w.Code, w.Body)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("Cache-Control %q, want immutable", cc)
	}
}

func TestHandler_RoutesFallBackToIndex(t *testing.T) {
	for _, p := range []string{"/", "/processes", "/conversations/abc", "/index.html"} {
		w := get(t, HandlerFS(testApp), "GET", p)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<title>Nine</title>") {
			t.Errorf("%s: status %d body %q, want index.html", p, w.Code, w.Body)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", p, cc)
		}
	}
}

func TestHandler_MissingAssetIs404(t *testing.T) {
	if w := get(t, HandlerFS(testApp), "GET", "/assets/gone.js"); w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404: a stale hashed asset must not get index.html", w.Code)
	}
}

func TestHandler_SecurityHeaders(t *testing.T) {
	for _, p := range []string{"/", "/assets/index-abc.css", "/favicon.svg"} {
		h := get(t, HandlerFS(testApp), "GET", p).Header()
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP %q lacks %q", p, csp, want)
			}
		}
		if strings.Contains(csp, "unsafe-inline") {
			t.Errorf("%s: CSP allows inline code", p)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing nosniff or no-referrer", p)
		}
	}
}

func TestHandler_RefusesWrites(t *testing.T) {
	if w := get(t, HandlerFS(testApp), "POST", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: status %d, want 405", w.Code)
	}
}

// The committed build must be complete: an index.html that loads its assets,
// with no inline script the CSP would block.
func TestEmbeddedBuild(t *testing.T) {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		t.Fatalf("embedded build has no index.html: %v — run make webui", err)
	}
	html := string(index)
	if !strings.Contains(html, `src="/assets/`) {
		t.Error("index.html loads no script from /assets/")
	}
	if strings.Contains(html, "<script>") || strings.Contains(html, "<script type=\"module\">") {
		t.Error("index.html has an inline script, which the CSP blocks")
	}
}
