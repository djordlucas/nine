package plugin

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"nine/internal/mcp"
)

// StartMCPServer connects to one external MCP server and registers it as a
// plugin named `mcp:<server>` with its tools prefixed `<server>__`
// (R-PLUG.15). A `command` spec is spawned as a supervised child of the daemon
// — no bridge process in between — and spoken to over stdio; a `url` spec is
// reached over streamable HTTP. The spawned server gets the same sanitized
// environment, cache dir, and disable switch as any plugin (R-PLUG.13/14/11);
// the config itself comes from the spec, never from nine.toml in the child.
func (m *Manager) StartMCPServer(spec mcp.ServerSpec) (*Plugin, error) {
	name := MCPInstanceName(spec.Name)

	// Every start path funnels through here, so a disabled server cannot be
	// connected by any caller.
	if m.IsDisabled(name) {
		return nil, fmt.Errorf("%q: %w", name, ErrPluginDisabled)
	}

	// A spawned server gets a cache dir like any plugin child (it may spill
	// binary parts there, R-PLUG.15b); a hosted one gets one too, because the
	// client now runs inside the daemon and needs somewhere Nine-owned to put
	// spilled bytes.
	cacheDir, ephemeral, err := m.allocCacheDir(name)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Plugin, error) {
		if ephemeral && cacheDir != "" {
			os.RemoveAll(cacheDir) //nolint:errcheck // start failed; reclaim the dir we just made
		}
		return nil, err
	}

	// The handshake runs under the handshake budget, not the socket budget:
	// there is no socket to wait for any more, and an `npx`-launched server
	// routinely needs far longer than any socket timeout just to start.
	ctx, cancel := context.WithTimeout(context.Background(), mcp.HandshakeTimeout())
	defer cancel()

	var conn mcp.Conn
	if spec.URL != "" {
		conn = mcp.DialHTTP(spec.URL, spec.Headers, mcp.WithSpillDir(cacheDir))
	} else {
		env := append([]string{}, sanitizedHostEnv()...)
		env = append(env, m.env...)
		env = append(env, m.envFor(name)...)
		if cacheDir != "" {
			env = append(env, "NINE_PLUGIN_CACHE_DIR="+cacheDir, "NINE_PLUGIN_CACHE_PERSISTENT="+persistentEnv(!ephemeral))
		}
		for k, v := range spec.Env {
			env = append(env, k+"="+v)
		}
		var derr error
		conn, derr = mcp.DialStdioEnv(spec.Command, spec.Args, env, mcp.WithSpillDir(cacheDir))
		if derr != nil {
			return fail(derr)
		}
	}

	version := spec.NineVersion
	if version == "" {
		version = "unknown"
	}
	tools, err := mcp.Handshake(ctx, conn, mcp.ProtocolVersionFor(spec), "nine", version)
	if err != nil {
		conn.Stop() //nolint:errcheck // best-effort teardown on a failed handshake
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(fmt.Errorf("handshake timed out after %s: %w", mcp.HandshakeTimeout(), ctxErr))
		}
		return fail(err)
	}

	// Prefixing lives here now, in the one client: two servers that both
	// advertise `search` would otherwise collide and one would silently lose
	// its tool (R-PLUG.9). First-party tools keep bare names (R-PLUG.16).
	defs := make([]ToolDefinition, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, ToolDefinition{
			Name:        spec.Name + mcp.ToolSeparator + t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	// stdio is serial (R-PLUG.8): one client-side write lock, declared as
	// max_concurrent 1. HTTP transports are concurrent.
	maxConcurrent := 0
	if spec.URL == "" {
		maxConcurrent = 1
	}

	p := &Plugin{
		Name:           name,
		client:         newMCPClient(name, "", nil, nil, conn),
		Tools:          defs,
		MaxConcurrent:  maxConcurrent,
		cacheDir:       cacheDir,
		cacheEphemeral: ephemeral,
	}

	// A spawned server that dies on its own marks the plugin down: the row stays
	// in the roster, and the next call errors on the dead connection, instead of
	// a bridge process serving a plugin with nothing behind it.
	if spec.URL == "" {
		go func() {
			<-conn.Exited()
			if conn.StoppedDeliberately() {
				return
			}
			slog.Warn("MCP server exited; its tools will fail until the daemon restarts", "server", spec.Name)
		}()
	}

	m.track(p)
	slog.Info("MCP server started", "name", name, "tools", len(defs),
		"transport", transportName(spec), "max_concurrent", maxConcurrent)
	return p, nil
}

// TryStartMCPServer is StartMCPServer with TryStart's tolerance: it logs and
// returns nil rather than failing the daemon's boot, so one unreachable MCP
// server does not stop Nine from starting.
func (m *Manager) TryStartMCPServer(spec mcp.ServerSpec) *Plugin {
	name := MCPInstanceName(spec.Name)
	if m.IsDisabled(name) {
		m.noteDisabledSkip(name)
		slog.Info("MCP server disabled by config, not starting", "name", name)
		return nil
	}
	p, err := m.StartMCPServer(spec)
	if err != nil {
		slog.Warn("MCP server start failed", "name", name, "err", err)
		return nil
	}
	return p
}

// transportName names the spec's transport for logs.
func transportName(spec mcp.ServerSpec) string {
	switch {
	case spec.URL != "":
		return "streamable-http"
	case spec.Socket != "":
		return "unix-socket-http"
	default:
		return "stdio"
	}
}
