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
// Four deliberate absences. `browser` is Node + Chromium, not Go, so it still
// ships as its own artifact under [plugins].bin. `mcp` is a built-in but is not
// auto-started: it runs once per [[mcp.server]] with that server's spec in its
// environment (R-PLUG.15), and a bare `mcp` would have nothing to bridge. `time` and
// `files` are no longer plugins at all — they are shipped sandboxed tools
// (internal/toolvm/shipped.go). Neither needs a subprocess holding the daemon's
// uid authority, which for files meant read_file could read any absolute path.
func TestAutoStartMatchesDaemonRoster(t *testing.T) {
	want := []string{"http", "shell"}
	if got := builtins.AutoStart(); !slices.Equal(got, want) {
		t.Errorf("AutoStart() = %v, want %v", got, want)
	}
	if builtins.Has("browser") {
		t.Error("browser must not be a built-in: it is Node + Chromium, not Go")
	}
	for _, name := range []string{"time", "files"} {
		if builtins.Has(name) {
			t.Errorf("%s must not be a built-in: it is a shipped sandboxed tool", name)
		}
	}
}

// Servable and auto-started are different questions, and mcp is the case that
// separates them: it must be servable (the daemon spawns `nine plugin serve
// mcp` per configured server) while staying out of the boot roster.
func TestMCPIsServableButNotAutoStarted(t *testing.T) {
	if !builtins.Has(builtins.MCPBuiltinName) {
		t.Error("mcp must be servable: the daemon spawns it per [[mcp.server]]")
	}
	if !slices.Contains(builtins.Names(), builtins.MCPBuiltinName) {
		t.Error("Names() must include mcp, so `plugin serve` usage lists it")
	}
	if slices.Contains(builtins.AutoStart(), builtins.MCPBuiltinName) {
		t.Error("mcp must not auto-start: with no server spec it can only fail")
	}
}

func TestHas(t *testing.T) {
	for _, name := range builtins.Names() {
		if !builtins.Has(name) {
			t.Errorf("Has(%q) = false for a name Names() reported", name)
		}
	}
	if builtins.Has("nope") {
		t.Error(`Has("nope") = true`)
	}
}

// TestServeUnknownName checks the `nine plugin serve <name>` entry point fails
// with a usable message rather than serving nothing, since a typo there would
// otherwise surface as a plugin that never starts listening.
func TestServeUnknownName(t *testing.T) {
	err := builtins.Serve("nope")
	if err == nil {
		t.Fatal("Serve(\"nope\") = nil, want error")
	}
	for _, want := range []string{"nope", "shell"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestStartBuiltinUnknownName covers the manager side of the same mistake. The
// child exits non-zero without ever listening, so this fails the way any plugin
// that never comes up does — waiting out the socket-ready timeout first. That
// is the slow path, not a fast one; the check here is only that it reports a
// failure instead of handing back a plugin nothing is serving.
func TestStartBuiltinUnknownName(t *testing.T) {
	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartBuiltin("nope")
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal("StartBuiltin(\"nope\") = nil error, want failure")
	}
}

// TestStartBuiltinNamesPlugin guards the one thing the built-in launch path has
// to get right that the binary path gave away for free: the plugin's wire name.
// The executable is "nine" for every built-in, so a name derived from the path
// would label them all "nine" in `nine plugins` and status.
func TestStartBuiltinNamesPlugin(t *testing.T) {
	p, _ := start(t, "shell")
	if p.Name != "shell" {
		t.Errorf("plugin name = %q, want %q", p.Name, "shell")
	}
}
