package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// examplesDir is the shipped worked examples, which are deliberately outside any
// tool directory (nothing scans them) and therefore reached by path here.
const examplesDir = "../../examples/tools"

// stageExample copies one shipped example into a scratch tool directory.
func stageExample(t *testing.T, name string, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(examplesDir, f))
		if err != nil {
			t.Fatalf("reading shipped example %s: %v", f, err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The committed sha256.wasm is the only artifact that exercises nine.h end to
// end — the header is C, so nothing else in the Go suite can reach it. Asserting
// against the shipped binary means a header change that breaks the envelope
// cannot land with green tests, and it needs no C toolchain to run.
func TestShippedWasmExample(t *testing.T) {
	dir := stageExample(t, "sha256", "sha256.wasm", "sha256.toml", "sha256.schema.json")
	h := openHost(t, dir, nil)
	for _, s := range h.Status() {
		if !s.Loaded {
			t.Fatalf("shipped example did not load: %s: %s", s.Name, s.Err)
		}
	}

	t.Run("digests", func(t *testing.T) {
		// Independently checkable: `printf '<in>' | shasum -a 256`.
		for in, want := range map[string]string{
			"":           "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			"hello nine": "50ce1f9527a47956e94d826d924578d9717c755b14a300ff85a517884d52d035",
			"The quick brown fox jumps over the lazy dog": "d7a8fbb307d7809469ca9abcb0082e4f8d5651e46d3cdb762d02d0bf37c9e592",
		} {
			args, err := json.Marshal(map[string]string{"text": in})
			if err != nil {
				t.Fatal(err)
			}
			got, err := h.Call(context.Background(), "sha256", args)
			if err != nil {
				t.Fatalf("sha256(%q): %v", in, err)
			}
			if got != want {
				t.Errorf("sha256(%q) = %s, want %s", in, got, want)
			}
		}
	})

	// The escapes go through nine_arg_str, and getting them wrong produces a
	// plausible-looking wrong digest rather than an error — so this is checked
	// against the real thing rather than assumed.
	t.Run("escaped input", func(t *testing.T) {
		got, err := h.Call(context.Background(), "sha256", json.RawMessage(`{"text":"a\"b\nc"}`))
		if err != nil {
			t.Fatal(err)
		}
		const want = "92d80d9fbccd9c8c8f7e2cadaea972af1ebdefe71bd557129d663a90d5b67ab4"
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	})

	// This is the wasm half of structured errors: proof that nine_fail_code emits
	// an envelope the host parses, from a real compiled module.
	t.Run("structured failure from C", func(t *testing.T) {
		_, err := h.Call(context.Background(), "sha256", json.RawMessage(`{"nope":1}`))
		if err == nil {
			t.Fatal("expected a failure for a missing argument")
		}
		var ce *CallError
		if !errors.As(err, &ce) {
			t.Fatalf("error is %T, not *CallError", err)
		}
		if ce.Code() != "E_ARGS" {
			t.Errorf("Code() = %q, want E_ARGS", ce.Code())
		}
		retry, stated := ce.Retryable()
		if retry || !stated {
			t.Errorf("Retryable() = (%v, %v), want (false, true) — a bad argument is not retryable",
				retry, stated)
		}
	})
}

// The js example ships beside it and must keep working too.
func TestShippedJSExample(t *testing.T) {
	dir := stageExample(t, "csvstats", "csvstats.js", "csvstats.toml", "csvstats.schema.json")
	h := openHost(t, dir, nil)
	for _, s := range h.Status() {
		if !s.Loaded {
			t.Fatalf("shipped example did not load: %s: %s", s.Name, s.Err)
		}
	}
	out, err := h.Call(context.Background(), "csv_stats", json.RawMessage(`{"csv":"a,b\n1,2\n3,4"}`))
	if err != nil {
		t.Fatal(err)
	}
	if out == "" {
		t.Error("csv_stats returned nothing")
	}
	t.Logf("csv_stats => %s", out)
}
