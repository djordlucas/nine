package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A piped turn writes new files only: it may create one and write to it again,
// but not write or edit a file that was there before the turn.
func TestPipedWriteGuard(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes/keep.md"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := pipedWriteGuard(root)
	call := func(name, path string) error {
		args, _ := json.Marshal(map[string]string{"path": path})
		return guard(name, args)
	}

	for _, tc := range []struct{ name, path string }{
		{"write_file", "notes/keep.md"},
		{"edit_file", "notes/keep.md"},
		{"write_file", "/work/notes/keep.md"},
		{"edit_file", "notes/../notes/keep.md"},
	} {
		if err := call(tc.name, tc.path); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Errorf("%s %s = %v, want a refusal", tc.name, tc.path, err)
		}
	}
	if err := call("write_file", "notes/incident.md"); err != nil {
		t.Errorf("creating a new file was refused: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "notes/incident.md"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"write_file", "edit_file"} {
		if err := call(name, "/work/notes/incident.md"); err != nil {
			t.Errorf("%s on a file the turn created was refused: %v", name, err)
		}
	}
	if err := call("read_file", "notes/keep.md"); err != nil {
		t.Errorf("a read was refused: %v", err)
	}
	if err := pipedWriteGuard("")("write_file", json.RawMessage(`{"path":"new.md"}`)); err == nil {
		t.Error("with no workspace root a write was allowed")
	}
}
