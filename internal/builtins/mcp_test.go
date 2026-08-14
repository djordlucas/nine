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
func startMCPBridge(t *testing.T, server string) (*plugin.Plugin, *plugin.Manager, string) {
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
	return p, m, serverBin
}

// The bridge presents as a plugin named for its server, not as "mcp" or "nine".
// That name is what the roster shows and what [plugins].disabled matches.
func TestMCPBridgeNamesInstance(t *testing.T) {
	p, _, _ := startMCPBridge(t, "fixture")

	if p.Name != "mcp:fixture" {
		t.Errorf("plugin name = %q, want %q", p.Name, "mcp:fixture")
	}
}

// Tools arrive prefixed with the server name, so two servers that both
// advertise the same tool cannot collide and silently lose one.
func TestMCPBridgeDescribePrefixesTools(t *testing.T) {
	p, _, _ := startMCPBridge(t, "fixture")

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
	p, m, _ := startMCPBridge(t, "fixture")

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
	p, m, _ := startMCPBridge(t, "fixture")

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
	p, _, _ := startMCPBridge(t, "fixture")

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
	p, m, serverBin := startMCPBridge(t, "fixture")

	// Kill the MCP server out from under the bridge, the way a crashing server
	// would go. Matched by its own binary path rather than a bare name: this
	// package runs tests in parallel with others that spawn the same fixture,
	// and a machine-wide pattern kill would take theirs down too.
	kill := exec.Command("pkill", "-9", "-f", serverBin)
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

// TestMCPBridgeSurvivesSlowServer is the regression test for the bug that made
// every real MCP server fail to load.
//
// The daemon bounds how long it waits for a plugin's socket (~3s, R-PLUG.3),
// but plugin.describe has no deadline. The bridge originally completed the MCP
// handshake *before* listening, so a server slower than 3s to start was
// declared dead — and `npx`-launched servers take far longer than that. Every
// bridge test passed anyway, because the fixture is a compiled binary that
// starts instantly. Delaying it is what makes this test mean anything.
func TestMCPBridgeSurvivesSlowServer(t *testing.T) {
	serverBin := filepath.Join(t.TempDir(), "testmcpserver")
	build := exec.Command("go", "build", "-o", serverBin, "./internal/builtins/testmcpserver")
	build.Dir = moduleRoot()
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test MCP server: %v\n%s", err, b)
	}
	spec, _ := json.Marshal(map[string]any{"name": "slow", "command": serverBin})

	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	// Comfortably past the socket-ready budget: before the fix this failed with
	// "plugin socket … not ready".
	p, err := m.StartBuiltinInstance("mcp", plugin.MCPInstanceName("slow"),
		"NINE_MCP_SERVER="+string(spec), "MCP_TEST_STARTUP_DELAY=5s")
	if err != nil {
		t.Fatalf("slow-starting MCP server failed to load: %v", err)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup

	if len(p.Tools) != 1 || p.Tools[0].Name != "slow__mcp_echo" {
		t.Errorf("tools = %+v, want [slow__mcp_echo]", p.Tools)
	}
}

// TestMCPBridgeHandshakeTimeoutFires is the regression test for a deadlock that
// made the handshake timeout unreachable.
//
// mcpStdio.call held a single mutex across the blocking Scan, and stop() — the
// only way to interrupt that read — began by taking the same mutex. So a server
// that accepted stdin and never replied left the timeout waiting on the very
// read it was meant to unblock: plugin.describe has no deadline, so the daemon's
// boot would have hung forever. Splitting callMu from stateMu is what makes the
// timeout able to fire at all.
//
// The bridge is driven directly rather than through the manager so the test can
// use a short deadline instead of the production three minutes.
func TestMCPBridgeHandshakeTimeoutFires(t *testing.T) {
	serverBin := filepath.Join(t.TempDir(), "testmcpserver")
	build := exec.Command("go", "build", "-o", serverBin, "./internal/builtins/testmcpserver")
	build.Dir = moduleRoot()
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test MCP server: %v\n%s", err, b)
	}
	spec, _ := json.Marshal(map[string]any{"name": "hang", "command": serverBin})

	sock := filepath.Join(t.TempDir(), "h.sock")
	cmd := exec.Command(nineBin, "plugin", "serve", "mcp")
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"NINE_PLUGIN_SOCKET=" + sock,
		"NINE_MCP_SERVER=" + string(spec),
		"MCP_TEST_HANG=1",
		// Far under the production three minutes: this asserts the timeout can
		// fire at all, not what its default is.
		"NINE_MCP_HANDSHAKE_TIMEOUT=5s",
	}
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bridge: %v", err)
	}
	t.Cleanup(func() { cmd.Process.Kill() }) //nolint:errcheck // test cleanup

	// The bridge must give up on its own. Before the fix it never would; the
	// bound here is far under the production timeout but well over the time a
	// deadlock-free path needs to notice.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-done:
		// Exited, which is the point: a hung server does not hold the bridge open.
	case <-time.After(60 * time.Second):
		t.Fatal("bridge never gave up on a server that accepts stdin and never replies; the handshake timeout cannot fire")
	}
}
