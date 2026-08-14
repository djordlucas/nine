package builtins_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/plugin"
)

// These replace the old in-core MCP adapter tests. They exercise the same
// ground, but through the real path an MCP server now takes: the daemon starts
// a `mcp` bridge instance, the bridge spawns the server over stdio, and the
// tools come back through the ordinary plugin contract
// (spec/contracts/plugin.md R-PLUG.15). Nothing here reaches into the bridge —
// if these pass, an MCP server is a plugin as far as Nine is concerned.

// startMCPBridge builds the test MCP server, then starts a bridge instance
// pointed at it exactly as cmd/nine/daemon.go does.
func startMCPBridge(t *testing.T, server string) (*plugin.Plugin, *plugin.Manager) {
	t.Helper()

	serverBin := filepath.Join(t.TempDir(), "testmcpserver")
	build := exec.Command("go", "build", "-o", serverBin, "./internal/builtins/testmcpserver")
	build.Dir = moduleRoot()
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test MCP server: %v\n%s", err, b)
	}

	spec, err := json.Marshal(map[string]any{"name": server, "command": serverBin})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartBuiltinInstance("mcp", plugin.MCPInstanceName(server), "NINE_MCP_SERVER="+string(spec))
	if err != nil {
		t.Fatalf("start mcp bridge: %v", err)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup
	return p, m
}

// The bridge presents as a plugin named for its server, not as "mcp" or "nine".
// That name is what the roster shows and what [plugins].disabled matches.
func TestMCPBridgeNamesInstance(t *testing.T) {
	p, _ := startMCPBridge(t, "fixture")

	if p.Name != "mcp:fixture" {
		t.Errorf("plugin name = %q, want %q", p.Name, "mcp:fixture")
	}
}

// Tools arrive prefixed with the server name, so two servers that both
// advertise the same tool cannot collide and silently lose one.
func TestMCPBridgeDescribePrefixesTools(t *testing.T) {
	p, _ := startMCPBridge(t, "fixture")

	if len(p.Tools) != 1 {
		t.Fatalf("got %d tools, want 1: %+v", len(p.Tools), p.Tools)
	}
	tool := p.Tools[0]
	if tool.Name != "fixture__mcp_echo" {
		t.Errorf("tool name = %q, want %q", tool.Name, "fixture__mcp_echo")
	}
	if tool.Description == "" {
		t.Error("tool description is empty; the server's description was dropped")
	}
	if len(tool.InputSchema) == 0 {
		t.Error("tool input schema is empty; the server's schema was dropped")
	}
}

// A call goes out under the prefixed name and reaches the server under its own.
func TestMCPBridgeCall(t *testing.T) {
	p, m := startMCPBridge(t, "fixture")

	args := json.RawMessage(`{"message":"hello mcp"}`)
	res, err := m.Call(context.Background(), p, "fixture__mcp_echo", args)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != string(args) {
		t.Errorf("output = %q, want %q", res.Output, args)
	}
}

// stdio is serial, and several calls in a row must not desynchronize request
// from response — the failure mode this transport is prone to.
func TestMCPBridgeCallRepeated(t *testing.T) {
	p, m := startMCPBridge(t, "fixture")

	for i := range 3 {
		args := json.RawMessage(`{"n":` + string(rune('0'+i)) + `}`)
		res, err := m.Call(context.Background(), p, "fixture__mcp_echo", args)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.Output != string(args) {
			t.Errorf("call %d: output = %q, want %q", i, res.Output, args)
		}
	}
}

// The bridge declares itself serial. stdio cannot match a response to a request
// without owning the stream for the round trip, so the daemon has to know not to
// fan out into it (R-PLUG.8) — a limitation that used to be an "except MCP"
// clause in the contract and is now just a number the plugin reports.
func TestMCPBridgeDeclaresSerial(t *testing.T) {
	p, _ := startMCPBridge(t, "fixture")

	if p.MaxConcurrent != 1 {
		t.Errorf("max_concurrent = %d, want 1 — the daemon would fan out into a serial transport", p.MaxConcurrent)
	}
}

// A bridge with no server spec must fail fast rather than serve nothing: it is
// started per [[mcp.server]] and has no meaning without one.
func TestMCPBridgeRequiresSpec(t *testing.T) {
	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)

	p, err := m.StartBuiltinInstance("mcp", plugin.MCPInstanceName("nospec"))
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal("bridge started with no NINE_MCP_SERVER; want failure")
	}
}

// A server that cannot be spawned fails the bridge rather than producing a
// plugin with no tools, so the daemon logs one clear failure and moves on.
func TestMCPBridgeUnreachableServerFails(t *testing.T) {
	spec, _ := json.Marshal(map[string]any{"name": "gone", "command": "/nonexistent/mcp-server"})

	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartBuiltinInstance("mcp", plugin.MCPInstanceName("gone"), "NINE_MCP_SERVER="+string(spec))
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal("bridge started for an unspawnable server; want failure")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Error("failure carried no reason")
	}
}

// A bridge whose server has died must not keep serving. Left alone it reports
// ok in the roster, fails every call with "MCP server closed stdout", and leaves
// the server unreaped as a zombie — a plugin that looks alive while what it
// fronts is gone. Exiting makes a dead server present as a dead plugin, which is
// what puts an MCP server on the same footing as every other plugin (R-PLUG.15).
func TestMCPBridgeDiesWithItsServer(t *testing.T) {
	p, m := startMCPBridge(t, "fixture")

	// Kill the MCP server out from under the bridge, the way a crashing server
	// would go.
	kill := exec.Command("pkill", "-9", "-f", "testmcpserver")
	kill.Run() //nolint:errcheck // no match is fine; the assertion below is the check

	// The bridge should exit, so its socket stops answering. Poll rather than
	// sleep a fixed time: this is a process teardown, not a fixed latency.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.Call(context.Background(), p, "fixture__mcp_echo", json.RawMessage(`{}`)); err != nil {
			return // the bridge is gone, which is the point
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("bridge still answering after its MCP server died; the daemon would never restart it")
}
