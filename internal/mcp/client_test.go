package mcp_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/mcp"
)

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

// buildTestMCPServer compiles the stdio MCP fixture shipped in
// internal/builtins/testmcpserver (adr/modular_nine.md §10: the internal/mcp
// client is unit-tested against it).
func buildTestMCPServer(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "testmcpserver")
	cmd := exec.Command("go", "build", "-o", out, "./internal/builtins/testmcpserver")
	cmd.Dir = findModuleRoot(t)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test MCP server: %v\n%s", err, b)
	}
	return out
}

// The client handshakes a stdio server end to end: initialize →
// notifications/initialized → tools/list, then a tools/call round trip.
func TestConnectStdioAndCall(t *testing.T) {
	bin := buildTestMCPServer(t)
	spec := mcp.ServerSpec{Name: "fixture", Command: bin}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, tools, err := mcp.Connect(ctx, spec)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Stop() //nolint:errcheck

	if len(tools) != 1 || tools[0].Name != "mcp_echo" {
		t.Fatalf("tools = %+v, want [mcp_echo]", tools)
	}
	if len(tools[0].InputSchema) == 0 {
		t.Error("input schema dropped")
	}

	args := json.RawMessage(`{"message":"hello mcp"}`)
	out, err := mcp.CallTool(ctx, conn, "mcp_echo", args)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != string(args) {
		t.Errorf("output = %q, want %q", out, args)
	}
}

// An unknown method comes back as a JSON-RPC error, not a silent nil result.
func TestUnknownMethodErrors(t *testing.T) {
	bin := buildTestMCPServer(t)
	spec := mcp.ServerSpec{Name: "fixture", Command: bin}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := mcp.Connect(ctx, spec)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer conn.Stop() //nolint:errcheck

	if _, err := conn.Call(ctx, "bogus/method", nil); err == nil || !strings.Contains(err.Error(), "MCP error -32601") {
		t.Errorf("Call(bogus) err = %v, want MCP error -32601", err)
	}
}

// NINE_MCP_HANDSHAKE_TIMEOUT overrides the handshake budget when parseable and
// falls back to the default when not — a malformed duration must not cost the
// operator their server.
func TestHandshakeTimeoutEnv(t *testing.T) {
	t.Setenv("NINE_MCP_HANDSHAKE_TIMEOUT", "90s")
	if got := mcp.HandshakeTimeout(); got != 90*time.Second {
		t.Errorf("HandshakeTimeout() = %v, want 90s", got)
	}
	t.Setenv("NINE_MCP_HANDSHAKE_TIMEOUT", "not-a-duration")
	if got := mcp.HandshakeTimeout(); got != mcp.DefaultHandshakeTimeout {
		t.Errorf("HandshakeTimeout() = %v, want default %v on unparseable value", got, mcp.DefaultHandshakeTimeout)
	}
}

// A server that accepts stdin and never replies must not hang the handshake
// forever: the ctx deadline tears the connection down, which unblocks the read.
func TestHandshakeCtxCancelTearsDownHungServer(t *testing.T) {
	bin := buildTestMCPServer(t)
	spec := mcp.ServerSpec{Name: "hang", Command: bin, Env: map[string]string{"MCP_TEST_HANG": "1"}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, _, err := mcp.Connect(ctx, spec)
	if err == nil {
		t.Fatal("Connect against a hung server = nil error, want timeout")
	}
	if !strings.Contains(err.Error(), "timed out") && !strings.Contains(err.Error(), "deadline") {
		t.Errorf("err = %v, want a deadline/timeout error", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Errorf("Connect took %v, want the ~2s ctx deadline to fire", elapsed)
	}
}
