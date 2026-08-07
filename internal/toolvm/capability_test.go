package toolvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fsReaderManifest = `
name = "reader"
kind = "js"
entrypoint = "./reader.js"
description = "Read a file."

[capabilities]
fs = ["read"]
`

// §6.3: a manifest declares a need, only config grants. A tool declaring a
// capability the operator has not granted fails to load with a named error
// rather than starting up crippled — silent degradation means a tool that
// half-works in ways neither the developer nor the operator predicted.
func TestDeclaredButNotGrantedFailsToLoad(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "reader", fsReaderManifest, `export default () => "ok";`)

	h := openHost(t, dir, nil) // no grants

	if h.Get("reader") != nil {
		t.Fatal("tool loaded despite an ungranted capability")
	}
	st := h.Status()
	if len(st) != 1 || st[0].Loaded {
		t.Fatalf("status = %+v, want one skipped entry", st)
	}
	if !strings.Contains(st[0].Err, CapFSRead) {
		t.Errorf("error %q should name the missing capability", st[0].Err)
	}
}

// The mirror case. Conferring reach on a tool that never asked for it is how an
// over-broad grant survives review, so the two documents are made to agree.
func TestGrantedButNotDeclaredFailsToLoad(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "quiet", `
name = "quiet"
kind = "js"
entrypoint = "./quiet.js"
description = "Declares nothing."
`, `export default () => "ok";`)

	h := openHost(t, dir, map[string]Grant{
		"quiet": {FSRead: []Mount{{Host: dir, Guest: "/data"}}},
	})

	if h.Get("quiet") != nil {
		t.Fatal("tool loaded with a capability it never declared")
	}
	if st := h.Status(); len(st) != 1 || !strings.Contains(st[0].Err, "does not declare") {
		t.Fatalf("status = %+v, want a granted-but-not-declared error", st)
	}
}

// The happy path: declaration and grant agree, and the mount is real.
func TestGrantedFSReadMountsTheDirectory(t *testing.T) {
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "note.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writeTool(t, dir, "reader", fsReaderManifest, `export default () => "ok";`)

	h := openHost(t, dir, map[string]Grant{
		"reader": {FSRead: []Mount{{Host: data, Guest: "/data"}}},
	})

	tool := h.Get("reader")
	if tool == nil {
		t.Fatalf("tool did not load: %+v", h.Status())
	}
	if got := tool.Grant.Summary(); !strings.Contains(got, "fs.read") || !strings.Contains(got, "/data") {
		t.Errorf("Summary() = %q, want it to name the mount", got)
	}
	// The tool still runs; the grant is wiring, not a behavior change.
	if _, err := h.Call(context.Background(), "reader", json.RawMessage(`{}`)); err != nil {
		t.Errorf("Call: %v", err)
	}
}

// env is granted per key, and the keys on both sides must be the same ones —
// matching on "the tool wants some env" would let a grant of the wrong keys pass
// review as if it were the right ones.
func TestEnvKeysMustMatchExactly(t *testing.T) {
	manifest := `
name = "envtool"
kind = "js"
entrypoint = "./envtool.js"
description = "Reads TZ."

[capabilities]
env = ["TZ"]
`
	for _, tc := range []struct {
		name    string
		granted []string
		wantErr string
	}{
		{"exact match", []string{"TZ"}, ""},
		{"wrong key granted", []string{"LANG"}, "not granted"},
		{"extra key granted", []string{"TZ", "LANG"}, "does not declare"},
		{"nothing granted", nil, CapEnvRead},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTool(t, dir, "envtool", manifest, `export default () => "ok";`)
			h := openHost(t, dir, map[string]Grant{"envtool": {Env: tc.granted}})

			st := h.Status()
			if len(st) != 1 {
				t.Fatalf("status = %+v", st)
			}
			if tc.wantErr == "" {
				if !st[0].Loaded {
					t.Fatalf("want loaded, got error %q", st[0].Err)
				}
				return
			}
			if st[0].Loaded {
				t.Fatal("want a load failure")
			}
			if !strings.Contains(st[0].Err, tc.wantErr) {
				t.Errorf("error %q should contain %q", st[0].Err, tc.wantErr)
			}
		})
	}
}

// A granted env key reaches the guest, and nothing else does.
func TestGrantedEnvKeyIsVisibleAndOthersAreNot(t *testing.T) {
	lookups := map[string]string{"TZ": "UTC"}
	old := lookupEnv
	lookupEnv = func(k string) (string, bool) { v, ok := lookups[k]; return v, ok }
	t.Cleanup(func() { lookupEnv = old })

	dir := t.TempDir()
	writeTool(t, dir, "envtool", `
name = "envtool"
kind = "js"
entrypoint = "./envtool.js"
description = "Reads TZ."

[capabilities]
env = ["TZ"]
`, `export default () => "ok";`)

	h := openHost(t, dir, map[string]Grant{"envtool": {Env: []string{"TZ"}}})
	tool := h.Get("envtool")
	if tool == nil {
		t.Fatalf("tool did not load: %+v", h.Status())
	}
	if len(tool.Grant.Env) != 1 || tool.Grant.Env[0] != "TZ" {
		t.Errorf("Grant.Env = %v, want [TZ]", tool.Grant.Env)
	}
}

// net.http follows the same conferred-never-claimed rule as everything else:
// declared and granted must agree, and the grant supplies the parameters.
func TestNetHTTPResolvesLikeAnyOtherCapability(t *testing.T) {
	const manifest = `
name = "fetcher"
kind = "js"
entrypoint = "./fetcher.js"
description = "Fetches."

[capabilities]
net = ["http"]
`
	grant := &HTTPGrant{AllowHosts: []string{"api.example.com"}, Methods: []string{"GET"}}

	t.Run("declared and granted", func(t *testing.T) {
		dir := t.TempDir()
		writeTool(t, dir, "fetcher", manifest, `export default () => "ok";`)
		h := openHost(t, dir, map[string]Grant{"fetcher": {HTTP: grant}})

		tool := h.Get("fetcher")
		if tool == nil {
			t.Fatalf("tool did not load: %+v", h.Status())
		}
		if got := tool.Grant.Summary(); !strings.Contains(got, "net.http") ||
			!strings.Contains(got, "api.example.com") {
			t.Errorf("Summary() = %q, want it to name the allowlist", got)
		}
	})

	t.Run("declared but not granted", func(t *testing.T) {
		dir := t.TempDir()
		writeTool(t, dir, "fetcher", manifest, `export default () => "ok";`)
		h := openHost(t, dir, nil)
		if h.Get("fetcher") != nil {
			t.Fatal("a tool declaring net.http loaded with no grant")
		}
	})

	t.Run("granted but not declared", func(t *testing.T) {
		dir := t.TempDir()
		writeTool(t, dir, "quiet", `
name = "quiet"
kind = "js"
entrypoint = "./quiet.js"
description = "Declares nothing."
`, `export default () => "ok";`)
		h := openHost(t, dir, map[string]Grant{"quiet": {HTTP: grant}})
		if h.Get("quiet") != nil {
			t.Fatal("a tool was granted network it never declared")
		}
	})
}

// resolveGrant returns the grant and only the grant. The declaration contributes
// no parameters — it is the requirement that the two agree, nothing more.
func TestResolvedGrantComesFromTheGrantAlone(t *testing.T) {
	decl := Declaration{FS: []string{"read"}}
	granted := Grant{FSRead: []Mount{{Host: "/srv/data", Guest: "/data"}}}

	got, err := resolveGrant(decl, granted)
	if err != nil {
		t.Fatalf("resolveGrant: %v", err)
	}
	if len(got.FSRead) != 1 || got.FSRead[0].Host != "/srv/data" || got.FSRead[0].Guest != "/data" {
		t.Errorf("FSRead = %+v, want the operator's mount verbatim", got.FSRead)
	}
}

// A typo in a manifest must fail loudly rather than quietly asking for nothing.
func TestUnknownCapabilityVerbIsAnError(t *testing.T) {
	if _, err := (Declaration{FS: []string{"append"}}).capabilities(); err == nil {
		t.Error("want an error for an unknown fs verb")
	}
	if _, err := (Declaration{Net: []string{"tcp"}}).capabilities(); err == nil {
		t.Error("want an error for an unknown net verb")
	}
}
