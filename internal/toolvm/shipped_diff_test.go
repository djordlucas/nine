package toolvm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type previewResult struct {
	Preview   bool   `json:"preview"`
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	Added     int    `json:"added_lines"`
	Removed   int    `json:"removed_lines"`
	Diff      string `json:"diff"`
	Truncated bool   `json:"truncated"`
	Note      string `json:"note"`
}

func decodePreview(t *testing.T, out string) previewResult {
	t.Helper()
	var got previewResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	return got
}

// A preview shows the change and makes none of it: that is what lets an
// approval prompt show a human what they are approving before it happens.
func TestShippedEditFilePreviewWritesNothing(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	const original = "name: billing-api\nreplicas: 3\ntimeout_seconds: 30\nregion: eu-west-1\n"
	seed(t, ws, "conf.yaml", original)

	got := decodePreview(t, mustCall(t, h, ctx, "edit_file",
		`{"path":"conf.yaml","old_text":"timeout_seconds: 30","new_text":"timeout_seconds: 90","preview":true}`))

	if !got.Preview {
		t.Error("result is not marked as a preview")
	}
	if !strings.Contains(got.Diff, "-timeout_seconds: 30") || !strings.Contains(got.Diff, "+timeout_seconds: 90") {
		t.Errorf("diff = %q, want both sides of the change", got.Diff)
	}
	if got.Added != 1 || got.Removed != 1 {
		t.Errorf("added/removed = %d/%d, want 1/1", got.Added, got.Removed)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "conf.yaml")); string(body) != original {
		t.Errorf("preview modified the file: %q", body)
	}
	if entries, _ := os.ReadDir(filepath.Join(ws, ".nine", "trash")); len(entries) != 0 {
		t.Error("preview put something in the trash")
	}
}

// A preview must refuse for the same reasons the real call would, or it is not
// a preview of anything.
func TestShippedEditFilePreviewRefusesAmbiguousMatch(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "conf.yaml", "port: 8080\nport: 8080\n")

	if _, err := h.Call(ctx, "edit_file",
		json.RawMessage(`{"path":"conf.yaml","old_text":"port: 8080","new_text":"port: 9090","preview":true}`)); err == nil {
		t.Fatal("preview accepted an edit the real call would refuse")
	}
}

// The diff covers the neighbourhood of the change, not the file: a preview of
// an edit to a huge file has to stay readable and has to stay fast.
func TestShippedEditFilePreviewOnLargeFileIsScoped(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	var b strings.Builder
	for i := 0; i < 200_000; i++ {
		b.WriteString("filler line that is here only to make the file large\n")
	}
	body := b.String() + "NEEDLE alpha\n" + b.String()
	seed(t, ws, "big.log", body)

	got := decodePreview(t, mustCall(t, h, ctx, "edit_file",
		`{"path":"big.log","old_text":"NEEDLE alpha","new_text":"NEEDLE omega","preview":true}`))

	if !strings.Contains(got.Diff, "-NEEDLE alpha") || !strings.Contains(got.Diff, "+NEEDLE omega") {
		t.Errorf("diff = %q, want the change", got.Diff)
	}
	if lines := strings.Count(got.Diff, "\n"); lines > 40 {
		t.Errorf("diff is %d lines; a scoped preview should be a handful", lines)
	}
	if cur, _ := os.ReadFile(filepath.Join(ws, "big.log")); string(cur) != body {
		t.Error("preview modified the large file")
	}
}

// write_file's preview covers both cases a human cares about: replacing a file,
// and creating one that does not exist yet.
func TestShippedWriteFilePreview(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "notes.md", "one\ntwo\nthree\n")

	got := decodePreview(t, mustCall(t, h, ctx, "write_file",
		`{"path":"notes.md","content":"one\ntwo\nthree and a half\n","preview":true}`))
	if !got.Exists {
		t.Error("preview reports the file as new")
	}
	if !strings.Contains(got.Diff, "-three") || !strings.Contains(got.Diff, "+three and a half") {
		t.Errorf("diff = %q", got.Diff)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "notes.md")); string(body) != "one\ntwo\nthree\n" {
		t.Errorf("preview wrote to the file: %q", body)
	}

	fresh := decodePreview(t, mustCall(t, h, ctx, "write_file",
		`{"path":"new.md","content":"first line\n","preview":true}`))
	if fresh.Exists {
		t.Error("preview reports a new file as existing")
	}
	if fresh.Added != 1 {
		t.Errorf("added = %d, want 1", fresh.Added)
	}
	if _, err := os.Stat(filepath.Join(ws, "new.md")); !os.IsNotExist(err) {
		t.Error("preview created the file")
	}
}

// Reporting a change after the fact. The previous version is already in the
// trash, so the "before" side costs nothing to keep.
func TestShippedDiffFileAgainstPreviousVersion(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "conf.yaml", "timeout: 30\nregion: eu\n")

	mustCall(t, h, ctx, "edit_file",
		`{"path":"conf.yaml","old_text":"timeout: 30","new_text":"timeout: 90"}`)

	var got struct {
		Path    string `json:"path"`
		Against string `json:"against"`
		Added   int    `json:"added_lines"`
		Removed int    `json:"removed_lines"`
		Diff    string `json:"diff"`
	}
	out := mustCall(t, h, ctx, "diff_file", `{"path":"conf.yaml"}`)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if !strings.Contains(got.Diff, "-timeout: 30") || !strings.Contains(got.Diff, "+timeout: 90") {
		t.Errorf("diff = %q, want both sides", got.Diff)
	}
	if got.Added != 1 || got.Removed != 1 {
		t.Errorf("added/removed = %d/%d", got.Added, got.Removed)
	}
	if got.Against == "" {
		t.Error("no trash entry was named")
	}
}

// A deleted file still diffs: everything removed, which is what "show me what
// you did" should report after a delete.
func TestShippedDiffFileAfterDelete(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "gone.txt", "line one\nline two\n")
	mustCall(t, h, ctx, "delete_file", `{"path":"gone.txt"}`)

	var got struct {
		Deleted bool   `json:"deleted"`
		Removed int    `json:"removed_lines"`
		Diff    string `json:"diff"`
	}
	out := mustCall(t, h, ctx, "diff_file", `{"path":"gone.txt"}`)
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Deleted {
		t.Error("diff_file does not report the file as deleted")
	}
	if got.Removed != 2 || !strings.Contains(got.Diff, "-line one") {
		t.Errorf("removed = %d, diff = %q", got.Removed, got.Diff)
	}
}

// A file Nine never changed has no previous version, and saying so beats
// inventing an empty diff that reads as "nothing changed".
func TestShippedDiffFileWithoutPreviousVersion(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "untouched.txt", "as delivered\n")

	_, err := h.Call(ctx, "diff_file", json.RawMessage(`{"path":"untouched.txt"}`))
	if err == nil {
		t.Fatal("diff_file invented a comparison for a file it never changed")
	}
	if !strings.Contains(err.Error(), "no previous version") && !strings.Contains(err.Error(), "nothing is in the trash") {
		t.Errorf("error = %v", err)
	}
}
