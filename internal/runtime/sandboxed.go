package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"nine/internal/agent"
	"nine/internal/config"
	"nine/internal/plugin"
	"nine/internal/toolvm"
)

// OpenSandboxedTools builds the sandboxed-tool host from config and loads the
// developer tools in [tools].user_dir, or returns nil when the subsystem is off.
//
// nil is the default and the shipped posture. Every caller downstream treats a
// nil host as "no sandboxed tools", so a deployment that never sets
// `[tools] enabled` builds exactly the loops it did before this existed.
//
// A failure to open the host is logged and yields nil rather than aborting the
// boot: an operator whose wasm runtime will not start should lose the sandboxed
// tools, not their daemon.
func OpenSandboxedTools(ctx context.Context, cfg *config.Config, mgr toolOwner) *toolvm.Host {
	if !cfg.Tools.Enabled {
		return nil
	}

	timeout, err := parseToolTimeout(cfg.Tools.Timeout)
	if err != nil {
		slog.Error("sandboxed tools disabled: bad [tools] timeout", "err", err)
		return nil
	}

	host, err := toolvm.Open(ctx, toolvm.Config{
		UserDir:  cfg.Tools.UserDir,
		Grants:   toolGrants(cfg),
		Timeout:  timeout,
		MemoryMB: cfg.Tools.MemoryMB,
	})
	if err != nil {
		slog.Error("sandboxed tools disabled: cannot open the wasm host", "err", err)
		return nil
	}

	host.Load(ctx, pluginCollides(mgr))
	return host
}

// ReloadSandboxedTools re-runs discovery, for the `nine tools reload` path. Like
// user plugins, newly-loaded tools are picked up by subsequently-built agent
// loops; turns already in flight keep the tool set they started with (§9.1).
func ReloadSandboxedTools(ctx context.Context, host *toolvm.Host, mgr toolOwner) {
	if host == nil {
		return
	}
	host.Load(ctx, pluginCollides(mgr))
}

// toolOwner is the slice of the plugin manager the collision check needs. An
// interface so the daemon can pass its own pluginRegistry on the reload path.
type toolOwner interface{ Running() []*plugin.Plugin }

// pluginCollides tells the host which names are already taken outside it, so
// "no override, ever" holds against built-in and plugin tools — the names the
// host cannot see for itself.
func pluginCollides(mgr toolOwner) toolvm.Collides {
	return func(name string) (string, bool) {
		// Core-intercepted tools are half the reserved namespace, and they are
		// checked first precisely because they do not depend on the plugin
		// manager: a sandboxed tool named `memory_set` would shadow a tool the
		// model relies on existing, under a very different contract, and that
		// must hold even in a configuration with no plugins running at all.
		for _, def := range agent.InterceptedDefs {
			if def.Name == name {
				return "nine (built-in)", true
			}
		}
		if mgr == nil {
			return "", false
		}
		for _, p := range mgr.Running() {
			if p == nil {
				continue
			}
			for _, t := range p.Tools {
				if t.Name == name {
					return p.Name + " (plugin)", true
				}
			}
		}
		return "", false
	}
}

// toolGrants converts the `[tool.<name>]` tables into the host's grant type.
// Config.Validate has already refused the combinations that are config errors,
// so this is a pure translation.
func toolGrants(cfg *config.Config) map[string]toolvm.Grant {
	if len(cfg.Tool) == 0 {
		return nil
	}
	out := make(map[string]toolvm.Grant, len(cfg.Tool))
	for name, entry := range cfg.Tool {
		caps := entry.Capabilities
		out[name] = toolvm.Grant{
			FSRead:  mounts(caps.FS.Read),
			FSWrite: mounts(caps.FS.Write),
			Env:     caps.Env,
			NetHTTP: caps.Net.HTTP != nil,
		}
	}
	return out
}

func mounts(in []config.ToolMount) []toolvm.Mount {
	if len(in) == 0 {
		return nil
	}
	out := make([]toolvm.Mount, len(in))
	for i, m := range in {
		out[i] = toolvm.Mount{Host: m.Host, Guest: m.Guest}
	}
	return out
}

// parseToolTimeout reads the `[tools] timeout` duration string. Empty keeps the
// package default.
func parseToolTimeout(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("parse %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("timeout %q must be positive", s)
	}
	return d, nil
}
