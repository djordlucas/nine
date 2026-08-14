package builtins_test

import (
	"slices"
	"strings"
	"testing"

	"nine/internal/builtins"
	"nine/internal/plugin"
)

// TestNamesMatchesDaemonRoster pins the built-in roster. The daemon starts
// exactly these at boot (cmd/nine/daemon.go), and spec/contracts/plugin.md
// R-PLUG.5 lists them, so a plugin added or dropped here is a contract change.
// browser is deliberately absent: it is Node + Chromium, not Go, so it still
// ships as its own artifact under [plugins].bin.
func TestNamesMatchesDaemonRoster(t *testing.T) {
	want := []string{"files", "http", "shell", "time"}
	if got := builtins.Names(); !slices.Equal(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if builtins.Has("browser") {
		t.Error("browser must not be a built-in: it is Node + Chromium, not Go")
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
	p, _ := start(t, "time")
	if p.Name != "time" {
		t.Errorf("plugin name = %q, want %q", p.Name, "time")
	}
}
