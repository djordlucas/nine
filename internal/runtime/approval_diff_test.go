package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/toolvm"
)

// approvalHost loads the shipped tools over a temporary workspace, which is
// what approvalDiff needs to run a preview.
func approvalHost(t *testing.T) (*toolvm.Host, string, context.Context) {
	t.Helper()
	ctx := context.Background()
	ws := t.TempDir()
	h, err := toolvm.Open(ctx, toolvm.Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open toolvm: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	h.SetShippedWorkspace(toolvm.ShippedWorkspace{Host: ws})
	h.LoadShipped(ctx, nil)
	return h, ws, ctx
}

// The question a gate asks is "should this change happen", and a path does not
// answer it. The prompt carries the diff the tool itself would produce.
func TestApprovalDiffShowsTheChange(t *testing.T) {
	h, ws, ctx := approvalHost(t)
	const original = "timeout: 30\nregion: eu\n"
	if err := os.WriteFile(filepath.Join(ws, "conf.yaml"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	args := json.RawMessage(`{"path":"conf.yaml","old_text":"timeout: 30","new_text":"timeout: 90"}`)
	diff := approvalDiff(ctx, h, "edit_file", args)

	if !strings.Contains(diff, "-timeout: 30") || !strings.Contains(diff, "+timeout: 90") {
		t.Errorf("diff = %q, want both sides of the change", diff)
	}
	if !strings.Contains(diff, "+1 −1 line(s)") {
		t.Errorf("diff = %q, want a line-count summary", diff)
	}

	// Asking what a change is must not be the change happening.
	if body, _ := os.ReadFile(filepath.Join(ws, "conf.yaml")); string(body) != original {
		t.Errorf("building the approval prompt wrote to the file: %q", body)
	}

	// And the prompt the human sees carries it.
	q := approvalQuestionWithDiff("edit_file", args, diff)
	if !strings.Contains(q, "Path: conf.yaml") || !strings.Contains(q, "+timeout: 90") {
		t.Errorf("question = %q", q)
	}
}

// write_file's prompt distinguishes a rewrite from a new file, which is the
// difference between losing work and not.
func TestApprovalDiffForWriteFile(t *testing.T) {
	h, ws, ctx := approvalHost(t)
	if err := os.WriteFile(filepath.Join(ws, "notes.md"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	diff := approvalDiff(ctx, h, "write_file",
		json.RawMessage(`{"path":"notes.md","content":"one\nrewritten\n"}`))
	if !strings.Contains(diff, "-two") || !strings.Contains(diff, "+rewritten") {
		t.Errorf("diff = %q", diff)
	}

	fresh := approvalDiff(ctx, h, "write_file",
		json.RawMessage(`{"path":"brand-new.md","content":"first\n"}`))
	if !strings.Contains(fresh, "+first") {
		t.Errorf("new-file diff = %q, want the added content", fresh)
	}
}

// A prompt a person has to scroll is one they stop reading, and a gate nobody
// reads is worse than no gate.
func TestApprovalDiffIsBounded(t *testing.T) {
	h, ws, ctx := approvalHost(t)
	var b strings.Builder
	for i := 0; i < 500; i++ {
		b.WriteString("original line\n")
	}
	if err := os.WriteFile(filepath.Join(ws, "big.txt"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	var replacement strings.Builder
	for i := 0; i < 500; i++ {
		replacement.WriteString("replacement line\n")
	}
	args, _ := json.Marshal(map[string]string{"path": "big.txt", "content": replacement.String()})

	diff := approvalDiff(ctx, h, "write_file", args)
	if lines := strings.Count(diff, "\n") + 1; lines > maxApprovalDiffLines+2 {
		t.Errorf("prompt diff is %d lines, want it clipped near %d", lines, maxApprovalDiffLines)
	}
	if !strings.Contains(diff, "shortened") {
		t.Errorf("diff = %q, want it to say it was shortened", diff)
	}
}

// Tools whose arguments are already self-explanatory keep the existing prompt,
// and a host that cannot preview must not break the gate.
func TestApprovalDiffAbsentCases(t *testing.T) {
	h, _, ctx := approvalHost(t)

	if got := approvalDiff(ctx, h, "shell", json.RawMessage(`{"command":"ls"}`)); got != "" {
		t.Errorf("shell prompt got a diff: %q", got)
	}
	if got := approvalDiff(ctx, nil, "edit_file", json.RawMessage(`{"path":"a"}`)); got != "" {
		t.Errorf("a nil host returned %q, want no diff and no panic", got)
	}
	// A preview that fails leaves the human to decide with less, rather than
	// failing the call outright.
	if got := approvalDiff(ctx, h, "edit_file",
		json.RawMessage(`{"path":"missing.txt","old_text":"a","new_text":"b"}`)); got != "" {
		t.Errorf("failed preview returned %q, want empty", got)
	}
}
