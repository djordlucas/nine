package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"nine/internal/mcp"
	"nine/internal/plugin"
)

// The MCP bridge. One instance fronts one MCP server: the daemon starts
// `nine plugin serve mcp` once per [[mcp.server]], each under its own wire name
// (`mcp:github`), so an MCP server is a plugin in every respect — its own
// process, its own crash isolation, its own roster row, its own disable switch
// (spec/contracts/plugin.md R-PLUG.15).
//
// The MCP client itself lives in internal/mcp now: the plugin manager is the
// single MCP client for everything in another process (adr/modular_nine.md), and
// this bridge is just one user of it — the one that also translates tool names
// with the server prefix. The limitations did not vanish — stdio still cannot
// be cancelled mid-read and still serializes — but they are now this bridge's
// business, declared honestly through the ordinary plugin contract
// (max_concurrent: 1) instead of being exceptions the daemon had to carry.

// serveMCP runs the bridge: read the server spec, spawn it, complete the MCP
// handshake, then serve its tools through the ordinary plugin contract.
func serveMCP() {
	spec, err := mcp.SpecFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: %v\n", err)
		os.Exit(1)
	}

	// The handshake runs inside ServeDeferred's ready func, not before it — the
	// socket must exist within the daemon's socket-ready budget (R-PLUG.3), and
	// an `npx`-launched MCP server routinely takes longer than that just to
	// start. Listening first moves the wait into plugin.describe, which has no
	// deadline. Handshaking first is exactly the bug that made every real
	// third-party server fail to load while a compiled test fixture passed.
	// The connection is built inside ready but has to be reachable from the
	// shutdown hook, which is registered before it exists.
	var (
		connMu sync.Mutex
		conn   mcp.Conn
	)

	plugin.ServeDeferred(
		func(ctx context.Context) ([]plugin.ToolDefinition, map[string]plugin.ToolHandler, error) {
			ctx, cancel := context.WithTimeout(ctx, mcp.HandshakeTimeout())
			defer cancel()

			c, tools, err := mcp.Connect(ctx, spec)
			if err != nil {
				return nil, nil, err
			}
			connMu.Lock()
			conn = c
			connMu.Unlock()
			exitWhenServerDies(spec.Name, c)

			defs, handlers := prefixedSurface(spec.Name, c, tools)
			return defs, handlers, nil
		},
		// stdio has no per-message framing, so a response can only be matched to
		// its request by owning the stream for the whole round trip. The transport
		// is serial; saying so lets the daemon bound concurrency correctly instead
		// of discovering it as contention (R-PLUG.8).
		plugin.WithMaxConcurrent(1),
		// Registered with Serve rather than as a second signal handler: two
		// handlers race, and Serve's calls os.Exit the moment it wakes, so a
		// competing one may never get to kill the server — leaving the orphaned
		// process tree this is here to prevent.
		plugin.WithOnShutdown(func() {
			connMu.Lock()
			c := conn
			connMu.Unlock()
			if c != nil {
				c.Stop() //nolint:errcheck // best-effort teardown on shutdown
			}
		}),
	)
}

// prefixedSurface turns the server's tools into the plugin contract's shape:
// prefixed names for the daemon and the model, each handler translating back to
// the server's own unprefixed name.
func prefixedSurface(server string, conn mcp.Conn, tools []mcp.Tool) ([]plugin.ToolDefinition, map[string]plugin.ToolHandler) {
	defs := make([]plugin.ToolDefinition, 0, len(tools))
	handlers := make(map[string]plugin.ToolHandler, len(tools))

	for _, t := range tools {
		prefixed := server + mcp.ToolSeparator + t.Name
		defs = append(defs, plugin.ToolDefinition{
			Name:        prefixed,
			Description: t.Description,
			InputSchema: t.InputSchema, // the schema itself is identical; only MCP's key name differs
		})
		// upstream is captured per tool: the model calls the prefixed name, the
		// server only ever knows its own.
		upstream := t.Name
		handlers[prefixed] = func(ctx context.Context, args json.RawMessage) (string, error) {
			// The daemon's ctx carries task_timeout, and a call bounded by nothing
			// is how one hung server wedges every later call to it.
			ctx, cancel := context.WithTimeout(ctx, mcp.CallTimeout)
			defer cancel()
			return mcp.CallTool(ctx, conn, upstream, args)
		}
	}
	return defs, handlers
}

// exitWhenServerDies ends the bridge when its MCP server exits on its own.
//
// Without this the bridge outlives the thing it exists to front: it keeps
// serving, keeps reporting `ok` in the roster, and fails every call with "MCP
// server closed stdout", while the dead server sits unreaped as a zombie. The
// bridge's liveness would mean nothing, because what it fronts is gone.
//
// Exiting makes a dead server present as a dead plugin. Nine's supervision
// watches plugin processes, not the grandchildren one spawns, so this is what
// puts an MCP server on the same footing as every other plugin — whatever
// happens to a plugin that dies happens to this one too, no sooner and no later.
// (Today that means the failure surfaces on the next call, as it does for any
// plugin: EventPluginCrashed exists in the supervisor but nothing emits it yet,
// so there is no automatic restart to rely on for MCP or anything else.)
func exitWhenServerDies(server string, conn mcp.Conn) {
	go func() {
		<-conn.Exited()
		if conn.StoppedDeliberately() {
			return // ordinary shutdown, not a failure
		}
		fmt.Fprintf(os.Stderr, "mcp %s: server exited; stopping the bridge rather than serving a plugin with nothing behind it\n", server)
		os.Exit(1)
	}()
}
