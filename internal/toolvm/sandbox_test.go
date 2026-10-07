package toolvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probe runs a one-off JS tool whose whole body is `expr` and returns what the
// model would see, or the error. Used below to ask the interpreter directly what
// it can reach.
func probe(t *testing.T, expr string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	writeTool(t, dir, "probe", `
name = "probe"
kind = "js"
entrypoint = "./probe.js"
description = "probe"
`, "export default () => "+expr+";")
	h := openHost(t, dir, nil)
	return h.Call(context.Background(), "probe", json.RawMessage(`{}`))
}

// §4.1 is a hard requirement, not a hardening nicety: QuickJS-NG ships `std` and
// `os` as separate opt-in init calls, and the stock qjs CLI links both. Between
// them they expose a filesystem API, os.exec, std.urlGet, and std.evalScript —
// in scope before any capability has been granted. build.sh links neither, and
// this is the test that stops a future version bump from quietly reintroducing
// them.
//
// A behavioral assertion rather than a symbol-table one: what matters is not
// that the symbols are absent from the blob but that the *guest* cannot reach
// them, which is the property an operator is relying on.
func TestInterpreterHasNoStdOrOSModule(t *testing.T) {
	// The globals quickjs-libc's js_std_add_helpers installs. Every one of them
	// must be absent; `print` and `scriptArgs` are the canaries, since they are
	// what a build that linked the CLI helpers would drag in alongside std and os.
	for _, global := range []string{"std", "os", "scriptArgs", "print"} {
		t.Run(global, func(t *testing.T) {
			out, err := probe(t, "typeof "+global)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if out != "undefined" {
				t.Errorf("typeof %s = %q, want undefined — this build must not link std or os", global, out)
			}
		})
	}
}

// The same check from the import side. `import` is the documented way to reach
// std and os, so it is the path that has to fail.
func TestImportingStdOrOSIsRefused(t *testing.T) {
	for _, spec := range []string{"std", "os", "qjs:std", "qjs:os"} {
		t.Run(spec, func(t *testing.T) {
			dir := t.TempDir()
			writeTool(t, dir, "probe", `
name = "probe"
kind = "js"
entrypoint = "./probe.js"
description = "probe"
`, `import * as m from "`+spec+`"; export default () => "reached";`)
			h := openHost(t, dir, nil)
			out, err := h.Call(context.Background(), "probe", json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("importing %q succeeded and returned %q", spec, out)
			}
			if !strings.Contains(err.Error(), "not available") {
				t.Errorf("error = %v, want the allowlist refusal", err)
			}
		})
	}
}

// §4.3: module resolution happens in the host, against a closed allowlist. The
// guest never receives a resolver that can touch disk or network. Without this,
// `import` is a capability-model bypass hiding in plain sight — a tool granted
// nothing could import a *developer* tool's bundle and run code the operator
// approved for a different purpose, and an fs-reading resolver is an ungranted
// fs.read by another name.
func TestModuleResolutionIsClosed(t *testing.T) {
	// A real file on disk, so a resolver that reaches the filesystem would
	// actually find something and the test would notice.
	dir := t.TempDir()
	neighbor := filepath.Join(dir, "neighbor.js")
	if err := os.WriteFile(neighbor, []byte(`export const secret = "leaked";`), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, spec := range []string{
		"./neighbor.js",
		"../neighbor.js",
		neighbor,
		"/etc/passwd",
		"file://" + neighbor,
		"https://cdn.example.com/lodash-es.js",
		"lodash-es",
		"node:fs",
	} {
		t.Run(spec, func(t *testing.T) {
			d := t.TempDir()
			writeTool(t, d, "probe", `
name = "probe"
kind = "js"
entrypoint = "./probe.js"
description = "probe"
`, `import * as m from "`+spec+`"; export default () => "reached";`)
			h := openHost(t, d, nil)
			if out, err := h.Call(context.Background(), "probe", json.RawMessage(`{}`)); err == nil {
				t.Fatalf("importing %q succeeded and returned %q", spec, out)
			}
		})
	}
}

// Dynamic import must inherit the same refusal — a resolver that is closed for
// static imports and open for dynamic ones is not closed.
func TestDynamicImportIsRefused(t *testing.T) {
	out, err := probe(t, `import("/etc/passwd").then(() => "reached", (e) => "refused")`)
	if err != nil {
		// Refused synchronously is also a pass.
		return
	}
	if out != "refused" {
		t.Errorf("dynamic import returned %q, want it refused", out)
	}
}

// The default is empty (§6.1): a tool granted nothing has no filesystem. wazero
// enforces this itself — with no pre-opens there is nothing for path_open to
// open — but the property is load-bearing enough to assert.
func TestNoFilesystemWithoutAGrant(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	// quickjs-libc's std and os stay unlinked, so the ambient filesystem API a
	// stock qjs would have does not exist here.
	out, err := probe(t, `typeof std === "undefined" && typeof os === "undefined" ? "no-fs-api" : "has-fs-api"`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "no-fs-api" {
		t.Errorf("got %q: the guest has a filesystem API with no grant", out)
	}

	// Since nine:fs exists (adr/rich-js-tools.md §6.4), the stronger claim is
	// the one that matters: bypassing the module's own capability check and
	// calling the primitive directly must still reach nothing, because with no
	// grant the instance has no pre-opens and there is nothing to open. What
	// protects the filesystem is wazero, not the JavaScript in fs.js.
	out, err = probe(t, `(() => {
    const I = globalThis[Symbol.for("nine.internal")];
    if (!I) return "no internals";
    for (const p of ["`+secret+`", "/etc/passwd", "/", "/data"]) {
      try { I.fsRead(p); return "READ " + p; } catch (e) { /* expected */ }
      try { I.fsReadDir(p); return "LISTED " + p; } catch (e) { /* expected */ }
    }
    return "nothing reachable";
  })()`)
	if err != nil {
		t.Fatal(err)
	}
	if out != "nothing reachable" {
		t.Errorf("the guest reached the filesystem with no grant: %s", out)
	}
}

// The environment is denied by default, and that matters more than most: the
// daemon's environment holds LLM provider API keys.
func TestNoEnvironmentWithoutAGrant(t *testing.T) {
	t.Setenv("NINE_TEST_SECRET", "classified")
	// WASI exposes environ through the runtime, not through a JS global, so the
	// assertion that counts is that the module config carried no env at all.
	dir := t.TempDir()
	writeTool(t, dir, "probe", `
name = "probe"
kind = "js"
entrypoint = "./probe.js"
description = "probe"
`, `export default () => "ok";`)
	h := openHost(t, dir, nil)

	tool := h.Get("probe")
	if tool == nil {
		t.Fatal("tool not loaded")
	}
	if len(tool.Grant.Env) != 0 {
		t.Errorf("Grant.Env = %v, want empty", tool.Grant.Env)
	}
	if got := tool.Grant.Summary(); got != "none" {
		t.Errorf("Summary() = %q, want %q", got, "none")
	}
}

// A developer tool's import allowlist is the curated nine:* set and nothing else
// (§4.2/§4.3). Third-party dependencies are still pre-bundled at development
// time — Nine resolves nothing — but the embedded, pure-ES stdlib is admitted:
// withholding it from hand-written tools was a leftover from before the
// generated tier existed, not a decision (adr/rich-js-tools.md §6.6).
func TestDeveloperToolImportsAreTheStdlibAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "probe", `
name = "probe"
kind = "js"
entrypoint = "./probe.js"
description = "probe"
`, `export default () => "ok";`)
	h := openHost(t, dir, nil)

	got := h.Get("probe").Imports()
	want := map[string]bool{
		"nine:csv": true, "nine:date": true, "nine:diff": true,
		"nine:html": true, "nine:fs": true, "nine:env": true,
		"nine:state": true, "nine:job": true, "nine:process": true,
	}
	if len(got) != len(want) {
		t.Fatalf("Imports() = %v, want exactly the nine:* stdlib", got)
	}
	for _, spec := range got {
		if !want[spec] {
			t.Errorf("Imports() includes %q, which is not part of the stdlib", spec)
		}
	}
}

// The point of the allowlist is what it excludes. A developer tool importing
// anything outside it must still fail, which is what keeps "Nine resolves no
// dependencies" true.
func TestDeveloperToolCannotImportOutsideTheStdlib(t *testing.T) {
	// "nine:tool" is deliberately absent from this list: it resolves to the
	// calling tool's own source, so importing it is a self-reference rather than
	// a way to reach anything (js.go).
	for _, spec := range []string{"lodash", "./helper.js", "/etc/passwd", "https://x.example/m.js", "nine:sqlite"} {
		t.Run(spec, func(t *testing.T) {
			dir := t.TempDir()
			writeTool(t, dir, "probe", `
name = "probe"
kind = "js"
entrypoint = "./probe.js"
description = "probe"
`, `import x from "`+spec+`";
export default () => "ok";`)
			h := openHost(t, dir, nil)
			if _, err := h.Call(context.Background(), "probe", json.RawMessage(`{}`)); err == nil {
				t.Errorf("importing %q was allowed", spec)
			}
		})
	}
}

// console is the `log` capability, granted by default because it leaks nothing.
// QuickJS itself has no console — the stock one comes from quickjs-libc, which
// this build does not link — so the harness has to provide it.
func TestConsoleIsAvailable(t *testing.T) {
	out, err := probe(t, `(console.log("hello from a tool"), "logged")`)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if out != "logged" {
		t.Errorf("output = %q", out)
	}
}

// clock and random are granted by default for the same reason.
func TestClockAndRandomAreGranted(t *testing.T) {
	out, err := probe(t, `Date.now() > 0 && Math.random() >= 0 ? "ok" : "no"`)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if out != "ok" {
		t.Errorf("output = %q, want clock and random to work", out)
	}
}
