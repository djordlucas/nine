package plugin_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestManagerDescribe(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	p, err := m.Start(path)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Stop(p)

	if len(p.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(p.Tools))
	}
	if p.Tools[0].Name != "echo" {
		t.Errorf("tool name = %q, want %q", p.Tools[0].Name, "echo")
	}
	if p.Tools[0].Description == "" {
		t.Error("tool description is empty")
	}
	if len(p.Tools[0].InputSchema) == 0 {
		t.Error("tool input_schema is empty")
	}
}

func TestManagerRejectsProtocolMismatch(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	// A future protocol version the daemon does not speak.
	p, err := m.Start(path, "NINE_TEST_PROTOCOL_VERSION=999")
	if err == nil {
		m.Stop(p) //nolint:errcheck
		t.Fatal("expected Start to reject a protocol-mismatched plugin, got nil error")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Errorf("error = %q, want it to mention the protocol mismatch", err)
	}
	if len(m.Running()) != 0 {
		t.Errorf("rejected plugin should not be tracked; Running() = %d", len(m.Running()))
	}
}

func TestManagerRejectsUnversionedPlugin(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testplugin")
	m := plugin.NewManager("")

	// Version 0 == absent: a plugin predating protocol versioning.
	p, err := m.Start(path, "NINE_TEST_PROTOCOL_VERSION=0")
	if err == nil {
		m.Stop(p) //nolint:errcheck
		t.Fatal("expected Start to reject an unversioned plugin, got nil error")
	}
	if !strings.Contains(err.Error(), "protocol") {
		t.Errorf("error = %q, want it to mention the protocol version", err)
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

// --- MCP adapter tests ---

func TestMCPDescribe(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testmcpserver")
	m := plugin.NewManager("")

	p, err := m.StartMCP(path, nil)
	if err != nil {
		t.Fatalf("StartMCP: %v", err)
	}
	defer m.Stop(p)

	if len(p.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(p.Tools))
	}
	if p.Tools[0].Name != "mcp_echo" {
		t.Errorf("tool name = %q, want mcp_echo", p.Tools[0].Name)
	}
	if p.Tools[0].Description == "" {
		t.Error("tool description is empty")
	}
	if len(p.Tools[0].InputSchema) == 0 {
		t.Error("tool input_schema is empty")
	}
}

func TestMCPCall(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testmcpserver")
	m := plugin.NewManager("")

	p, err := m.StartMCP(path, nil)
	if err != nil {
		t.Fatalf("StartMCP: %v", err)
	}
	defer m.Stop(p)

	args := json.RawMessage(`{"message":"hello mcp"}`)
	result, err := m.Call(context.Background(), p, "mcp_echo", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result.Output != `{"message":"hello mcp"}` {
		t.Errorf("output = %q, want %q", result.Output, `{"message":"hello mcp"}`)
	}
}

func TestMCPCallMultiple(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testmcpserver")
	m := plugin.NewManager("")

	p, err := m.StartMCP(path, nil)
	if err != nil {
		t.Fatalf("StartMCP: %v", err)
	}
	defer m.Stop(p)

	for i := range 3 {
		args := json.RawMessage(`{"n":` + string(rune('0'+i)) + `}`)
		result, err := m.Call(context.Background(), p, "mcp_echo", args)
		if err != nil {
			t.Fatalf("Call %d: %v", i, err)
		}
		if result.Output != string(args) {
			t.Errorf("call %d: output = %q, want %q", i, result.Output, args)
		}
	}
}

func TestMCPStop(t *testing.T) {
	path := buildBinary(t, "./internal/plugin/testmcpserver")
	m := plugin.NewManager("")

	p, err := m.StartMCP(path, nil)
	if err != nil {
		t.Fatalf("StartMCP: %v", err)
	}

	if err := m.Stop(p); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	_, err = m.Call(context.Background(), p, "mcp_echo", json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error after Stop, got nil")
	}
}
