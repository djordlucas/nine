package builtins_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"nine/internal/plugin"
)

// Browser automation, end to end, against the real upstream server.
//
// Nine used to ship its own Playwright wrapper as a Node plugin. It is gone: a
// browser is now an ordinary [[mcp.server]] (docs/browser.md), which means the
// thing worth testing is no longer "do our browser tools work" but "does a real,
// third-party MCP server load and answer through the bridge". The rest of the
// MCP tests run against a compiled fixture in testmcpserver/, which is fast and
// hermetic but agrees with us by construction — it cannot catch a handshake
// quirk, a slow `npx` cold start, or a content shape we guessed wrong. This one
// can, which is exactly why it exists and why it is opt-in.
//
//	NINE_PLAYWRIGHT_TEST=1 go test ./internal/builtins/ -run Playwright
//
// Prerequisites: `npx` on PATH, and a browser for Playwright to drive:
//
//	npx @playwright/mcp@0.0.79 install-browser chrome-for-testing
//
// Note that this is *not* `npx playwright install chromium`. Under
// `--browser chromium` this server resolves a "chrome-for-testing" build it
// manages itself, and fails with a bare "not installed" error if only the
// playwright-managed chromium is present.

// playwrightPackage pins the version this test was validated against. An
// unpinned `@playwright/mcp` would make a green test go red on an upstream
// release with no commit here, which is a bad trade for a test whose whole job
// is to tell us when *our* side broke. Bump it deliberately.
const playwrightPackage = "@playwright/mcp@0.0.79"

// Cold `npx` resolution dominates: the bridge's own default allows 3 minutes
// (mcpHandshakeTimeout) for exactly this reason. Kept explicit so a slow fetch
// reads as a slow fetch rather than a mysterious bridge failure.
const playwrightHandshake = "5m"

// requirePlaywright gates the test. Missing prerequisites *after* opting in are
// a failure, not a skip: an operator who set the variable asked for this to run,
// and silently skipping is how a browser test rots unnoticed.
func requirePlaywright(t *testing.T) {
	t.Helper()
	if os.Getenv("NINE_PLAYWRIGHT_TEST") != "1" {
		t.Skip("playwright MCP test skipped (set NINE_PLAYWRIGHT_TEST=1 to run)")
	}
	if _, err := exec.LookPath("npx"); err != nil {
		t.Fatalf("NINE_PLAYWRIGHT_TEST=1 but npx is not on PATH: %v", err)
	}
}

// startPlaywrightBridge starts an `mcp` bridge pointed at the upstream
// Playwright server, wired exactly as cmd/nine/daemon.go wires an
// [[mcp.server]] — same manager, same instance naming, same env-passed spec.
// Nothing here reaches into the bridge.
func startPlaywrightBridge(t *testing.T) (*plugin.Plugin, *plugin.Manager, string) {
	t.Helper()

	cacheRoot := t.TempDir()
	outputDir := t.TempDir()

	spec, err := json.Marshal(map[string]any{
		"name":    "playwright",
		"command": "npx",
		"args": []string{
			"-y", playwrightPackage,
			"--headless",
			// Upstream defaults to the `chrome` channel — a real Google Chrome
			// install — and fails outright without one. `chromium` selects the
			// portable build the server manages itself (see the install note at
			// the top of this file).
			"--browser", "chromium",
			// Keep the profile in memory so a test run leaves no browser state
			// behind and cannot inherit any.
			"--isolated",
			// The container this may run in is already the isolation boundary,
			// and Chromium's own sandbox needs namespace privileges that are
			// routinely unavailable there.
			"--no-sandbox",
			"--output-dir", outputDir,
		},
	})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	// A cache dir is what makes binary content reachable: the bridge writes
	// image parts there and names the file in its text reply, rather than
	// spending a context window on base64. Persisted so the assertions can read
	// the file back before Stop reclaims the directory.
	m.SetCacheConfig(cacheRoot, func(string) bool { return true })

	p, err := m.StartBuiltinInstance("mcp", plugin.MCPInstanceName("playwright"),
		"NINE_MCP_SERVER="+string(spec),
		"NINE_MCP_HANDSHAKE_TIMEOUT="+playwrightHandshake,
	)
	if err != nil {
		t.Fatalf("start playwright bridge: %v\n"+
			"is a browser installed? try: npx %s install-browser chrome-for-testing",
			err, playwrightPackage)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup
	return p, m, cacheRoot
}

// servePage starts a local page for the browser to visit. Hermetic on purpose:
// the test asserts on the bridge, not on some third party's uptime or markup.
func servePage(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// callTool invokes one tool with a deadline. Driving a real browser is slow enough
// that an unbounded call would hang a broken run instead of failing it.
func callTool(t *testing.T, m *plugin.Manager, p *plugin.Plugin, tool, args string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	res, err := m.Call(ctx, p, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return res.Output
}

// The server's tools arrive under the bridge, prefixed with the name the
// operator gave it in nine.toml. This is the load-bearing claim of the whole
// move: a browser reaches Nine through the ordinary plugin path, with no
// browser-specific code anywhere in the daemon.
func TestPlaywrightMCPAdvertisesPrefixedTools(t *testing.T) {
	requirePlaywright(t)
	p, _, _ := startPlaywrightBridge(t)

	if p.Name != "mcp:playwright" {
		t.Errorf("plugin name = %q, want %q", p.Name, "mcp:playwright")
	}

	got := make(map[string]bool, len(p.Tools))
	for _, tool := range p.Tools {
		got[tool.Name] = true
	}
	// A deliberately small set: the ones the docs promise and the ones the
	// other tests here call. Asserting the full roster would turn every
	// upstream addition into a failure.
	for _, want := range []string{
		"playwright__browser_navigate",
		"playwright__browser_snapshot",
		"playwright__browser_take_screenshot",
	} {
		if !got[want] {
			t.Errorf("tool %q missing; got %d tools: %v", want, len(p.Tools), sortedKeys(got))
		}
	}

	// Unprefixed names must not leak through, or two servers exposing the same
	// tool would collide and one would be silently dropped (R-PLUG.9).
	if got["browser_navigate"] {
		t.Error("tool browser_navigate is unprefixed; the server name was not applied")
	}
}

// A page is fetched and read back through the bridge — the replacement for the
// old browser_navigate + browser_extract pair, and the proof that a round trip
// carrying real page data survives the stdio transport.
//
// What navigate returns is worth knowing before writing an agent against it: at
// this version the reply carries the URL and title inline, but the accessibility
// snapshot is written into --output-dir and merely *linked*, so the page text is
// not in the reply. Content that must land in the model's context has to be
// asked for explicitly, which is what browser_evaluate does below.
func TestPlaywrightMCPNavigatesAndReadsPage(t *testing.T) {
	requirePlaywright(t)
	p, m, _ := startPlaywrightBridge(t)

	const marker = "nine-mcp-browser-marker"
	const title = "Nine MCP"
	url := servePage(t, `<!doctype html><title>`+title+`</title><h1>`+marker+`</h1>`)

	out := callTool(t, m, p, "playwright__browser_navigate",
		`{"url":`+jsonString(url)+`}`)
	if !strings.Contains(out, title) {
		t.Errorf("navigate output does not report the page title %q; got:\n%s", title, out)
	}
	if !strings.Contains(out, url) {
		t.Errorf("navigate output does not report the page URL %q; got:\n%s", url, out)
	}

	// Page text, inline. This is the call that actually puts content in front of
	// the model, and the one an agent reaches for where it used to use
	// browser_extract.
	got := callTool(t, m, p, "playwright__browser_evaluate",
		`{"function":"() => document.body.innerText"}`)
	if !strings.Contains(got, marker) {
		t.Errorf("evaluate did not return the page marker %q; got:\n%s", marker, got)
	}
}

// The content path that had no real-server coverage: a screenshot comes back as
// a text summary *and* an image part. The image must reach the cache dir and be
// named in the reply — dropping it handed the model a confident description of
// a picture it never received.
func TestPlaywrightMCPScreenshotIsSaved(t *testing.T) {
	requirePlaywright(t)
	p, m, cacheRoot := startPlaywrightBridge(t)

	url := servePage(t, `<!doctype html><title>Shot</title><h1>shot</h1>`)
	callTool(t, m, p, "playwright__browser_navigate", `{"url":`+jsonString(url)+`}`)

	out := callTool(t, m, p, "playwright__browser_take_screenshot", `{}`)

	if strings.Contains(out, "not saved") {
		t.Fatalf("screenshot was not persisted; the bridge had no cache dir:\n%s", out)
	}
	// Base64 must not be inlined into the reply — that is the regression the
	// cache-dir handoff exists to prevent.
	if strings.Contains(out, "iVBORw0KGgo") {
		t.Error("screenshot reply inlines base64 PNG data instead of a file path")
	}

	saved := findFiles(t, cacheRoot)
	if len(saved) == 0 {
		t.Fatalf("no file written under the plugin cache dir %s; reply was:\n%s", cacheRoot, out)
	}
	// The reply has to name the file, or the bytes are unreachable in practice.
	var named bool
	for _, f := range saved {
		if strings.Contains(out, filepath.Base(f)) {
			named = true
		}
	}
	if !named {
		t.Errorf("reply does not name any saved file %v; got:\n%s", saved, out)
	}
}

// findFiles lists every regular file under root.
func findFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Size() > 0 {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// jsonString JSON-quotes a string for embedding in a hand-written args literal.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// sortedKeys is a stable rendering of a name set, for failure messages.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
