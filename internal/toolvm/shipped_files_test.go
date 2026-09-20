package toolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// workspaceHost is a host with the shipped tier loaded over a temporary
// workspace, which is what every file tool needs to do anything at all.
func workspaceHost(t *testing.T) (*Host, string, context.Context) {
	t.Helper()
	ctx := context.Background()
	ws := t.TempDir()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(ctx) })
	h.SetShippedWorkspace(ShippedWorkspace{Host: ws})
	h.LoadShipped(ctx, nil)
	return h, ws, ctx
}

func seed(t *testing.T, ws, rel, content string) string {
	t.Helper()
	full := filepath.Join(ws, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return full
}

func mustCall(t *testing.T, h *Host, ctx context.Context, tool, args string) string {
	t.Helper()
	out, err := h.Call(ctx, tool, json.RawMessage(args))
	if err != nil {
		t.Fatalf("%s(%s): %v", tool, args, err)
	}
	return out
}

// The point of edit_file: change part of a file without the file passing
// through anyone's context, leaving every other byte as it was.
func TestShippedEditFileReplacesExactText(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "config/service.yaml", "name: billing-api\nreplicas: 3\ntimeout_seconds: 30\nregion: eu-west-1\n")

	out := mustCall(t, h, ctx, "edit_file",
		`{"path":"config/service.yaml","old_text":"timeout_seconds: 30","new_text":"timeout_seconds: 90"}`)
	if !strings.Contains(out, "replaced 1") {
		t.Errorf("result = %q, want it to report one replacement", out)
	}

	got, err := os.ReadFile(filepath.Join(ws, "config/service.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	want := "name: billing-api\nreplicas: 3\ntimeout_seconds: 90\nregion: eu-west-1\n"
	if string(got) != want {
		t.Errorf("file =\n%q\nwant\n%q", got, want)
	}
}

// An ambiguous edit must change nothing. A model that meant one occurrence and
// got three silently edited is worse off than one told the count.
func TestShippedEditFileRefusesAmbiguousMatch(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	const original = "port: 8080\nport: 8080\nport: 8080\n"
	seed(t, ws, "conf.yaml", original)

	_, err := h.Call(ctx, "edit_file",
		json.RawMessage(`{"path":"conf.yaml","old_text":"port: 8080","new_text":"port: 9090"}`))
	if err == nil {
		t.Fatal("edit_file replaced an ambiguous match instead of refusing")
	}
	if !strings.Contains(err.Error(), "expect") {
		t.Errorf("error = %v, want it to explain expect", err)
	}

	got, _ := os.ReadFile(filepath.Join(ws, "conf.yaml"))
	if string(got) != original {
		t.Errorf("file was modified by a refused edit: %q", got)
	}
}

// expect:"all" is how a model says it meant every occurrence.
func TestShippedEditFileReplacesAllWhenAsked(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "conf.yaml", "port: 8080\nport: 8080\nport: 8080\n")

	out := mustCall(t, h, ctx, "edit_file",
		`{"path":"conf.yaml","old_text":"8080","new_text":"9090","expect":"all"}`)
	if !strings.Contains(out, "replaced 3") {
		t.Errorf("result = %q, want three replacements", out)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "conf.yaml"))
	if string(got) != "port: 9090\nport: 9090\nport: 9090\n" {
		t.Errorf("file = %q", got)
	}
}

// Text that is not there must fail loudly rather than write an unchanged file.
func TestShippedEditFileReportsMissingText(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "a.txt", "alpha\n")

	_, err := h.Call(ctx, "edit_file",
		json.RawMessage(`{"path":"a.txt","old_text":"omega","new_text":"x"}`))
	if err == nil {
		t.Fatal("edit_file accepted text that does not occur")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %v, want it to say the text was not found", err)
	}
	if entries, _ := os.ReadDir(ws); len(entries) != 1 {
		t.Errorf("a failed edit left debris behind: %v", entries)
	}
}

// The reason edit_file exists. This file is larger than a call's whole memory
// cap (16 MiB), so any implementation that materialized it would fail here.
func TestShippedEditFileHandlesFileLargerThanMemory(t *testing.T) {
	h, ws, ctx := workspaceHost(t)

	line := strings.Repeat("filler filler filler filler filler filler\n", 32) // ~1.3 KiB
	var b strings.Builder
	for i := 0; i < 16_000; i++ { // ~21 MiB
		b.WriteString(line)
		if i == 9_000 {
			b.WriteString("NEEDLE marker-alpha\n")
		}
	}
	seed(t, ws, "big.log", b.String())

	before, err := os.Stat(filepath.Join(ws, "big.log"))
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() < 16<<20 {
		t.Fatalf("fixture is %d bytes, smaller than the memory cap it is meant to exceed", before.Size())
	}

	out := mustCall(t, h, ctx, "edit_file",
		`{"path":"big.log","old_text":"NEEDLE marker-alpha","new_text":"NEEDLE marker-omega"}`)
	if !strings.Contains(out, "replaced 1") {
		t.Errorf("result = %q", out)
	}

	got, err := os.ReadFile(filepath.Join(ws, "big.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "marker-alpha") || !strings.Contains(string(got), "marker-omega") {
		t.Error("the replacement did not land in the large file")
	}
	if delta := int64(len(got)) - before.Size(); delta != 0 {
		t.Errorf("file size changed by %d bytes; the edit was the same length", delta)
	}
}

// A match spanning two read windows must still be found: the window boundary is
// an implementation detail, not something a caller can see.
func TestShippedEditFileFindsMatchAcrossWindowBoundary(t *testing.T) {
	h, ws, ctx := workspaceHost(t)

	// WINDOW is 1 MiB in edit_file.js; straddle that boundary exactly.
	const window = 1 << 20
	needle := "STRADDLE-" + strings.Repeat("x", 64)
	head := strings.Repeat("a", window-len(needle)/2)
	seed(t, ws, "straddle.txt", head+needle+strings.Repeat("b", 4096))

	args := fmt.Sprintf(`{"path":"straddle.txt","old_text":%q,"new_text":"REPLACED"}`, needle)
	if out := mustCall(t, h, ctx, "edit_file", args); !strings.Contains(out, "replaced 1") {
		t.Errorf("result = %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "straddle.txt"))
	if !strings.Contains(string(got), "REPLACED") || strings.Contains(string(got), needle) {
		t.Error("a match across the window boundary was missed")
	}
}

// Moving is a relink: no read, no copy, and the contents are untouched.
func TestShippedMoveFile(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "inbox/report.md", "codeword juniper-51\n")

	mustCall(t, h, ctx, "move_file", `{"from":"inbox/report.md","to":"reports/2026/report.md"}`)

	if _, err := os.Stat(filepath.Join(ws, "inbox/report.md")); !os.IsNotExist(err) {
		t.Error("the source still exists after a move")
	}
	got, err := os.ReadFile(filepath.Join(ws, "reports/2026/report.md"))
	if err != nil {
		t.Fatalf("destination missing: %v", err)
	}
	if string(got) != "codeword juniper-51\n" {
		t.Errorf("content = %q", got)
	}
}

// A move that silently replaced a file would destroy one nobody looked at.
func TestShippedMoveFileRefusesExistingDestination(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "a.txt", "source\n")
	seed(t, ws, "b.txt", "destination\n")

	_, err := h.Call(ctx, "move_file", json.RawMessage(`{"from":"a.txt","to":"b.txt"}`))
	if err == nil {
		t.Fatal("move_file overwrote a file without being asked to")
	}
	if !strings.Contains(err.Error(), "overwrite") {
		t.Errorf("error = %v, want it to name the overwrite option", err)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "b.txt")); string(got) != "destination\n" {
		t.Errorf("destination changed: %q", got)
	}

	mustCall(t, h, ctx, "move_file", `{"from":"a.txt","to":"b.txt","overwrite":true}`)
	if got, _ := os.ReadFile(filepath.Join(ws, "b.txt")); string(got) != "source\n" {
		t.Errorf("overwrite:true did not replace the destination: %q", got)
	}
}

// Copying streams host-side; the bytes never enter the model's context.
func TestShippedCopyFile(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	body := strings.Repeat("payload\n", 200_000) // ~1.6 MiB, several stream chunks
	seed(t, ws, "src.txt", body)

	out := mustCall(t, h, ctx, "copy_file", `{"from":"src.txt","to":"backup/src.txt"}`)
	if !strings.Contains(out, "copied") {
		t.Errorf("result = %q", out)
	}
	got, err := os.ReadFile(filepath.Join(ws, "backup/src.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("copy differs from the source (%d vs %d bytes)", len(got), len(body))
	}
	if src, _ := os.ReadFile(filepath.Join(ws, "src.txt")); string(src) != body {
		t.Error("the source was modified by a copy")
	}
}

// A copy over a shorter file must not leave the old tail behind.
func TestShippedCopyFileOverwriteTruncates(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "short.txt", "short\n")
	seed(t, ws, "long.txt", strings.Repeat("long\n", 1000))

	mustCall(t, h, ctx, "copy_file", `{"from":"short.txt","to":"long.txt","overwrite":true}`)
	if got, _ := os.ReadFile(filepath.Join(ws, "long.txt")); string(got) != "short\n" {
		t.Errorf("destination = %q, want the source's contents alone", got)
	}
}

// Line ranges are the unit a model thinks in, and the unit edit_file reports.
func TestShippedReadFileLineRange(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	var b strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	seed(t, ws, "numbered.txt", b.String())

	out := mustCall(t, h, ctx, "read_file", `{"path":"numbered.txt","lines":"120-122"}`)
	var got struct {
		Content   string `json:"content"`
		FirstLine int    `json:"first_line"`
		Lines     int    `json:"lines"`
		Version   string `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}
	if got.Content != "line 120\nline 121\nline 122" {
		t.Errorf("content = %q", got.Content)
	}
	if got.FirstLine != 120 || got.Lines != 3 {
		t.Errorf("first_line/lines = %d/%d, want 120/3", got.FirstLine, got.Lines)
	}
	if got.Version == "" {
		t.Error("no version token was returned")
	}

	numbered := mustCall(t, h, ctx, "read_file", `{"path":"numbered.txt","lines":"7","line_numbers":true}`)
	if !strings.Contains(numbered, `7\tline 7`) {
		t.Errorf("numbered read = %q, want the line prefixed with its number", numbered)
	}
}

// A whole-file read keeps returning a bare string: the common call did not
// change shape when windows were added.
func TestShippedReadFileWholeFileStaysAString(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "a.txt", "hello\n")

	if out := mustCall(t, h, ctx, "read_file", `{"path":"a.txt"}`); out != "hello\n" {
		t.Errorf("read_file = %q, want the bare contents", out)
	}
}

// Binary content decoded as text produces replacement characters a model cannot
// distinguish from real ones, so it is refused instead.
func TestShippedReadFileRefusesBinary(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	if err := os.WriteFile(filepath.Join(ws, "image.png"),
		[]byte{0x89, 'P', 'N', 'G', 0x00, 0x1a, 0x0a, 0xff, 0xfe}, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := h.Call(ctx, "read_file", json.RawMessage(`{"path":"image.png"}`))
	if err == nil {
		t.Fatal("read_file returned binary content as text")
	}
	if !strings.Contains(err.Error(), "not a text file") {
		t.Errorf("error = %v, want it to say the file is not text", err)
	}
}

// Appending must not read the file back: that is the whole reason it exists.
func TestShippedWriteFileAppendMode(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "log.txt", "first\n")

	mustCall(t, h, ctx, "write_file", `{"path":"log.txt","content":"second\n","mode":"append"}`)
	mustCall(t, h, ctx, "write_file", `{"path":"log.txt","content":"third\n","mode":"append"}`)

	got, _ := os.ReadFile(filepath.Join(ws, "log.txt"))
	if string(got) != "first\nsecond\nthird\n" {
		t.Errorf("file = %q", got)
	}

	// Append also creates a file that is not there yet.
	mustCall(t, h, ctx, "write_file", `{"path":"new.txt","content":"x\n","mode":"append"}`)
	if got, _ := os.ReadFile(filepath.Join(ws, "new.txt")); string(got) != "x\n" {
		t.Errorf("append did not create the file: %q", got)
	}
}

// Two writers, one file: the second must be told rather than silently winning.
func TestShippedWriteFileIfUnchanged(t *testing.T) {
	h, ws, ctx := workspaceHost(t)
	seed(t, ws, "shared.txt", "original\n")

	out := mustCall(t, h, ctx, "read_file", `{"path":"shared.txt","offset":0,"limit":4}`)
	var read struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal([]byte(out), &read); err != nil {
		t.Fatalf("unmarshal %q: %v", out, err)
	}

	// Someone else writes in between.
	seed(t, ws, "shared.txt", "changed by another agent\n")

	_, err := h.Call(ctx, "write_file", json.RawMessage(
		fmt.Sprintf(`{"path":"shared.txt","content":"mine\n","if_unchanged":%q}`, read.Version)))
	if err == nil {
		t.Fatal("write_file overwrote a file that had changed since it was read")
	}
	if !strings.Contains(err.Error(), "changed since you read it") {
		t.Errorf("error = %v, want it to name the conflict", err)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "shared.txt")); string(got) != "changed by another agent\n" {
		t.Errorf("the other agent's write was lost: %q", got)
	}
}
