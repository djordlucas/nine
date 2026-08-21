// Package builtins holds the tool handlers for Nine's built-in plugins, linked
// into the nine binary itself rather than shipped as separate executables.
//
// A built-in plugin is still a *separate process*: the daemon spawns
// `nine plugin serve <name>`, which calls [Serve] and blocks in plugin.Serve on
// its own Unix socket, exactly as a standalone plugin binary did. Only the
// packaging changes — plugin isolation (spec/contracts/plugin.md R-PLUG.4), the
// sanitized spawn environment, per-plugin cache dirs, and max_concurrent all
// behave identically, because the process boundary is unchanged.
//
// Two consequences are worth stating, since they are the point of the layout:
//
//   - There is one binary to ship. `make plugins` and the image's per-plugin
//     build loop are gone.
//   - Protocol-version skew between the daemon and a built-in is structurally
//     impossible — they are the same build. The version check in
//     internal/plugin still runs, and still matters, for user plugins and MCP
//     servers, which are genuinely separate artifacts.
//
// The browser plugin is deliberately NOT here: it is Node + Chromium, not Go,
// so it remains a separate artifact resolved through [plugins].bin.
//
// Handlers live one file per plugin (shell.go, files.go, http.go, time.go).
// They share a package rather than sitting in per-plugin packages so that the
// wire names "http" and "time" stay plain data instead of Go package names that
// would shadow the stdlib packages those files import.
package builtins

import (
	"fmt"
	"sort"
)

// serveFuncs maps a built-in plugin's wire name — the name the daemon starts it
// under and the agent sees in `nine plugins` — to the function that serves it.
// Each entry blocks until the plugin process is told to stop.
var serveFuncs = map[string]func(){
	"shell":        serveShell,
	MCPBuiltinName: serveMCP,
}

// MCPBuiltinName is the built-in that bridges one MCP server. It is served like
// any other built-in but started differently — see AutoStart.
const MCPBuiltinName = "mcp"

// autoStart names the built-ins the daemon starts unconditionally at boot, one
// process each.
//
// `mcp` is deliberately absent. It is started once per [[mcp.server]], under an
// instance name (`mcp:github`) and with that server's spec in its environment
// (R-PLUG.15); a bare `mcp` has nothing to bridge and would do nothing but fail
// on every boot. Servable and startable are different questions, and this is the
// one built-in where they diverge.
// Only shell remains. time, files and http are shipped sandboxed tools now
// (internal/toolvm/shipped.go). None of them needed a subprocess holding the
// daemon's uid — which is what a built-in plugin is, and which meant read_file
// could read any absolute path and http_get could reach cloud instance
// metadata. shell stays because it needs real exec.
var autoStart = []string{"shell"}

// AutoStart returns the built-ins to start at daemon boot, in a stable order.
func AutoStart() []string {
	out := make([]string, len(autoStart))
	copy(out, autoStart)
	sort.Strings(out)
	return out
}

// Names returns every built-in this binary can serve, in a stable order —
// including ones the daemon does not start on its own (see AutoStart).
func Names() []string {
	out := make([]string, 0, len(serveFuncs))
	for name := range serveFuncs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Has reports whether name is a built-in plugin served by this binary.
func Has(name string) bool {
	_, ok := serveFuncs[name]
	return ok
}

// Serve runs the named built-in plugin, blocking until it is stopped. It is the
// body of `nine plugin serve <name>`, invoked by the daemon's plugin manager as
// a child process. It returns an error only for an unknown name; a serving
// plugin that cannot listen exits the process itself (plugin.Serve).
func Serve(name string) error {
	fn, ok := serveFuncs[name]
	if !ok {
		return fmt.Errorf("unknown built-in plugin %q (have: %v)", name, Names())
	}
	fn()
	return nil
}
