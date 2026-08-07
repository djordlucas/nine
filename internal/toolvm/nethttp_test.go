package toolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// httpTool installs a `js` tool with the given source and a net.http grant, and
// returns a host ready to call it.
func httpTool(t *testing.T, grant *HTTPGrant, source string) *Host {
	t.Helper()
	dir := t.TempDir()
	writeTool(t, dir, "fetcher", `
name = "fetcher"
kind = "js"
entrypoint = "./fetcher.js"
description = "Fetches."

[capabilities]
net = ["http"]
`, source)
	return openHost(t, dir, map[string]Grant{"fetcher": {HTTP: grant}})
}

// A tool that fetches whatever URL it is handed and returns the body.
const fetchSource = `
export default async ({ url, method, headers, body }) => {
  const res = await fetch(url, { method, headers, body });
  return { status: res.status, body: res.text() };
};
`

// hostPortOf splits a test server URL into its host and port.
func hostPortOf(t *testing.T, rawURL string) (host, port string) {
	t.Helper()
	trimmed := strings.TrimPrefix(rawURL, "http://")
	h, p, err := net.SplitHostPort(trimmed)
	if err != nil {
		t.Fatalf("split %q: %v", rawURL, err)
	}
	return h, p
}

func callTool(t *testing.T, h *Host, args any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return h.Call(context.Background(), "fetcher", raw)
}

// The happy path has to work, or every block below proves nothing.
//
// httptest binds to loopback, which the SSRF gate blocks by design, so reaching
// it needs the loopback rejection relaxed. allowTestLoopback does that and only
// that — link-local and the rest stay blocked, which is what keeps the metadata
// and redirect tests below meaningful while it is in effect.
// TestDialGateHasNoProductionEscapeHatch asserts production cannot reach it.
func TestGrantedToolCanFetchAnAllowedHost(t *testing.T) {
	var (
		mu      sync.Mutex
		gotPath string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		fmt.Fprint(w, "hello")
	}))
	defer srv.Close()

	_, port := hostPortOf(t, srv.URL)
	allowTestLoopback(t)

	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"127.0.0.1"},
		Methods:    []string{"GET"},
	}, fetchSource)

	out, err := callTool(t, h, map[string]string{"url": "http://127.0.0.1:" + port + "/greet"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, `"status":200`) {
		t.Errorf("output = %q", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/greet" {
		t.Errorf("server saw path %q, want /greet", gotPath)
	}
}

// The single most important test in this package. Without the link-local block,
// a generated tool on any cloud host reaches the instance credentials endpoint
// and the sandbox has bought nothing.
func TestInstanceMetadataEndpointIsBlocked(t *testing.T) {
	h := httpTool(t, &HTTPGrant{
		// Deliberately allow the hostname: the point is that the *name* passing
		// the allowlist must not be sufficient. The IP gate is what stops it.
		AllowHosts: []string{"*.test", "metadata.test", "169.254.169.254"},
		Methods:    []string{"GET"},
	}, fetchSource)

	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/iam/security-credentials/",
		"http://[fe80::1]/",
		"http://169.254.170.2/v2/credentials",
	} {
		t.Run(target, func(t *testing.T) {
			out, err := callTool(t, h, map[string]string{"url": target})
			if err == nil {
				t.Fatalf("reached %s and got %q", target, out)
			}
			if !strings.Contains(err.Error(), "blocked") {
				t.Errorf("error = %v, want a block", err)
			}
		})
	}
}

// The rebinding defense, stated as the property it actually is: passing the
// hostname allowlist must NOT be sufficient. Here the host is in the allowlist
// and the connection is still refused, because the gate that matters runs on the
// address at connect time. A filter that trusted the name — which is what a
// rebinding attack exploits, by pointing an allowed name at a hostile address —
// would let this through.
//
// Note there is no allowTestLoopback here: that is the point.
func TestPassingTheNameAllowlistIsNotSufficient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "should never be reached")
	}))
	defer srv.Close()

	_, port := hostPortOf(t, srv.URL)

	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"127.0.0.1"}, // explicitly allowed by name
		Methods:    []string{"GET"},
	}, fetchSource)

	out, err := callTool(t, h, map[string]string{"url": "http://127.0.0.1:" + port + "/"})
	if err == nil {
		t.Fatalf("an allowed NAME reached a blocked ADDRESS and got %q", out)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error = %v, want a block naming the address", err)
	}
}

// A redirect is an attacker-controlled URL. Treating the first check as covering
// the chain is how an allowed host becomes an open redirect into anywhere.
func TestRedirectToBlockedAddressIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	_, port := hostPortOf(t, srv.URL)
	allowTestLoopback(t)

	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"127.0.0.1"},
		Methods:    []string{"GET"},
	}, fetchSource)

	out, err := callTool(t, h, map[string]string{"url": "http://127.0.0.1:" + port + "/go"})
	if err == nil {
		t.Fatalf("followed a redirect to the metadata endpoint and got %q", out)
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error = %v, want a block", err)
	}
}

// A redirect off the allowlist must be refused even when the target is
// perfectly reachable.
//
// The destination here is deliberately a host that WOULD connect — "localhost"
// is the same loopback interface the allowed "127.0.0.1" is, and the escape
// hatch permits loopback dials — so the only thing that can refuse it is the
// per-hop allowlist recheck. An earlier version of this test redirected to an
// unresolvable public name and passed even with the recheck deleted, because DNS
// failed first; it proved nothing.
func TestRedirectOffTheAllowlistIsRefused(t *testing.T) {
	var reached bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		fmt.Fprint(w, "exfiltrated")
	}))
	defer target.Close()

	_, targetPort := hostPortOf(t, target.URL)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+targetPort+"/collect", http.StatusFound)
	}))
	defer redirector.Close()

	_, port := hostPortOf(t, redirector.URL)
	allowTestLoopback(t)

	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"127.0.0.1"}, // note: "localhost" is NOT allowed
		Methods:    []string{"GET"},
	}, fetchSource)

	out, err := callTool(t, h, map[string]string{"url": "http://127.0.0.1:" + port + "/go"})
	if err == nil {
		t.Fatalf("followed a redirect off the allowlist and got %q", out)
	}
	if reached {
		t.Error("the off-allowlist host was actually contacted")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error = %v, want the allowlist refusal", err)
	}
}

// Credentials must not follow a redirect to another origin — otherwise an
// allowed host can launder a tool's Authorization header to anywhere else the
// allowlist happens to permit.
func TestCredentialsAreStrippedOnCrossOriginRedirect(t *testing.T) {
	var (
		mu            sync.Mutex
		secondHopAuth string
	)
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		secondHopAuth = r.Header.Get("Authorization")
		mu.Unlock()
		fmt.Fprint(w, "arrived")
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+"/landing", http.StatusFound)
	}))
	defer first.Close()

	_, firstPort := hostPortOf(t, first.URL)
	allowTestLoopback(t)

	// Both hops are allowed by name, and they differ only by port — which is
	// still a different origin, and is exactly the case Go's own cross-domain
	// stripping does not cover.
	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"127.0.0.1"},
		Methods:    []string{"GET"},
	}, fetchSource)

	_, err := callTool(t, h, map[string]any{
		"url":     "http://127.0.0.1:" + firstPort + "/go",
		"headers": map[string]string{"Authorization": "Bearer super-secret"},
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	mu.Lock()
	got := secondHopAuth
	mu.Unlock()
	if got != "" {
		t.Errorf("Authorization survived a cross-origin redirect as %q", got)
	}
}

// The method allowlist is a real control, not documentation.
func TestMethodAllowlistIsEnforced(t *testing.T) {
	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"api.example.com"},
		Methods:    []string{"GET"},
	}, fetchSource)

	_, err := callTool(t, h, map[string]string{
		"url": "https://api.example.com/x", "method": "DELETE",
	})
	if err == nil {
		t.Fatal("DELETE was permitted against a GET-only grant")
	}
	if !strings.Contains(err.Error(), "DELETE") {
		t.Errorf("error = %v, want it to name the method", err)
	}
}

// A tool with no net.http grant has no network, and the refusal must not depend
// on the tool being unable to find the function — the import is shared.
func TestUngrantedToolCannotFetch(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "quiet", `
name = "quiet"
kind = "js"
entrypoint = "./quiet.js"
description = "Declares nothing."
`, fetchSource)

	h := openHost(t, dir, nil)
	if h.Get("quiet") == nil {
		t.Fatalf("tool did not load: %+v", h.Status())
	}

	_, err := h.Call(context.Background(), "quiet",
		json.RawMessage(`{"url":"https://example.com/"}`))
	if err == nil {
		t.Fatal("an ungranted tool made a request")
	}
	if !strings.Contains(err.Error(), "not granted") {
		t.Errorf("error = %v, want it to name the missing grant", err)
	}
}

// Non-HTTP schemes are a classic way to reach things an HTTP filter never
// considered.
func TestNonHTTPSchemesAreRefused(t *testing.T) {
	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"*.example.com", "example.com"},
		Methods:    []string{"GET"},
	}, fetchSource)

	for _, target := range []string{
		"file:///etc/passwd",
		"gopher://example.com:70/_test",
		"ftp://example.com/x",
		"data:text/plain,hello",
	} {
		t.Run(target, func(t *testing.T) {
			out, err := callTool(t, h, map[string]string{"url": target})
			if err == nil {
				t.Fatalf("scheme was permitted, got %q", out)
			}
			// Asserting the reason, not just that something failed: Go's client
			// refuses these schemes too, so a test that only checked for an error
			// would pass with our own check deleted.
			if !strings.Contains(err.Error(), "scheme") {
				t.Errorf("error = %v, want our scheme refusal", err)
			}
		})
	}
}

// user:pass@host is parser-confusion bait: what a human reads as the host and
// what the client dials can differ.
func TestURLCredentialsAreRefused(t *testing.T) {
	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"api.example.com"},
		Methods:    []string{"GET"},
	}, fetchSource)

	if out, err := callTool(t, h, map[string]string{
		"url": "http://api.example.com@169.254.169.254/",
	}); err == nil {
		t.Fatalf("URL credentials were permitted, got %q", out)
	}
}

// The guest must not be able to desynchronize the connection or lie about its
// target.
func TestForbiddenRequestHeadersAreRefused(t *testing.T) {
	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"api.example.com"},
		Methods:    []string{"GET"},
	}, fetchSource)

	for _, header := range []string{"Host", "Transfer-Encoding", "Content-Length", "Connection"} {
		t.Run(header, func(t *testing.T) {
			_, err := callTool(t, h, map[string]any{
				"url":     "https://api.example.com/x",
				"headers": map[string]string{header: "evil"},
			})
			if err == nil {
				t.Fatalf("%s was accepted from the guest", header)
			}
		})
	}
}

// The body cap keeps a hostile or merely large response out of the daemon's
// memory, before the dispatcher's output cap ever sees it.
func TestResponseBodyIsCapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(strings.Repeat("A", 100_000))) //nolint:errcheck
	}))
	defer srv.Close()

	_, port := hostPortOf(t, srv.URL)
	allowTestLoopback(t)

	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"127.0.0.1"},
		Methods:    []string{"GET"},
		MaxBytes:   1024,
	}, `
export default async ({ url }) => {
  const res = await fetch(url);
  return { length: res.text().length };
};
`)

	out, err := callTool(t, h, map[string]string{"url": "http://127.0.0.1:" + port + "/big"})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `"length":1024`) {
		t.Errorf("output = %q, want the body capped at 1024", out)
	}
}

// The cap must bound what is read, not merely what is returned. An endless
// response with the LimitReader removed would be read until the daemon died —
// truncating afterwards is far too late, and a test that only checks the
// returned length passes either way.
//
// The server here streams forever, so this terminates only if the read is
// actually bounded.
func TestEndlessResponseIsBoundedByTheReadNotTheTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("A", 4096))
		for {
			if _, err := w.Write(chunk); err != nil {
				return // the client hung up, which is the pass condition
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	defer srv.Close()

	_, port := hostPortOf(t, srv.URL)
	allowTestLoopback(t)

	h := httpTool(t, &HTTPGrant{
		AllowHosts: []string{"127.0.0.1"},
		Methods:    []string{"GET"},
		MaxBytes:   8192,
	}, `
export default async ({ url }) => {
  const res = await fetch(url);
  return { length: res.text().length };
};
`)

	done := make(chan string, 1)
	go func() {
		out, err := h.Call(context.Background(), "fetcher",
			json.RawMessage(`{"url":"http://127.0.0.1:`+port+`/endless"}`))
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		done <- out
	}()

	select {
	case out := <-done:
		if !strings.Contains(out, `"length":8192`) {
			t.Errorf("output = %q, want the read capped at 8192", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("reading an endless response did not terminate; the body read is not bounded")
	}
}

// Every outbound call must be auditable: "what did this tool reach, and what
// came back" needs an exact answer after the fact (§8 item 8, §9.3).
func TestOutboundCallsAreAudited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	_, port := hostPortOf(t, srv.URL)
	allowTestLoopback(t)

	var calls []HTTPCall
	dir := t.TempDir()
	writeTool(t, dir, "fetcher", `
name = "fetcher"
kind = "js"
entrypoint = "./fetcher.js"
description = "Fetches."

[capabilities]
net = ["http"]
`, fetchSource)

	ctx := context.Background()
	h, err := Open(ctx, Config{
		UserDir:   dir,
		Grants:    map[string]Grant{"fetcher": {HTTP: &HTTPGrant{AllowHosts: []string{"127.0.0.1"}, Methods: []string{"GET"}}}},
		AuditHTTP: func(c HTTPCall) { calls = append(calls, c) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.Load(ctx, nil)

	if _, err := callTool(t, h, map[string]string{"url": "http://127.0.0.1:" + port + "/thing"}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if len(calls) != 1 {
		t.Fatalf("audited %d calls, want 1", len(calls))
	}
	c := calls[0]
	if c.Tool != "fetcher" || c.Method != "GET" || c.Host != "127.0.0.1" || c.Status != 200 {
		t.Errorf("audit record = %+v", c)
	}
	if c.Bytes != 2 {
		t.Errorf("Bytes = %d, want 2", c.Bytes)
	}
}
