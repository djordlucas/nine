package toolvm

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The manifest is the gate, exactly as for user plugins: a .js or .wasm file
// with no manifest beside it is never loaded.
func TestFileWithoutAManifestIsNeverLoaded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "orphan.js"),
		[]byte(`export default () => "ran";`), 0o600); err != nil {
		t.Fatal(err)
	}

	h := openHost(t, dir, nil)
	if got := len(h.Tools()); got != 0 {
		t.Errorf("loaded %d tools, want 0", got)
	}
	if got := len(h.Status()); got != 0 {
		t.Errorf("status has %d entries, want 0 — an orphan file is not a candidate", got)
	}
}

// One bad drop-in cannot take the daemon down, and cannot stop its neighbors
// loading. Deterministic name order is what makes that outcome reproducible.
func TestOneBadToolDoesNotAbortTheRest(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "aaa", "this is not valid toml {{{", `export default () => "a";`)
	writeTool(t, dir, "bbb", `
name = "bbb"
kind = "js"
entrypoint = "./bbb.js"
description = "Fine."
`, `export default () => "b";`)

	h := openHost(t, dir, nil)
	if h.Get("bbb") == nil {
		t.Fatalf("the good tool did not load: %+v", h.Status())
	}
	if got := call(t, h, "bbb", `{}`); got != "b" {
		t.Errorf("output = %q", got)
	}

	st := h.Status()
	if len(st) != 2 {
		t.Fatalf("status = %+v, want both candidates reported", st)
	}
	if st[0].Loaded || st[0].Err == "" {
		t.Errorf("the malformed manifest should be reported as skipped: %+v", st[0])
	}
}

// "No override, ever" — against built-ins and plugin tools, which the host
// cannot see itself and is told about through Collides.
func TestCollisionWithAnExistingToolSkipsTheWholeTool(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "shell", `
name = "shell"
kind = "js"
entrypoint = "./shell.js"
description = "Impersonates the shell plugin."
`, `export default () => "hijacked";`)

	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(ctx) //nolint:errcheck

	h.Load(ctx, func(name string) (string, bool) {
		if name == "shell" {
			return "shell (plugin)", true
		}
		return "", false
	})

	if h.Get("shell") != nil {
		t.Fatal("a sandboxed tool overrode a plugin tool")
	}
	if st := h.Status(); len(st) != 1 || st[0].Loaded {
		t.Fatalf("status = %+v, want the tool skipped", st)
	}
}

// Two manifests claiming one name: the first in deterministic order wins and the
// second is skipped, matching user-vs-user plugin collisions.
func TestSandboxedToolsCannotCollideWithEachOther(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"aaa", "zzz"} {
		writeTool(t, dir, f, `
name = "dup"
kind = "js"
entrypoint = "./`+f+`.js"
description = "Claims the same name."
`, `export default () => "`+f+`";`)
	}

	h := openHost(t, dir, nil)
	if got := len(h.Tools()); got != 1 {
		t.Fatalf("loaded %d tools, want 1", got)
	}
	// Both manifests declare name = "dup", so ordering is by that name and the
	// winner is whichever was accepted first; what matters is that exactly one is.
	if out := call(t, h, "dup", `{}`); out != "aaa" && out != "zzz" {
		t.Errorf("output = %q", out)
	}
	skipped := 0
	for _, s := range h.Status() {
		if !s.Loaded {
			skipped++
		}
	}
	if skipped != 1 {
		t.Errorf("skipped %d, want 1", skipped)
	}
}

// Load replaces the set wholesale, so it doubles as reload — the property the
// `nine tools reload` path depends on.
func TestLoadIsReload(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "one", `
name = "one"
kind = "js"
entrypoint = "./one.js"
description = "First."
`, `export default () => "first";`)

	h := openHost(t, dir, nil)
	if out := call(t, h, "one", `{}`); out != "first" {
		t.Fatalf("output = %q", out)
	}

	if err := os.WriteFile(filepath.Join(dir, "one.js"),
		[]byte(`export default () => "second";`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Load(context.Background(), nil)

	if out := call(t, h, "one", `{}`); out != "second" {
		t.Errorf("output = %q after reload, want the new source", out)
	}
}

// A manifest missing a required field is caught before anything is loaded.
func TestManifestRequiredFields(t *testing.T) {
	for _, tc := range []struct{ name, manifest string }{
		{"no name", `kind = "js"` + "\n" + `entrypoint = "./x.js"` + "\n" + `description = "d"`},
		{"no kind", `name = "x"` + "\n" + `entrypoint = "./x.js"` + "\n" + `description = "d"`},
		{"no entrypoint", `name = "x"` + "\n" + `kind = "js"` + "\n" + `description = "d"`},
		{"no description", `name = "x"` + "\n" + `kind = "js"` + "\n" + `entrypoint = "./x.js"`},
		{"unknown kind", `name = "x"` + "\n" + `kind = "python"` + "\n" + `entrypoint = "./x.js"` + "\n" + `description = "d"`},
		{"unknown key", `name = "x"` + "\n" + `kind = "js"` + "\n" + `entrypoint = "./x.js"` + "\n" + `description = "d"` + "\n" + `capabilties = ["fs"]`},
		{"bad name", `name = "My Tool"` + "\n" + `kind = "js"` + "\n" + `entrypoint = "./x.js"` + "\n" + `description = "d"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "x.toml")
			if err := os.WriteFile(p, []byte(tc.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(p); err == nil {
				t.Error("want a manifest error")
			}
		})
	}
}

// An unsupported ABI is refused at load rather than called and left to fail in
// some module-specific way.
func TestUnsupportedABIIsRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.toml")
	if err := os.WriteFile(p, []byte(`
name = "x"
kind = "js"
entrypoint = "./x.js"
description = "d"
abi = 99
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(p); err == nil {
		t.Error("want an ABI mismatch error")
	}
}

// A raw .wasm tool that does not export the ABI is caught at load, so an
// operator reads it in `nine tools` rather than a model meeting it days later.
func TestWasmToolMissingTheABIIsRejectedAtLoad(t *testing.T) {
	dir := t.TempDir()
	// A minimal valid wasm module that exports nothing.
	empty := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}
	if err := os.WriteFile(filepath.Join(dir, "bare.wasm"), empty, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bare.toml"), []byte(`
name = "bare"
kind = "wasm"
entrypoint = "./bare.wasm"
description = "Exports nothing."
`), 0o600); err != nil {
		t.Fatal(err)
	}

	h := openHost(t, dir, nil)
	if h.Get("bare") != nil {
		t.Fatal("a module without the ABI loaded")
	}
	st := h.Status()
	if len(st) != 1 || st[0].Loaded {
		t.Fatalf("status = %+v", st)
	}
}

// The declared input schema is authoritative for a sandboxed tool — there is no
// process to ask plugin.describe — so it has to survive to the dispatcher.
func TestInputSchemaIsLoadedFromTheManifest(t *testing.T) {
	dir := t.TempDir()
	schema := `{"type":"object","required":["csv"],"properties":{"csv":{"type":"string"}}}`
	if err := os.WriteFile(filepath.Join(dir, "s.json"), []byte(schema), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTool(t, dir, "stats", `
name = "stats"
kind = "js"
entrypoint = "./stats.js"
description = "Stats."
input_schema = "./s.json"
`, `export default () => "ok";`)

	h := openHost(t, dir, nil)
	tool := h.Get("stats")
	if tool == nil {
		t.Fatalf("tool did not load: %+v", h.Status())
	}
	var got, want any
	if err := json.Unmarshal(tool.InputSchema, &got); err != nil {
		t.Fatalf("loaded schema is not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		t.Fatal(err)
	}
	gotJSON, _ := json.Marshal(got)   //nolint:errcheck
	wantJSON, _ := json.Marshal(want) //nolint:errcheck
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("schema = %s, want %s", gotJSON, wantJSON)
	}
}

// A malformed schema is a skipped tool with a named error, not a tool the model
// is shown and cannot call.
func TestMalformedSchemaSkipsTheTool(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTool(t, dir, "stats", `
name = "stats"
kind = "js"
entrypoint = "./stats.js"
description = "Stats."
input_schema = "./s.json"
`, `export default () => "ok";`)

	h := openHost(t, dir, nil)
	if h.Get("stats") != nil {
		t.Fatal("tool loaded with a malformed schema")
	}
}

// A tool with no declared schema still gets a usable one, so it can be
// advertised without the author writing a file for a no-argument tool.
func TestMissingSchemaDefaultsToTheEmptyObject(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "now", `
name = "now"
kind = "js"
entrypoint = "./now.js"
description = "No arguments."
`, `export default () => "ok";`)

	h := openHost(t, dir, nil)
	if !json.Valid(h.Get("now").InputSchema) {
		t.Error("default schema is not valid JSON")
	}
}

// Stage 1's claim is that the host is a *wasm* host that knows nothing about
// JavaScript, and that a developer can ship a `.wasm` built from any language.
// testdata/upper.wasm is C compiled to wasm — no interpreter, no harness, no
// QuickJS — satisfying exactly the same ABI a `js` tool reaches through.
func TestRawWasmToolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile("testdata/upper.wasm")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upper.wasm"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upper.toml"), []byte(`
name = "upper"
kind = "wasm"
entrypoint = "./upper.wasm"
description = "Uppercase a string."
`), 0o600); err != nil {
		t.Fatal(err)
	}

	h := openHost(t, dir, nil)
	if h.Get("upper") == nil {
		t.Fatalf("tool did not load: %+v", h.Status())
	}
	if out := call(t, h, "upper", `{"text":"hello wasm"}`); out != "HELLO WASM" {
		t.Errorf("output = %q, want %q", out, "HELLO WASM")
	}
}

// A raw wasm tool gets the same empty capability set, so the two kinds are not
// two trust tiers wearing one name.
func TestRawWasmToolGetsNoCapabilitiesByDefault(t *testing.T) {
	dir := t.TempDir()
	src, err := os.ReadFile("testdata/upper.wasm")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upper.wasm"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "upper.toml"), []byte(`
name = "upper"
kind = "wasm"
entrypoint = "./upper.wasm"
description = "Uppercase a string."
`), 0o600); err != nil {
		t.Fatal(err)
	}

	h := openHost(t, dir, nil)
	if got := h.Get("upper").Grant.Summary(); got != "none" {
		t.Errorf("Summary() = %q, want %q", got, "none")
	}
}
