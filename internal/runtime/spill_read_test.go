package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/toolvm"
)

// spillReadHost is a dispatcher carrying the shipped file tools over a real
// workspace, plus a store — the two namespaces read_file now spans.
func spillReadHost(t *testing.T) (*agent.Dispatcher, *memory.Store, string) {
	t.Helper()
	ctx := context.Background()
	ws := t.TempDir()

	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	host, err := toolvm.Open(ctx, toolvm.Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open toolvm: %v", err)
	}
	t.Cleanup(func() { _ = host.Close(ctx) })
	host.SetShippedWorkspace(toolvm.ShippedWorkspace{Host: ws})
	host.LoadShipped(ctx, nil)

	d := agent.New()
	d.SyncSandboxed(host, func(string) bool { return true })
	agent.RegisterSpillReader(d, store)
	registerLargeOutput(d, store, "agent-test", ws)
	return d, store, ws
}

// One read tool over both namespaces. Reaching into the wrong one is the most
// common model error adr/tool-output-spill.md records, and it cannot happen if
// there is only one tool to reach with.
func TestReadFileServesSpillsAndWorkspace(t *testing.T) {
	d, store, ws := spillReadHost(t)

	if err := store.FileStore("spill/agent-test/fetch-abcd.txt", "pangolin-count=8321\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("ordinary file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	spill, err := d.Dispatch(context.Background(), "read_file",
		json.RawMessage(`{"path":"spill/agent-test/fetch-abcd.txt"}`))
	if err != nil {
		t.Fatalf("read_file on a spill: %v", err)
	}
	if !strings.Contains(spill.Output, "pangolin-count=8321") {
		t.Errorf("spill read = %q", spill.Output)
	}

	file, err := d.Dispatch(context.Background(), "read_file", json.RawMessage(`{"path":"notes.txt"}`))
	if err != nil {
		t.Fatalf("read_file on a workspace file: %v", err)
	}
	if !strings.Contains(file.Output, "ordinary file") {
		t.Errorf("workspace read = %q", file.Output)
	}
}

// The truncation notice tells the model to read a window; the same tool has to
// serve one, with the shape file_fetch used so nothing has to learn a second.
func TestReadFileWindowsASpill(t *testing.T) {
	d, store, _ := spillReadHost(t)
	body := strings.Repeat("A", 100) + "MIDDLE" + strings.Repeat("B", 100)
	if err := store.FileStore("spill/agent-test/big.txt", body); err != nil {
		t.Fatal(err)
	}

	res, err := d.Dispatch(context.Background(), "read_file",
		json.RawMessage(`{"path":"spill/agent-test/big.txt","offset":100,"limit":6}`))
	if err != nil {
		t.Fatal(err)
	}
	var slice struct {
		Content string `json:"content"`
		Offset  int    `json:"offset"`
		Total   int    `json:"total"`
	}
	if err := json.Unmarshal([]byte(res.Output), &slice); err != nil {
		t.Fatalf("unmarshal %q: %v", res.Output, err)
	}
	if slice.Content != "MIDDLE" {
		t.Errorf("content = %q, want the requested window", slice.Content)
	}
	if slice.Offset != 100 || slice.Total != len(body) {
		t.Errorf("offset/total = %d/%d, want 100/%d", slice.Offset, slice.Total, len(body))
	}
}

// A spill path that is not there explains the namespace rather than saying
// "no such file" about a file that plainly exists somewhere else.
func TestReadFileMissingSpillExplainsItself(t *testing.T) {
	d, store, _ := spillReadHost(t)
	if err := store.FileStore("spill/agent-test/real.txt", "content"); err != nil {
		t.Fatal(err)
	}

	_, err := d.Dispatch(context.Background(), "read_file",
		json.RawMessage(`{"path":"spill/agent-test/ghost.txt"}`))
	if err == nil {
		t.Fatal("reading a missing spill succeeded")
	}
	if !strings.Contains(err.Error(), "memory file store") {
		t.Errorf("error = %v, want it to name the namespace", err)
	}
	if !strings.Contains(err.Error(), "spill/agent-test/real.txt") {
		t.Errorf("error = %v, want it to list what is stored", err)
	}
}

// Passing a payload by reference: the model names a path and the bytes go from
// the store into a file without crossing its context.
func TestWriteFileContentRefFromSpill(t *testing.T) {
	d, store, ws := spillReadHost(t)
	payload := strings.Repeat("recorded output\n", 500)
	if err := store.FileStore("spill/agent-test/out.txt", payload); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Dispatch(context.Background(), "write_file",
		json.RawMessage(`{"path":"kept/output.txt","content_ref":"spill/agent-test/out.txt"}`)); err != nil {
		t.Fatalf("write_file with content_ref: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(ws, "kept/output.txt"))
	if err != nil {
		t.Fatalf("file was not written: %v", err)
	}
	if string(got) != payload {
		t.Errorf("written %d bytes, want the stored payload's %d", len(got), len(payload))
	}
}

// A ref may name a workspace file too: "copy this" should not work only for
// output that happened to be truncated.
func TestWriteFileContentRefFromWorkspaceFile(t *testing.T) {
	d, _, ws := spillReadHost(t)
	if err := os.WriteFile(filepath.Join(ws, "source.txt"), []byte("original body\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Dispatch(context.Background(), "write_file",
		json.RawMessage(`{"path":"copy.txt","content_ref":"source.txt"}`)); err != nil {
		t.Fatalf("write_file with a workspace ref: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(ws, "copy.txt"))
	if string(got) != "original body\n" {
		t.Errorf("copy = %q", got)
	}
}

// A ref resolves inside the workspace only. The resolver reads with the
// daemon's own authority, not through the sandbox's pre-open, so the check is
// the containment.
func TestRefResolverRefusesPathsOutsideTheWorkspace(t *testing.T) {
	ws := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("not yours\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{outside, "../" + filepath.Base(outside), "/etc/hosts"} {
		if _, err := readWorkspaceFile(ws, p); err == nil {
			t.Errorf("readWorkspaceFile(%q) succeeded, want it refused", p)
		}
	}
}
