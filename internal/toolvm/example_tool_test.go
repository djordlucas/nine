package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// The shipped examples are what a new author copies, so they are tested as
// shipped rather than as a paraphrase of themselves.
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

// link_check is the worked example: it declares capabilities, so it also proves
// the shipped manifest and the shipped code agree about what it needs.
func TestShippedLinkCheckExample(t *testing.T) {
	dir := stageExample(t, "linkcheck", "linkcheck.js", "linkcheck.toml", "linkcheck.schema.json")

	t.Run("loads with its declared grants", func(t *testing.T) {
		h := openHost(t, dir, map[string]Grant{
			"link_check": {
				FSRead: []Mount{{Host: t.TempDir(), Guest: "/data"}},
				HTTP:   &HTTPGrant{AllowHosts: []string{"example.com"}, Methods: []string{"GET"}},
			},
		})
		for _, s := range h.Status() {
			if !s.Loaded {
				t.Fatalf("shipped example did not load: %s", s.Err)
			}
		}
	})

	// The asymmetry the capability model exists for: declaring without a grant is
	// a load failure, not a tool that half-works.
	t.Run("is skipped when the grants are missing", func(t *testing.T) {
		h := openHost(t, dir, nil)
		for _, s := range h.Status() {
			if s.Loaded {
				t.Error("loaded with none of its declared capabilities granted")
			}
			if !strings.Contains(s.Err, "fs.read") && !strings.Contains(s.Err, "net.http") {
				t.Errorf("skip reason names neither capability: %s", s.Err)
			}
		}
	})

	// A model reads "/data" in the description and passes "data/urls.txt" as
	// often as "urls.txt" — observed on a live turn, which is why the example
	// normalizes rather than lecturing. Pinned here because a worked example
	// getting this wrong teaches everyone who copies it.
	t.Run("tolerates the prefixes a model actually sends", func(t *testing.T) {
		mount := t.TempDir()
		if err := os.WriteFile(filepath.Join(mount, "urls.txt"), []byte("# none\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		h := openHost(t, dir, map[string]Grant{
			"link_check": {
				FSRead: []Mount{{Host: mount, Guest: "/data"}},
				HTTP:   &HTTPGrant{AllowHosts: []string{"example.com"}, Methods: []string{"GET"}},
			},
		})
		for _, name := range []string{"urls.txt", "data/urls.txt", "/data/urls.txt"} {
			args, _ := json.Marshal(map[string]string{"file": name})
			_, err := h.Call(context.Background(), "link_check", json.RawMessage(args))
			// The file exists but holds no URLs, so E_EMPTY means it was FOUND.
			var ce *CallError
			if !errors.As(err, &ce) || ce.Code() != "E_EMPTY" {
				t.Errorf("file=%q: got %v, want the file to be found", name, err)
			}
		}
		// …and traversal is still named rather than silently normalized.
		args, _ := json.Marshal(map[string]string{"file": "../etc/passwd"})
		_, err := h.Call(context.Background(), "link_check", json.RawMessage(args))
		var ce *CallError
		if !errors.As(err, &ce) || ce.Code() != "E_ARGS" {
			t.Errorf("traversal: got %v, want E_ARGS", err)
		}
	})

	// Its argument handling is the part a model gets wrong, and every failure
	// there is non-retryable — so the example should demonstrate saying so.
	t.Run("bad arguments are refused as non-retryable", func(t *testing.T) {
		h := openHost(t, dir, map[string]Grant{
			"link_check": {
				FSRead: []Mount{{Host: t.TempDir(), Guest: "/data"}},
				HTTP:   &HTTPGrant{AllowHosts: []string{"example.com"}, Methods: []string{"GET"}},
			},
		})
		for _, args := range []string{`{}`, `{"urls":[]}`, `{"file":"nope.txt"}`} {
			_, err := h.Call(context.Background(), "link_check", json.RawMessage(args))
			if err == nil {
				t.Errorf("%s was accepted", args)
				continue
			}
			var ce *CallError
			if !errors.As(err, &ce) {
				t.Errorf("%s: error is %T, not *CallError", args, err)
				continue
			}
			if retry, stated := ce.Retryable(); retry || !stated {
				t.Errorf("%s: Retryable() = (%v, %v), want (false, true)", args, retry, stated)
			}
		}
	})
}
