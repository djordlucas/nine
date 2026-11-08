package plugin_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/mcp"
	"nine/internal/plugin"
)

// External MCP servers are manager children now (adr/modular_nine.md P5): the
// daemon connects per [[mcp.server]] through StartMCPServer, no bridge process
// in between. These exercise that path end to end against the compiled fixture
// in internal/mcp/testmcpserver — the same fixture the internal/mcp client
// tests use — so a server is a plugin as far as Nine is concerned.

// startMCPServer builds the test MCP server, then connects to it exactly as
// cmd/nine/daemon.go's startMCPServers does.
func startMCPServer(t *testing.T, server string, env map[string]string) (*plugin.Plugin, *plugin.Manager, string) {
	t.Helper()

	serverBin := filepath.Join(t.TempDir(), "testmcpserver")
	build := exec.Command("go", "build", "-o", serverBin, "./internal/mcp/testmcpserver")
	build.Dir = findModuleRoot(t)
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test MCP server: %v\n%s", err, b)
	}

	m := plugin.NewManager("")
	p, err := m.StartMCPServer(mcp.ServerSpec{Name: server, Command: serverBin, Env: env})
	if err != nil {
		t.Fatalf("start MCP server: %v", err)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup
	return p, m, serverBin
}

// The server presents as a plugin named for its server, not as "mcp" or "nine".
// That name is what the roster shows and what [plugins].disabled matches.
func TestMCPServerNamesInstance(t *testing.T) {
	p, _, _ := startMCPServer(t, "fixture", nil)

	if p.Name != "mcp:fixture" {
		t.Errorf("plugin name = %q, want %q", p.Name, "mcp:fixture")
	}
}

// Tools arrive prefixed with the server name, so two servers that both
// advertise the same tool cannot collide and silently lose one.
func TestMCPServerPrefixesTools(t *testing.T) {
	p, _, _ := startMCPServer(t, "fixture", nil)

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
func TestMCPServerCall(t *testing.T) {
	p, m, _ := startMCPServer(t, "fixture", nil)

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
func TestMCPServerCallRepeated(t *testing.T) {
	p, m, _ := startMCPServer(t, "fixture", nil)

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

// A spawned (stdio) server declares itself serial: the daemon must not fan out
// into a transport that cannot match responses to requests (R-PLUG.8).
func TestMCPServerDeclaresSerial(t *testing.T) {
	p, _, _ := startMCPServer(t, "fixture", nil)

	if p.MaxConcurrent != 1 {
		t.Errorf("max_concurrent = %d, want 1 — the daemon would fan out into a serial transport", p.MaxConcurrent)
	}
}

// A server that cannot be spawned fails the start rather than producing a
// plugin with no tools, so the daemon logs one clear failure and moves on.
func TestMCPServerUnreachableFails(t *testing.T) {
	m := plugin.NewManager("")
	p, err := m.StartMCPServer(mcp.ServerSpec{Name: "gone", Command: "/nonexistent/mcp-server"})
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal("start succeeded for an unspawnable server; want failure")
	}
	if strings.TrimSpace(err.Error()) == "" {
		t.Error("failure carried no reason")
	}
}

// A server that dies on its own marks the plugin down: the roster row stays, and
// the next call errors on the dead connection, instead of a bridge process
// serving a plugin with nothing behind it (adr/modular_nine.md §10 P5).
func TestMCPServerDeathFailsNextCall(t *testing.T) {
	p, m, serverBin := startMCPServer(t, "fixture", nil)

	// Kill the server out from under the manager, the way a crashing server
	// would go. Matched by its own binary path rather than a bare name: this
	// package runs tests in parallel with others that spawn the same fixture,
	// and a machine-wide pattern kill would take theirs down too.
	kill := exec.Command("pkill", "-9", "-f", serverBin)
	kill.Run() //nolint:errcheck // no match is fine; the assertion below is the check

	// Poll rather than sleep a fixed time: this is a process teardown, not a
	// fixed latency.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := m.Call(context.Background(), p, "fixture__mcp_echo", json.RawMessage(`{}`)); err != nil {
			return // the call failed on the dead connection, which is the point
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("call still succeeded after the MCP server died; want an error on the dead connection")
}

// A server slower than any socket budget still loads: there is no bridge socket
// any more, so the handshake runs inside the manager under the handshake budget
// (a measured `npx` server takes ~72s to answer tools/list).
func TestMCPServerSurvivesSlowServer(t *testing.T) {
	serverBin := filepath.Join(t.TempDir(), "testmcpserver")
	build := exec.Command("go", "build", "-o", serverBin, "./internal/mcp/testmcpserver")
	build.Dir = findModuleRoot(t)
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test MCP server: %v\n%s", err, b)
	}

	m := plugin.NewManager("")
	p, err := m.StartMCPServer(mcp.ServerSpec{
		Name:    "slow",
		Command: serverBin,
		Env:     map[string]string{"MCP_TEST_STARTUP_DELAY": "5s"},
	})
	if err != nil {
		t.Fatalf("slow-starting MCP server failed to load: %v", err)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup

	if len(p.Tools) != 1 || p.Tools[0].Name != "slow__mcp_echo" {
		t.Errorf("tools = %+v, want [slow__mcp_echo]", p.Tools)
	}
}

// A server that accepts stdin and never replies must not hang the handshake
// forever: the NINE_MCP_HANDSHAKE_TIMEOUT budget fires and the start fails.
func TestMCPServerHandshakeTimeoutFires(t *testing.T) {
	serverBin := filepath.Join(t.TempDir(), "testmcpserver")
	build := exec.Command("go", "build", "-o", serverBin, "./internal/mcp/testmcpserver")
	build.Dir = findModuleRoot(t)
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build test MCP server: %v\n%s", err, b)
	}

	t.Setenv("NINE_MCP_HANDSHAKE_TIMEOUT", "5s")

	m := plugin.NewManager("")
	start := time.Now()
	p, err := m.StartMCPServer(mcp.ServerSpec{
		Name:    "hang",
		Command: serverBin,
		Env:     map[string]string{"MCP_TEST_HANG": "1"},
	})
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal("start succeeded against a server that never replies; want a handshake timeout")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %v, want a timeout error", err)
	}
	// Far under the production three minutes, well over a deadlock-free path.
	if elapsed := time.Since(start); elapsed > 60*time.Second {
		t.Errorf("start took %v, want the 5s override to fire", elapsed)
	}
}
