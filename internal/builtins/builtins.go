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

// mcpServeFuncs maps a built-in's wire name to the function that serves it over
// MCP (`nine mcp serve <name>`, adr/modular_nine.md): the only wire protocol that
// crosses a process boundary, so the only serve path left (the legacy
// `nine plugin serve` envelope was removed at P4).
var mcpServeFuncs = map[string]func(version string){
	"shell": serveShellMCP,
}

// autoStart names the built-ins the daemon starts unconditionally at boot, one
// process each — each over MCP (`nine mcp serve <name>`, adr/modular_nine.md).
//
// Only shell remains. time, files and http are built-in Go tools now
// (internal/agent/shipped_*.go). None of them needed a subprocess holding the
// daemon's uid — which is what a built-in plugin is, and which meant read_file
// could read any absolute path and http_get could reach cloud instance
// metadata. shell stays because it needs real exec.
//
// The `mcp` bridge builtin is gone (P5): external [[mcp.server]] servers are
// spawned directly by the plugin manager (StartMCPServer), so there is no
// built-in left that is not auto-started.
var autoStart = []string{"shell"}

// AutoStart returns the built-ins to start at daemon boot, in a stable order.
func AutoStart() []string {
	out := make([]string, len(autoStart))
	copy(out, autoStart)
	sort.Strings(out)
	return out
}

// HasMCPServe reports whether name is a built-in this binary can serve over MCP
// (`nine mcp serve <name>`). Every built-in is MCP-servable now; the daemon
// starts them through the manager's MCP launch (adr/modular_nine.md).
func HasMCPServe(name string) bool {
	_, ok := mcpServeFuncs[name]
	return ok
}

// MCPNames returns every built-in this binary can serve over MCP, in a stable
// order.
func MCPNames() []string {
	out := make([]string, 0, len(mcpServeFuncs))
	for name := range mcpServeFuncs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ServeMCP runs the named built-in over MCP, blocking until it is stopped. It
// is the body of `nine mcp serve <name>`, invoked by the daemon's plugin
// manager as a child process. It returns an error only for an unknown name; a
// serving plugin that cannot listen exits the process itself (mcp.Serve).
func ServeMCP(name, version string) error {
	fn, ok := mcpServeFuncs[name]
	if !ok {
		return fmt.Errorf("unknown MCP-servable built-in %q (have: %v)", name, MCPNames())
	}
	fn(version)
	return nil
}
