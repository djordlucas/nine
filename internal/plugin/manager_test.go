package plugin_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"nine/internal/plugin"
)

// findModuleRoot walks up from the current directory to find go.mod.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

// buildBinary compiles the Go package at pkgPath (relative to module root)
// and returns the path to the resulting binary.
func buildBinary(t *testing.T, pkgPath string) string {
	t.Helper()
	root := findModuleRoot(t)
	out := filepath.Join(t.TempDir(), filepath.Base(pkgPath))
	cmd := exec.Command("go", "build", "-o", out, pkgPath)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkgPath, err, b)
	}
	return out
}

// --- native plugin tests ---
//
// Plugins are MCP servers now (R-PLUG.1): there is no hand-stamped protocol
// version to mismatch — the MCP initialize negotiation is the versioning — so
// the old rejection tests are gone; a binary that does not answer initialize is
// simply not a plugin (covered in mcp_only_test.go).

func TestManagerDescribe(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p)

	names := make([]string, 0, len(p.Tools))
	for _, tool := range p.Tools {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "echo") {
		t.Errorf("tools = %v, want echo among them", names)
	}
	for _, tool := range p.Tools {
		if tool.Name == "echo" {
			if tool.Description == "" {
				t.Error("tool description is empty")
			}
			if len(tool.InputSchema) == 0 {
				t.Error("tool input schema is empty")
			}
		}
	}
	if p.MaxConcurrent != 0 {
		t.Errorf("max_concurrent = %d, want 0 (unix-socket HTTP is concurrent)", p.MaxConcurrent)
	}
}

// The stdio transport registers the same plugin as serial (max_concurrent 1):
// stdio cannot match a response to a request without owning the stream.
func TestManagerStdioIsSerial(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.StartStdio(path)
	if err != nil {
		t.Fatalf("StartStdio: %v", err)
	}
	defer m.Stop(p)

	if p.MaxConcurrent != 1 {
		t.Errorf("max_concurrent = %d, want 1 for stdio", p.MaxConcurrent)
	}
	res, err := m.Call(context.Background(), p, "echo", json.RawMessage(`{"message":"stdio"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != `{"message":"stdio"}` {
		t.Errorf("output = %q, want the echoed args", res.Output)
	}
}

func TestManagerCall(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p)

	args := json.RawMessage(`{"message":"hello"}`)
	result, err := m.Call(context.Background(), p, "echo", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Output != `{"message":"hello"}` {
		t.Errorf("output = %q, want %q", result.Output, `{"message":"hello"}`)
	}
}

func TestManagerCallMultiple(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p)

	for i := range 3 {
		args := json.RawMessage(`{"n":` + string(rune('0'+i)) + `}`)
		result, err := m.Call(context.Background(), p, "echo", args)
		if err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		if result.Output != string(args) {
			t.Errorf("call %d: output = %q, want %q", i, result.Output, args)
		}
	}
}

func TestManagerStop(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := m.Stop(p); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	_, err = m.Call(context.Background(), p, "echo", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error after Stop, got nil")
	}
}

func TestManagerStopIdempotent(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if err := m.Stop(p); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := m.Stop(p); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}
