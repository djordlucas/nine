package toolvm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// trashedFiles lists every file under the workspace trash, as paths relative to
// the trash root, so a test can assert what was kept without knowing the entry
// name (which carries a timestamp and four random bytes).
func trashedFiles(t *testing.T, ws string) []string {
	t.Helper()
	root := filepath.Join(ws, ".nine", "trash")
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil //nolint:nilerr // an absent trash is an empty trash
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("walk trash: %v", err)
	}
	return out
}

// Deleting moves the file out of the workspace and into the trash, rather than
// destroying it: the session doing the deleting is frequently one nobody is
// watching.
func TestShippedDeleteFileTrashesRatherThanDestroys(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "notes/pricing.md", "enterprise tier: 4200 per seat\n")

	out := mustCall(t, h, ctx, "delete_file", `{"path":"notes/pricing.md"}`)
	if !strings.Contains(out, "recoverable") {
		t.Errorf("result = %q, want it to say the file can be recovered", out)
	}

	if _, err := os.Stat(filepath.Join(ws, "notes/pricing.md")); !os.IsNotExist(err) {
		t.Error("the file is still in the workspace after a delete")
	}

	got := trashedFiles(t, ws)
	if len(got) != 1 || !strings.HasSuffix(got[0], filepath.Join("notes", "pricing.md")) {
		t.Fatalf("trash holds %v, want one entry keeping the original path", got)
	}
	// The bytes have to actually be there; a trash that loses content is worse
	// than no trash, because it reads as recoverable.
	body, err := os.ReadFile(filepath.Join(ws, ".nine", "trash", got[0]))
	if err != nil || string(body) != "enterprise tier: 4200 per seat\n" {
		t.Errorf("trashed content = %q, %v", body, err)
	}
}

// The round trip the trash exists for: delete, find it, put it back.
func TestShippedRestoreFileRoundTrip(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "notes/pricing.md", "enterprise tier: 4200 per seat\n")
	mustCall(t, h, ctx, "delete_file", `{"path":"notes/pricing.md"}`)

	listed := mustCall(t, h, ctx, "trash_list", `{"path":"pricing"}`)
	var list struct {
		Entries []struct {
			Entry string `json:"entry"`
			Files []struct {
				OriginalPath string `json:"original_path"`
			} `json:"files"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(listed), &list); err != nil {
		t.Fatalf("unmarshal %q: %v", listed, err)
	}
	if len(list.Entries) != 1 {
		t.Fatalf("trash_list returned %d entries, want 1: %s", len(list.Entries), listed)
	}
	if got := list.Entries[0].Files[0].OriginalPath; got != "/work/notes/pricing.md" {
		t.Errorf("original_path = %q", got)
	}

	mustCall(t, h, ctx, "restore_file",
		`{"entry":"`+list.Entries[0].Entry+`"}`)

	body, err := os.ReadFile(filepath.Join(ws, "notes/pricing.md"))
	if err != nil {
		t.Fatalf("file was not restored: %v", err)
	}
	if string(body) != "enterprise tier: 4200 per seat\n" {
		t.Errorf("restored content = %q", body)
	}
}

// Recovering one file by destroying another is not a recovery.
func TestShippedRestoreFileRefusesToClobber(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "a.txt", "original\n")
	mustCall(t, h, ctx, "delete_file", `{"path":"a.txt"}`)
	seed(t, ws, "a.txt", "something newer\n")

	listed := mustCall(t, h, ctx, "trash_list", `{}`)
	var list struct {
		Entries []struct {
			Entry string `json:"entry"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(listed), &list); err != nil {
		t.Fatal(err)
	}

	_, err := h.Call(ctx, "restore_file", json.RawMessage(`{"entry":"`+list.Entries[0].Entry+`"}`))
	if err == nil {
		t.Fatal("restore_file overwrote a file that was in the way")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %v", err)
	}
	if body, _ := os.ReadFile(filepath.Join(ws, "a.txt")); string(body) != "something newer\n" {
		t.Errorf("the newer file was destroyed: %q", body)
	}

	// With a free destination it succeeds, so the refusal is about the clobber
	// and not about the entry.
	mustCall(t, h, ctx, "restore_file",
		`{"entry":"`+list.Entries[0].Entry+`","to":"recovered.txt"}`)
	if body, _ := os.ReadFile(filepath.Join(ws, "recovered.txt")); string(body) != "original\n" {
		t.Errorf("restored to = %q", body)
	}
}

// Overwriting destroys contents as thoroughly as deleting does, so it goes to
// the trash too.
func TestShippedWriteFileTrashesPreviousVersion(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "report.md", "first draft\n")

	out := mustCall(t, h, ctx, "write_file", `{"path":"report.md","content":"second draft\n"}`)
	if !strings.Contains(out, "trash") {
		t.Errorf("result = %q, want it to mention the previous version", out)
	}

	got := trashedFiles(t, ws)
	if len(got) != 1 {
		t.Fatalf("trash holds %v, want the previous version", got)
	}
	body, _ := os.ReadFile(filepath.Join(ws, ".nine", "trash", got[0]))
	if string(body) != "first draft\n" {
		t.Errorf("trashed content = %q, want the first draft", body)
	}
	if cur, _ := os.ReadFile(filepath.Join(ws, "report.md")); string(cur) != "second draft\n" {
		t.Errorf("current content = %q", cur)
	}
}

// Rewriting a file with what it already holds is common — a tool that reads,
// changes nothing, and writes back. A copy per rewrite would fill the trash with
// duplicates of a file that never changed.
func TestShippedWriteFileIdenticalContentIsNotTrashed(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "stable.txt", "unchanged\n")

	mustCall(t, h, ctx, "write_file", `{"path":"stable.txt","content":"unchanged\n"}`)
	if got := trashedFiles(t, ws); len(got) != 0 {
		t.Errorf("trash holds %v after a no-op rewrite, want nothing", got)
	}
}

// A new file has no previous version to keep.
func TestShippedWriteFileNewFileTrashesNothing(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	mustCall(t, h, ctx, "write_file", `{"path":"fresh.txt","content":"new\n"}`)
	if got := trashedFiles(t, ws); len(got) != 0 {
		t.Errorf("trash holds %v after creating a file, want nothing", got)
	}
}

// An edit keeps the version before it, which is what makes an edit reviewable
// after the fact and recoverable when it was wrong.
func TestShippedEditFileTrashesPreviousVersion(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "conf.yaml", "timeout: 30\nregion: eu\n")

	mustCall(t, h, ctx, "edit_file",
		`{"path":"conf.yaml","old_text":"timeout: 30","new_text":"timeout: 90"}`)

	got := trashedFiles(t, ws)
	if len(got) != 1 {
		t.Fatalf("trash holds %v, want the pre-edit version", got)
	}
	body, _ := os.ReadFile(filepath.Join(ws, ".nine", "trash", got[0]))
	if string(body) != "timeout: 30\nregion: eu\n" {
		t.Errorf("trashed content = %q, want the file as it was before the edit", body)
	}
	if cur, _ := os.ReadFile(filepath.Join(ws, "conf.yaml")); string(cur) != "timeout: 90\nregion: eu\n" {
		t.Errorf("current content = %q", cur)
	}
}

// .nine/ is Nine's own bookkeeping. The write tools refuse it, so an agent
// cannot rewrite the record of what it deleted; trash_list and restore_file are
// the only way in.
func TestShippedFileToolsRefuseTheStateDirectory(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "a.txt", "x\n")
	mustCall(t, h, ctx, "delete_file", `{"path":"a.txt"}`)

	for _, tc := range []struct{ tool, args string }{
		{"write_file", `{"path":".nine/trash/forged.txt","content":"not a real deletion"}`},
		{"delete_file", `{"path":".nine/trash"}`},
		{"edit_file", `{"path":".nine/.gitignore","old_text":"*","new_text":""}`},
	} {
		if _, err := h.Call(ctx, tc.tool, json.RawMessage(tc.args)); err == nil {
			t.Errorf("%s was allowed to write inside .nine/", tc.tool)
		}
	}
}

// Deleting a directory that still holds files would be a recursive delete in
// disguise.
func TestShippedDeleteFileRefusesNonEmptyDirectory(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "tree/child.txt", "x\n")

	_, err := h.Call(ctx, "delete_file", json.RawMessage(`{"path":"tree"}`))
	if err == nil {
		t.Fatal("delete_file removed a non-empty directory")
	}
	if !strings.Contains(err.Error(), "not empty") {
		t.Errorf("error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "tree/child.txt")); err != nil {
		t.Errorf("the directory's contents were disturbed: %v", err)
	}
}

// The operator's version control is not the agent's to delete.
func TestShippedDeleteFileRefusesGitDirectory(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, ".git/HEAD", "ref: refs/heads/main\n")

	if _, err := h.Call(ctx, "delete_file", json.RawMessage(`{"path":".git/HEAD"}`)); err == nil {
		t.Error("delete_file removed a file inside .git")
	}
	if _, err := os.Stat(filepath.Join(ws, ".git/HEAD")); err != nil {
		t.Errorf("the git file was removed: %v", err)
	}
}
