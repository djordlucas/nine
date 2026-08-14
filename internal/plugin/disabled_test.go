package plugin

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// [plugins].disabled is the operator's lever for withholding a capability —
// `shell` above all — and it replaced a lever that used to exist implicitly:
// before the Go plugins moved into the nine binary, not shipping a binary kept
// its plugin from starting (R-PLUG.13, R-PLUG.14). These pin that the lever
// actually holds on every start path.

func TestDisabledRefusesBuiltin(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"shell"})

	p, err := m.StartBuiltin("shell")
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal("StartBuiltin(\"shell\") succeeded; want it refused")
	}
	if !errors.Is(err, ErrPluginDisabled) {
		t.Errorf("err = %v, want ErrPluginDisabled", err)
	}
}

// A disabled plugin must be refused before anything is spawned, so the refusal
// costs no process and leaves no socket. StartBuiltin with a bogus binary would
// otherwise fail slowly on the socket-ready timeout; refusing first is instant
// and reports the real reason.
func TestDisabledRefusesBeforeSpawn(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"shell"})
	m.SetBuiltinBinary("/nonexistent/nine")

	if _, err := m.StartBuiltin("shell"); !errors.Is(err, ErrPluginDisabled) {
		t.Errorf("err = %v, want ErrPluginDisabled (not a spawn failure)", err)
	}
}

// Start is the path for plugins with their own binary (browser, user plugins).
// The check lives in the shared start(), so it covers this path too.
func TestDisabledRefusesBinaryPlugin(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"browser"})

	if _, err := m.Start("/some/dir/browser"); !errors.Is(err, ErrPluginDisabled) {
		t.Errorf("err = %v, want ErrPluginDisabled", err)
	}
}

// TryStart/TryStartBuiltin tolerate a missing plugin, so they must report a
// disabled one as a decision — nil, no error — and record it for the roster.
func TestTryStartBuiltinDisabledIsNotAFailure(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"shell", "http"})

	if p := m.TryStartBuiltin("shell"); p != nil {
		t.Error("TryStartBuiltin returned a plugin for a disabled name")
	}
	if p := m.TryStart("browser"); p != nil {
		t.Error("TryStart returned a plugin for a name not disabled but absent")
	}
	if p := m.TryStartBuiltin("http"); p != nil {
		t.Error("TryStartBuiltin returned a plugin for a disabled name")
	}

	// Only the disabled ones are recorded; `browser` was merely absent, which is
	// a different thing and must not be reported as switched off.
	got := m.DisabledSkipped()
	slices.Sort(got)
	if want := []string{"http", "shell"}; !slices.Equal(got, want) {
		t.Errorf("DisabledSkipped() = %v, want %v", got, want)
	}
}

// The roster is the whole point of recording skips: an operator debugging a
// missing tool has to be able to see that they switched it off. Recording twice
// would double the line.
func TestDisabledSkippedDeduplicates(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"shell"})

	m.TryStartBuiltin("shell")
	m.TryStartBuiltin("shell")

	if got := m.DisabledSkipped(); len(got) != 1 {
		t.Errorf("DisabledSkipped() = %v, want one entry", got)
	}
}

func TestNotDisabledByDefault(t *testing.T) {
	m := NewManager("")
	if m.IsDisabled("shell") {
		t.Error("shell is disabled with no config saying so")
	}
	if got := m.DisabledSkipped(); len(got) != 0 {
		t.Errorf("DisabledSkipped() = %v, want empty", got)
	}
}

// SetDisabled replaces the set rather than accumulating, so a reconfigure that
// drops a name actually re-enables it.
func TestSetDisabledReplaces(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"shell"})
	m.SetDisabled([]string{"http"})

	if m.IsDisabled("shell") {
		t.Error("shell still disabled after being dropped from the set")
	}
	if !m.IsDisabled("http") {
		t.Error("http not disabled after being added to the set")
	}
}

// A name that matches no plugin disables nothing — `disabled = ["shel"]` leaves
// `shell` running. That fails open while reading as closed, so it has to be
// reportable rather than silent.
func TestUnmatchedDisabledReportsTypos(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"shell", "shel", "nosuchplugin"})

	m.TryStartBuiltin("shell") // the real one is refused and recorded

	got := m.UnmatchedDisabled()
	if want := []string{"nosuchplugin", "shel"}; !slices.Equal(got, want) {
		t.Errorf("UnmatchedDisabled() = %v, want %v", got, want)
	}
}

// Nothing to report when every name did its job.
func TestUnmatchedDisabledEmptyWhenAllMatched(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{"shell", "time"})

	m.TryStartBuiltin("shell")
	m.TryStartBuiltin("time")

	if got := m.UnmatchedDisabled(); len(got) != 0 {
		t.Errorf("UnmatchedDisabled() = %v, want empty", got)
	}
}

// An MCP server is disabled by its instance name, which is what boot registers
// and what the roster shows. Getting this wrong would mean an operator writing
// `disabled = ["mcp:github"]` and quietly getting the server anyway.
func TestDisabledRefusesBuiltinInstance(t *testing.T) {
	m := NewManager("")
	m.SetDisabled([]string{MCPInstanceName("github")})

	if p := m.TryStartBuiltinInstance("mcp", MCPInstanceName("github")); p != nil {
		t.Error("TryStartBuiltinInstance returned a plugin for a disabled instance")
	}
	if got := m.DisabledSkipped(); len(got) != 1 || got[0] != "mcp:github" {
		t.Errorf("DisabledSkipped() = %v, want [mcp:github]", got)
	}

	// Disabling one instance must not disable the built-in for the others.
	if m.IsDisabled("mcp") {
		t.Error(`disabling "mcp:github" also disabled the "mcp" built-in`)
	}
	if m.IsDisabled(MCPInstanceName("slack")) {
		t.Error(`disabling "mcp:github" also disabled "mcp:slack"`)
	}
}

// The socket path is derived from the plugin name, and instance names carry a
// colon. A separator would place the socket outside socketDir, which exists to
// keep the path under the macOS sun_path limit.
func TestSocketPathIsSafeForInstanceNames(t *testing.T) {
	path, err := allocSocketPath(MCPInstanceName("github"))
	if err != nil {
		t.Fatalf("allocSocketPath: %v", err)
	}
	if dir := filepath.Dir(path); dir != socketDir {
		t.Errorf("socket dir = %q, want %q", dir, socketDir)
	}
	if strings.ContainsAny(filepath.Base(path), ":/") {
		t.Errorf("socket filename %q still contains a path-unsafe character", filepath.Base(path))
	}
}
