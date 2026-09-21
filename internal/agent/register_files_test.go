package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/memory"
)

// dispatchTool is a small helper: dispatch toolName with the given JSON args.
func dispatchTool(t *testing.T, d *agent.Dispatcher, toolName, args string) (agent.CallResult, error) {
	t.Helper()
	return d.Dispatch(context.Background(), toolName, json.RawMessage(args))
}

// fileToolDispatcher carries the core file tools over a store.
//
// The store is seeded directly rather than through a tool. It used to be seeded
// with file_store, which no longer exists: the agent writes files to the
// workspace now, and the `files` table holds only what the daemon puts there —
// spilled tool output and job results (adr/file-namespaces.md).
func fileToolDispatcher(t *testing.T) (*agent.Dispatcher, *memory.Store) {
	t.Helper()
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterMemoryTools(d, store, nil, nil, false)
	return d, store
}

func seedStored(t *testing.T, store *memory.Store, path, content string) {
	t.Helper()
	if err := store.FileStore(path, content); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

// Searching inside one spilled output rather than across every one of them:
// how an agent finds the relevant region of a large truncated result.
func TestFileSearchTextScopedByPath(t *testing.T) {
	d, store := fileToolDispatcher(t)
	seedStored(t, store, "spill/agent-1/a.txt", "a rare pangolin appeared")
	seedStored(t, store, "spill/agent-2/b.txt", "another rare pangolin")

	res, err := dispatchTool(t, d, "file_search_text", `{"query":"pangolin","path":"spill/agent-1/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "spill/agent-1/a.txt") || strings.Contains(res.Output, "spill/agent-2/b.txt") {
		t.Errorf("scoped search = %s, want only the agent-1 hit", res.Output)
	}
}

// A path absent from the store must teach the model which namespace it is in.
// Nine has two — the workspace and the memory file store — and a live model
// (llama3.1:8b) that searched the wrong one got a bare result and invented an
// answer rather than correcting itself.
func TestMissingStorePathExplainsNamespace(t *testing.T) {
	store := newTestStore(t)
	seedStored(t, store, "spill/agent-1/tool-aa.txt", "payload")

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
	d, store := fileToolDispatcher(t)
	seedStored(t, store, "spill/agent-1/a.txt", "nothing relevant here")

	// A filesystem-looking path filter: the usual namespace mix-up.
	res, err := dispatchTool(t, d, "file_search_text", `{"query":"pangolin","path":"/work/audit.txt"}`)
	if err != nil {
		t.Fatalf("a no-hit search must not fail: %v", err)
	}
	if res.Output == "null" || res.Output == "" {
		t.Fatalf("empty search returned %q; it must explain why there were no hits", res.Output)
	}
	if !strings.Contains(res.Output, "MEMORY FILE STORE") {
		t.Errorf("output %q should name the namespace for a filesystem-looking path", res.Output)
	}

	// A store path that exists but has no match says so, without the lecture.
	res, err = dispatchTool(t, d, "file_search_text", `{"query":"pangolin","path":"spill/agent-1/"}`)
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
