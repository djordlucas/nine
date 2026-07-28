package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
)

// dispatchTool is a small helper: dispatch toolName with the given JSON args.
func dispatchTool(t *testing.T, d *agent.Dispatcher, toolName, args string) (agent.CallResult, error) {
	t.Helper()
	return d.Dispatch(context.Background(), toolName, json.RawMessage(args))
}

func fileToolDispatcher(t *testing.T) *agent.Dispatcher {
	t.Helper()
	d := agent.New()
	agent.RegisterMemoryTools(d, newTestStore(t), nil, nil, false)
	return d
}

// The spill namespace is daemon-owned: a spilled output must always be exactly
// what a tool returned, never something the model composed there.
func TestFileStoreRefusesSpillPrefix(t *testing.T) {
	d := fileToolDispatcher(t)

	_, err := dispatchTool(t, d, "file_store",
		`{"path":"spill/agent-1/forged.txt","content":"injected"}`)
	if err == nil {
		t.Fatal("expected file_store to refuse a write under the reserved spill prefix")
	}
	if !strings.Contains(err.Error(), "reserved") {
		t.Errorf("error %q should explain that the prefix is reserved", err)
	}

	// A path that merely starts with the same letters is fine.
	if _, err := dispatchTool(t, d, "file_store",
		`{"path":"spillover/notes.txt","content":"fine"}`); err != nil {
		t.Errorf("a non-spill path was wrongly refused: %v", err)
	}
}

func TestFileFetchWindowed(t *testing.T) {
	d := fileToolDispatcher(t)
	if _, err := dispatchTool(t, d, "file_store",
		`{"path":"notes/big.txt","content":"0123456789abcdefghij"}`); err != nil {
		t.Fatal(err)
	}

	// A plain fetch stays a plain string.
	res, err := dispatchTool(t, d, "file_fetch", `{"path":"notes/big.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output != "0123456789abcdefghij" {
		t.Errorf("whole-file fetch = %q, want the raw content", res.Output)
	}

	// A windowed fetch reports its position so the model can page on.
	res, err = dispatchTool(t, d, "file_fetch", `{"path":"notes/big.txt","offset":10,"limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	var slice struct {
		Content string `json:"content"`
		Offset  int    `json:"offset"`
		Chars   int    `json:"chars"`
		Total   int    `json:"total"`
	}
	if err := json.Unmarshal([]byte(res.Output), &slice); err != nil {
		t.Fatalf("windowed fetch did not return a slice object: %v (%s)", err, res.Output)
	}
	if slice.Content != "abcde" || slice.Offset != 10 || slice.Chars != 5 || slice.Total != 20 {
		t.Errorf("slice = %+v, want {abcde 10 5 20}", slice)
	}
}

func TestFileFetchMissingPath(t *testing.T) {
	d := fileToolDispatcher(t)
	if _, err := dispatchTool(t, d, "file_fetch", `{"path":"nope.txt","offset":0,"limit":5}`); err == nil {
		t.Error("expected an error for a missing path")
	}
	if _, err := dispatchTool(t, d, "file_fetch", `{"path":"nope.txt"}`); err == nil {
		t.Error("expected an error for a missing path on a whole-file fetch")
	}
}

func TestFileSearchTextScopedByPath(t *testing.T) {
	d := fileToolDispatcher(t)
	for _, f := range []string{
		`{"path":"notes/a.md","content":"a rare pangolin appeared"}`,
		`{"path":"other/b.md","content":"another rare pangolin"}`,
	} {
		if _, err := dispatchTool(t, d, "file_store", f); err != nil {
			t.Fatal(err)
		}
	}

	res, err := dispatchTool(t, d, "file_search_text", `{"query":"pangolin","path":"notes/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "notes/a.md") || strings.Contains(res.Output, "other/b.md") {
		t.Errorf("scoped search = %s, want only the notes/ hit", res.Output)
	}
}

// file_store's content_ref lets the model copy a large stored file to a new
// path without the bytes passing through its context.
func TestFileStoreContentRef(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterMemoryTools(d, store, nil, nil, false)
	d.SetRefResolver(func(_ context.Context, path string) (string, error) {
		content, found, err := store.FileFetch(path)
		if err != nil {
			return "", err
		}
		if !found {
			return "", errNotFound
		}
		return content, nil
	})

	big := strings.Repeat("LARGE-", 5000)
	if err := store.FileStore("spill/agent-1/tool-aa.txt", big); err != nil {
		t.Fatal(err)
	}

	// The model names a path; the handler receives the content.
	if _, err := dispatchTool(t, d, "file_store",
		`{"path":"runbooks/kept.txt","content_ref":"spill/agent-1/tool-aa.txt"}`); err != nil {
		t.Fatalf("file_store with content_ref: %v", err)
	}
	got, found, err := store.FileFetch("runbooks/kept.txt")
	if err != nil || !found {
		t.Fatalf("copy not stored: err=%v found=%v", err, found)
	}
	if got != big {
		t.Errorf("copied %d chars, want the full %d", len(got), len(big))
	}

	// An explicit content argument still wins over content_ref.
	if _, err := dispatchTool(t, d, "file_store",
		`{"path":"runbooks/explicit.txt","content":"literal","content_ref":"spill/agent-1/tool-aa.txt"}`); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.FileFetch("runbooks/explicit.txt"); got != "literal" {
		t.Errorf("content = %q, want the literal content to take precedence", got)
	}
}

var errNotFound = errString("no stored file")

type errString string

func (e errString) Error() string { return string(e) }

// A path absent from the store must teach the model which namespace it is in.
// Nine has two — the workspace filesystem and the memory file store — and a
// live model (llama3.1:8b) that searched the wrong one got a bare result and
// invented an answer rather than correcting itself.
func TestMissingStorePathExplainsNamespace(t *testing.T) {
	store := newTestStore(t)
	if err := store.FileStore("spill/agent-1/tool-aa.txt", "payload"); err != nil {
		t.Fatal(err)
	}

	msg := agent.MissingStorePathError(store, "/work/audit.txt").Error()
	for _, want := range []string{"/work/audit.txt", "MEMORY FILE STORE", "read_file"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if !strings.Contains(msg, "spill/agent-1/tool-aa.txt") {
		t.Errorf("error %q should list the stored paths so the model can correct itself", msg)
	}

	// A store-shaped path that is simply absent gets no filesystem lecture.
	msg = agent.MissingStorePathError(store, "spill/agent-1/missing.txt").Error()
	if strings.Contains(msg, "workspace filesystem path") {
		t.Errorf("error %q should not lecture about filesystems for a store-shaped path", msg)
	}
}

// file_search_text returned a bare "null" for no hits, which a live model
// answered by hallucinating a value. It must explain instead.
func TestFileSearchTextNoHitsExplains(t *testing.T) {
	d := fileToolDispatcher(t)
	if _, err := dispatchTool(t, d, "file_store",
		`{"path":"notes/a.md","content":"nothing relevant here"}`); err != nil {
		t.Fatal(err)
	}

	// A filesystem-looking path filter: the usual namespace mix-up.
	res, err := dispatchTool(t, d, "file_search_text", `{"query":"pangolin","path":"/work/audit.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output == "null" || res.Output == "" {
		t.Fatalf("empty search returned %q; it must explain why there were no hits", res.Output)
	}
	if !strings.Contains(res.Output, "MEMORY FILE STORE") {
		t.Errorf("output %q should name the namespace for a filesystem-looking path", res.Output)
	}

	// A store path that exists but has no match says so, without the lecture.
	res, err = dispatchTool(t, d, "file_search_text", `{"query":"pangolin","path":"notes/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Output, "MEMORY FILE STORE") {
		t.Errorf("output %q should not lecture when the path exists", res.Output)
	}
	if res.Output == "null" {
		t.Error("a genuine no-match must still explain itself")
	}

	// An unscoped miss explains too.
	res, err = dispatchTool(t, d, "file_search_text", `{"query":"pangolin"}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.Output == "null" {
		t.Error("an unscoped no-match must explain itself, not return null")
	}
}
