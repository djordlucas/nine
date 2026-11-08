package builtins_test

import (
	"slices"
	"strings"
	"testing"

	"nine/internal/builtins"
	"nine/internal/plugin"
)

// TestAutoStartMatchesDaemonRoster pins what the daemon starts unconditionally
// at boot (cmd/nine/daemon.go); spec/contracts/plugin.md R-PLUG.5 lists the same
// set, so a plugin added or dropped here is a contract change.
//
// Only shell is left. `browser` is Node + Chromium, not Go, so it is not a
// built-in; it arrives as an [[mcp.server]] (docs/browser.md). `time`, `files`
// and `http` are built-in Go tools now (internal/agent/shipped_*.go). The
// `mcp` bridge builtin is gone (P5): the manager spawns [[mcp.server]] servers
// directly.
func TestAutoStartMatchesDaemonRoster(t *testing.T) {
	want := []string{"shell"}
	if got := builtins.AutoStart(); !slices.Equal(got, want) {
		t.Errorf("AutoStart() = %v, want %v", got, want)
	}
	if !builtins.HasMCPServe("shell") {
		t.Error("shell must be MCP-servable: the daemon spawns `nine mcp serve shell`")
	}
	if got := builtins.MCPNames(); !slices.Equal(got, want) {
		t.Errorf("MCPNames() = %v, want %v — every built-in serves MCP now (P4)", got, want)
	}
}

// TestServeMCPUnknownName checks the `nine mcp serve <name>` entry point fails
// with a usable message rather than serving nothing, since a typo there would
// otherwise surface as a plugin that never starts listening.
func TestServeMCPUnknownName(t *testing.T) {
	err := builtins.ServeMCP("nope", "test")
	if err == nil {
		t.Fatal(`ServeMCP("nope") = nil, want error`)
	}
	for _, want := range []string{"nope", "shell"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}

// TestStartMCPBuiltinUnknownName covers the manager side of the same mistake.
// The child exits non-zero without ever listening, so this fails the way any
// plugin that never comes up does — waiting out the socket-ready timeout first.
// That is the slow path, not a fast one; the check here is only that it reports
// a failure instead of handing back a plugin nothing is serving.
func TestStartMCPBuiltinUnknownName(t *testing.T) {
	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartMCPBuiltin("nope")
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal(`StartMCPBuiltin("nope") = nil error, want failure`)
	}
}

// TestStartMCPBuiltinNamesPlugin guards the one thing the built-in launch path
// has to get right that the binary path gave away for free: the plugin's wire
// name. The executable is "nine" for every built-in, so a name derived from the
// path would label them all "nine" in `nine plugins` and status.
func TestStartMCPBuiltinNamesPlugin(t *testing.T) {
	p, _ := start(t, "shell")
	if p.Name != "shell" {
		t.Errorf("plugin name = %q, want %q", p.Name, "shell")
	}
}
