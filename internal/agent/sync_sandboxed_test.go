package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/toolvm"
)

// catalogHost is a SandboxedHost whose catalog a test can change between syncs.
type catalogHost struct{ names []string }

func (h *catalogHost) Tools() []*toolvm.Tool {
	out := make([]*toolvm.Tool, 0, len(h.names))
	for _, n := range h.names {
		out = append(out, &toolvm.Tool{Name: n, InputSchema: json.RawMessage(`{}`)})
	}
	return out
}

func (h *catalogHost) CallOutput(_ context.Context, name string, _ json.RawMessage) (toolvm.Output, error) {
	return toolvm.Output{Text: "wasm:" + name}, nil
}

// SyncSandboxed adds what the host gained and drops what it lost, honors the
// role's allowlist, and never replaces a handler it did not install — a core
// tool keeps its name, as it does at build.
func TestSyncSandboxedTracksTheCatalog(t *testing.T) {
	d := New()
	d.handlers["core_tool"] = func(context.Context, json.RawMessage) (string, error) { return "core", nil }
	host := &catalogHost{names: []string{"core_tool", "kept", "denied"}}
	allow := func(name string) bool { return name != "denied" }

	d.SyncSandboxed(host, allow)
	call := func(name string) string {
		t.Helper()
		res, err := d.Dispatch(context.Background(), name, json.RawMessage(`{}`))
		if err != nil {
			return "error: " + err.Error()
		}
		return res.Output
	}
	if got := call("core_tool"); got != "core" {
		t.Errorf("core_tool = %q; a sandboxed tool must not replace a core handler", got)
	}
	if got := call("kept"); got != "wasm:kept" {
		t.Errorf("kept = %q, want the sandboxed handler", got)
	}
	if d.Has("denied") {
		t.Error("a tool outside the role's allowlist was registered")
	}

	host.names = []string{"core_tool", "added"}
	d.SyncSandboxed(host, allow)
	if d.Has("kept") {
		t.Error("a tool the host dropped still has a handler")
	}
	if got := call("added"); got != "wasm:added" {
		t.Errorf("added = %q, want the new sandboxed handler", got)
	}
	if got := call("core_tool"); got != "core" {
		t.Errorf("core_tool = %q after resync; the core handler must survive", got)
	}
}

// An unknown tool name comes back with something to act on: the nearest
// registered names when one is close, and otherwise the catalog tool. A model
// that invents a wrapper — `tool_call` for a tool it just wrote — gets nothing
// to correct with from a bare "unknown tool".
func TestUnknownToolErrorIsActionable(t *testing.T) {
	d := New()
	d.handlers["char_code_sum"] = func(context.Context, json.RawMessage) (string, error) { return "", nil }
	d.handlers["tool_list"] = func(context.Context, json.RawMessage) (string, error) { return "", nil }

	_, err := d.Dispatch(context.Background(), "tool_call", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("dispatching an unregistered tool succeeded")
	}
	if !strings.Contains(err.Error(), "tool_list") {
		t.Errorf("error = %q, want it to point at tool_list", err)
	}

	// A typo is close enough to name the tool it missed.
	_, err = d.Dispatch(context.Background(), "char_code_sumx", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "Did you mean char_code_sum?") {
		t.Errorf("error = %v, want the near-miss suggestion", err)
	}
}
