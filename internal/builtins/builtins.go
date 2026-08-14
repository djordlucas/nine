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
	"shell": serveShell,
	"files": serveFiles,
	"http":  serveHTTP,
	"time":  serveTime,
}

// Names returns the built-in plugin names in a stable order.
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
