package toolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTool drops a manifest and its entrypoint into dir, the two-file layout an
// operator actually installs.
func writeTool(t *testing.T, dir, name, manifest, source string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if source != "" {
		if err := os.WriteFile(filepath.Join(dir, name+".js"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// openHost opens a host over dir with the given grants and loads it.
func openHost(t *testing.T, dir string, grants map[string]Grant) *Host {
	t.Helper()
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: dir, Grants: grants})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.Load(ctx, nil)
	return h
}

// call runs a tool and fails the test if it errors.
func call(t *testing.T, h *Host, name, args string) string {
	t.Helper()
	out, err := h.Call(context.Background(), name, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Call(%s): %v", name, err)
	}
	return out
}

const echoManifest = `
name = "echo"
kind = "js"
entrypoint = "./echo.js"
description = "Echo a value back."
`

func TestJSToolRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "echo", echoManifest,
		`export default ({ value }) => ({ echoed: value, doubled: value * 2 });`)

	h := openHost(t, dir, nil)

	if got := len(h.Tools()); got != 1 {
		t.Fatalf("loaded %d tools, want 1: %+v", got, h.Status())
	}
	if out := call(t, h, "echo", `{"value":21}`); out != `{"echoed":21,"doubled":42}` {
		t.Errorf("output = %q", out)
	}
}

// A string result is the tool's own formatting and must pass through untouched,
// since CallResult.Output is what the model reads.
func TestJSToolStringResultPassesThrough(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "greet", strings.ReplaceAll(echoManifest, "echo", "greet"),
		`export default ({ name }) => "hello, " + name;`)

	h := openHost(t, dir, nil)
	if out := call(t, h, "greet", `{"name":"world"}`); out != "hello, world" {
		t.Errorf("output = %q, want %q", out, "hello, world")
	}
}

func TestJSToolAsyncResultIsAwaited(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "echo", echoManifest,
		`export default async ({ value }) => { await null; return { v: value }; };`)

	h := openHost(t, dir, nil)
	if out := call(t, h, "echo", `{"value":7}`); out != `{"v":7}` {
		t.Errorf("output = %q", out)
	}
}

// A thrown error is the tool's failure, not the host's: it must reach the model
// as an ordinary tool error carrying the message, so the model can fix its
// arguments rather than give up on the tool.
func TestJSToolThrowIsAToolError(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "echo", echoManifest,
		`export default () => { throw new Error("date is not ISO-8601"); };`)

	h := openHost(t, dir, nil)
	_, err := h.Call(context.Background(), "echo", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "date is not ISO-8601") {
		t.Errorf("error = %v, want it to carry the thrown message", err)
	}
}

// A syntax error must be a legible failure, not a crash or an empty result.
func TestJSToolSyntaxErrorIsReported(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "echo", echoManifest, `export default ( => {`)

	h := openHost(t, dir, nil)
	_, err := h.Call(context.Background(), "echo", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("want an error")
	}
}

// Per-call instantiation is the strongest property in the design
// (docs/sandboxed-tools.md §3). If a global could survive, two calls could
// observe each other and a tool could accumulate state across a session.
func TestNoStateSurvivesACall(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "counter", strings.ReplaceAll(echoManifest, "echo", "counter"), `
		globalThis.n = (globalThis.n || 0) + 1;
		export default () => ({ n: globalThis.n });
	`)

	h := openHost(t, dir, nil)
	for i := 0; i < 3; i++ {
		if out := call(t, h, "counter", `{}`); out != `{"n":1}` {
			t.Fatalf("call %d returned %q; state leaked across calls", i, out)
		}
	}
}

// The wall clock is the only CPU bound wazero offers, so a spinning tool must
// actually die rather than pin a core forever.
func TestRunawayToolHitsTheDeadline(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "spin", strings.ReplaceAll(echoManifest, "echo", "spin"),
		`export default () => { for (;;) {} };`)

	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: dir, Timeout: 250 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.Load(ctx, nil)

	start := time.Now()
	if _, err := h.Call(ctx, "spin", json.RawMessage(`{}`)); err == nil {
		t.Fatal("want a timeout error")
	} else if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want a timeout", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("took %s to enforce a 250ms deadline", elapsed)
	}
}

// Regression. QuickJS requires `buf[buf_len] == 0` for JS_Eval and JS_ParseJSON,
// and the host originally allocated exactly len bytes for the call envelope —
// leaving the terminator to be whatever the guest allocator happened to have
// left there.
//
// The failure mode is the nasty kind: it depended on nothing but the total byte
// length of the input, so a tool ran or did not according to how long its source
// happened to be, with "unexpected data at the end" as the only clue. About one
// length in sixteen failed. A single fixed-size fixture would very likely have
// missed it, which is why this sweeps.
func TestGuestInputIsNULTerminatedAtEveryLength(t *testing.T) {
	dir := t.TempDir()
	// One host, reloaded per iteration: opening a new one recompiles the ~1 MB
	// interpreter each time, which turns a 0.3s test into a 20s one.
	writeTool(t, dir, "probe", strings.ReplaceAll(echoManifest, "echo", "probe"), `export default () => "ok";`)
	h := openHost(t, dir, nil)

	for pad := 0; pad < 96; pad++ {
		// Padding with spaces keeps the source valid JS at every length, so the
		// only variable is the envelope's size.
		src := `export default () => "ok";` + strings.Repeat(" ", pad)
		if err := os.WriteFile(filepath.Join(dir, "probe.js"), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		h.Load(context.Background(), nil)

		out, err := h.Call(context.Background(), "probe", json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("pad=%d (envelope length %d bytes): %v", pad, pad, err)
		}
		if out != "ok" {
			t.Fatalf("pad=%d: output = %q", pad, out)
		}
	}
}

// The same sweep on the argument side: arguments are JSON the host writes into
// the same buffer, so their length moves the envelope through the same residues.
func TestArgumentsOfEveryLengthRoundTrip(t *testing.T) {
	dir := t.TempDir()
	writeTool(t, dir, "echo", echoManifest, `export default ({ s }) => ({ n: (s ?? "").length });`)
	h := openHost(t, dir, nil)

	for n := 0; n < 96; n++ {
		args, err := json.Marshal(map[string]string{"s": strings.Repeat("x", n)})
		if err != nil {
			t.Fatal(err)
		}
		out, err := h.Call(context.Background(), "echo", args)
		if err != nil {
			t.Fatalf("argument length %d: %v", n, err)
		}
		if want := fmt.Sprintf(`{"n":%d}`, n); out != want {
			t.Fatalf("argument length %d: output = %q, want %q", n, out, want)
		}
	}
}
