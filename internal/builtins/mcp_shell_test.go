package builtins_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/mcp"
	"nine/internal/plugin"
)

// shell served over MCP (adr/modular_nine.md P1): the daemon starts
// `nine mcp serve shell`, the manager handshakes MCP on the plugin socket, and
// the tool behaves exactly as it did over the legacy envelope — same bare name,
// same guard, same workspace, same timeout. The roster is unchanged: the
// plugin is still named `shell` and still advertises one tool, `shell`.

// startMCP launches one built-in through the manager's MCP launch
// (`nine mcp serve <name>`) and stops it when the test ends.
func startMCP(t *testing.T, name string, extraEnv ...string) (*plugin.Plugin, *plugin.Manager) {
	t.Helper()
	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartMCPBuiltin(name, extraEnv...)
	if err != nil {
		t.Fatalf("StartMCPBuiltin %s: %v", name, err)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup
	return p, m
}

func TestShellOverMCPDescribe(t *testing.T) {
	p, _ := startMCP(t, "shell")
	if p.Name != "shell" {
		t.Errorf("plugin name = %q, want %q — the roster must not change", p.Name, "shell")
	}
	if len(p.Tools) != 1 || p.Tools[0].Name != "shell" {
		t.Fatalf("tools = %+v, want one bare [shell]", p.Tools)
	}
	if len(p.Tools[0].InputSchema) == 0 {
		t.Error("input schema dropped on the MCP path")
	}
	if p.Tools[0].Description == "" {
		t.Error("description dropped on the MCP path")
	}
}

func TestShellOverMCPEcho(t *testing.T) {
	p, m := startMCP(t, "shell")
	r, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"echo hello-mcp"}`))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	json.Unmarshal([]byte(r.Output), &out) //nolint:errcheck
	if !strings.Contains(out.Stdout, "hello-mcp") {
		t.Errorf("stdout = %q, want 'hello-mcp'", out.Stdout)
	}
	if out.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", out.ExitCode)
	}
}

func TestShellOverMCPRunsInWorkspace(t *testing.T) {
	ws := t.TempDir()
	resolved, err := filepath.EvalSymlinks(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes.txt"), []byte("pangolin-8321\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, m := startMCP(t, "shell", "NINE_WORKSPACE="+ws)
	r, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"cat notes.txt"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Output, "pangolin-8321") {
		t.Errorf("output = %q, want the workspace file's contents", r.Output)
	}
	r, err = m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"pwd -P"}`))
	if err != nil {
		t.Fatal(err)
	}
	var pwd struct {
		Stdout string `json:"stdout"`
	}
	json.Unmarshal([]byte(r.Output), &pwd) //nolint:errcheck
	if got := strings.TrimSpace(pwd.Stdout); got != resolved {
		t.Errorf("pwd = %q, want %q", got, resolved)
	}
}

func TestShellOverMCPTimeout(t *testing.T) {
	p, m := startMCP(t, "shell")
	_, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"sleep 10","timeout":1}`))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want 'timed out'", err)
	}
}

// The destructive-command guard fires on the MCP path exactly as on the legacy
// one: the handler is shared, but this pins that the error survives the
// isError → Go error mapping.
func TestShellOverMCPBlocked(t *testing.T) {
	p, m := startMCP(t, "shell")
	_, err := m.Call(context.Background(), p, "shell", json.RawMessage(`{"command":"rm -rf /tmp/foo"}`))
	if err == nil {
		t.Fatal("rm -rf was allowed over MCP, want blocked")
	}
	if !strings.Contains(err.Error(), "blocked") {
		t.Errorf("error = %v, want 'blocked'", err)
	}
}

// [plugins].disabled blocks both start paths by wire name (R-PLUG.14): the MCP
// launch and the legacy launch of `shell` are both refused before spawning.
func TestDisabledBlocksBothShellStartPaths(t *testing.T) {
	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	m.SetDisabled([]string{"shell"})

	if p := m.TryStartMCPBuiltin("shell"); p != nil {
		m.Stop(p) //nolint:errcheck
		t.Error("TryStartMCPBuiltin(shell) returned a plugin, want nil")
	}
	if p := m.TryStartBuiltin("shell"); p != nil {
		m.Stop(p) //nolint:errcheck
		t.Error("TryStartBuiltin(shell) returned a plugin, want nil")
	}
	if got := m.UnmatchedDisabled(); len(got) != 0 {
		t.Errorf("UnmatchedDisabled = %v, want none", got)
	}
}

// The stdio opt-in (adr/modular_nine.md §9.6): `nine mcp serve shell` with
// NINE_MCP_TRANSPORT=stdio serves MCP on its pipes, no socket needed, and the
// hand-rolled stdio client reaches it end to end.
func TestShellOverMCPStdio(t *testing.T) {
	ws := t.TempDir()
	spec := mcp.ServerSpec{
		Name:    "shell",
		Command: nineBin,
		Args:    []string{"mcp", "serve", "shell"},
		Env: map[string]string{
			"NINE_MCP_TRANSPORT": "stdio",
			"NINE_WORKSPACE":     ws,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, tools, err := mcp.Connect(ctx, spec)
	if err != nil {
		t.Fatalf("Connect over stdio: %v", err)
	}
	t.Cleanup(func() { conn.Stop() }) //nolint:errcheck

	if len(tools) != 1 || tools[0].Name != "shell" {
		t.Fatalf("tools = %+v, want [shell]", tools)
	}

	out, err := mcp.CallTool(ctx, conn, "shell", json.RawMessage(`{"command":"echo stdio-shell"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(out, "stdio-shell") {
		t.Errorf("output = %q, want 'stdio-shell'", out)
	}

	if _, err := mcp.CallTool(ctx, conn, "shell", json.RawMessage(`{"command":"rm -rf /tmp/foo"}`)); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Errorf("blocked command err = %v, want 'blocked'", err)
	}
}
