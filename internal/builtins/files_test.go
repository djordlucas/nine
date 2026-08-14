package builtins_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFilesDescribe(t *testing.T) {
	p, _ := start(t, "files")
	names := make(map[string]bool)
	for _, tool := range p.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"read_file", "write_file"} {
		if !names[want] {
			t.Errorf("missing tool %q", want)
		}
	}
}

func TestWriteAndRead(t *testing.T) {
	p, m := start(t, "files")
	path := filepath.Join(t.TempDir(), "test.txt")

	args, _ := json.Marshal(map[string]string{"path": path, "content": "hello files"})
	r, err := m.Call(context.Background(), p, "write_file", args)
	if err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if r.Output != "ok" {
		t.Errorf("write_file output = %q", r.Output)
	}

	args, _ = json.Marshal(map[string]string{"path": path})
	r, err = m.Call(context.Background(), p, "read_file", args)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if r.Output != "hello files" {
		t.Errorf("read_file = %q, want %q", r.Output, "hello files")
	}
}

func TestWriteCreatesParentDirs(t *testing.T) {
	p, m := start(t, "files")
	path := filepath.Join(t.TempDir(), "a", "b", "c", "test.txt")
	args, _ := json.Marshal(map[string]string{"path": path, "content": "nested"})
	if _, err := m.Call(context.Background(), p, "write_file", args); err != nil {
		t.Fatalf("write_file nested: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file not created: %v", err)
	}
}

func TestReadMissingFile(t *testing.T) {
	p, m := start(t, "files")
	args, _ := json.Marshal(map[string]string{"path": "/nonexistent/file.txt"})
	_, err := m.Call(context.Background(), p, "read_file", args)
	if err == nil {
		t.Error("expected error reading missing file")
	}
}

// TestWorkspaceAliasRoundtrip proves that, with a workspace root configured, the
// /work alias the agent addresses files by resolves to that root for both write
// and read — the path convention the eval schema (docs/evals.md §2) relies on.
func TestWorkspaceAliasRoundtrip(t *testing.T) {
	ws := t.TempDir()
	p, m := start(t, "files", "NINE_WORKSPACE="+ws)

	args, _ := json.Marshal(map[string]string{"path": "/work/out.txt", "content": "aliased"})
	if _, err := m.Call(context.Background(), p, "write_file", args); err != nil {
		t.Fatalf("write_file /work: %v", err)
	}
	// The write landed at the real root, not a literal /work path.
	if b, err := os.ReadFile(filepath.Join(ws, "out.txt")); err != nil || string(b) != "aliased" {
		t.Fatalf("file at root = %q, err=%v; want %q", b, err, "aliased")
	}

	args, _ = json.Marshal(map[string]string{"path": "/work/out.txt"})
	r, err := m.Call(context.Background(), p, "read_file", args)
	if err != nil {
		t.Fatalf("read_file /work: %v", err)
	}
	if r.Output != "aliased" {
		t.Errorf("read_file /work = %q, want %q", r.Output, "aliased")
	}
}

// TestWriteOutsideWorkspaceRejected ensures the root confinement still holds: a
// genuine absolute path outside the workspace is refused.
func TestWriteOutsideWorkspaceRejected(t *testing.T) {
	ws := t.TempDir()
	p, m := start(t, "files", "NINE_WORKSPACE="+ws)

	args, _ := json.Marshal(map[string]string{"path": "/tmp/escape.txt", "content": "nope"})
	if _, err := m.Call(context.Background(), p, "write_file", args); err == nil {
		t.Error("expected write outside workspace root to be rejected")
	}
}
