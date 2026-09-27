package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"nine/internal/agent"
	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/toolvm"
	"nine/internal/toolvm/deps"
)

// OpenSandboxedTools builds the sandboxed-tool host from config and loads the
// developer tools in [tools].user_dir, or returns nil when the subsystem is off.
//
// A host is the default: `[tools] enabled` defaults to true, because this tier
// carries the workspace file tools. Every caller downstream still treats a nil
// host as "no sandboxed tools", so an operator who sets `enabled = false` builds
// exactly the loops they would have before this existed.
//
// A failure to open the host is logged and yields nil rather than aborting the
// boot: an operator whose wasm runtime will not start should lose the sandboxed
// tools, not their daemon.
//
// store is where generated tools live (spec/contracts/toolvm.md R-TVM.14). It
// feeds the initial catalog into the host and receives the per-call usage touch
// that drives LRU eviction; a nil store leaves the generated tier off, whatever
// [tools.agent] says.
func OpenSandboxedTools(ctx context.Context, cfg *config.Config, store *memory.Store, mgr toolOwner) *toolvm.Host {
	if !cfg.Tools.IsEnabled() {
		return nil
	}

	timeout, err := parseToolTimeout(cfg.Tools.Timeout)
	if err != nil {
		slog.Error("sandboxed tools disabled: bad [tools] timeout", "err", err)
		return nil
	}

	timeouts, err := toolTimeouts(cfg)
	if err != nil {
		slog.Error("sandboxed tools disabled: bad per-tool timeout", "err", err)
		return nil
	}

	host, err := toolvm.Open(ctx, toolvm.Config{
		UserDir:       cfg.Tools.UserDir,
		Grants:        toolGrants(cfg),
		Timeout:       timeout,
		Timeouts:      timeouts,
		MemoryMB:      cfg.Tools.MemoryMB,
		MaxConcurrent: cfg.Tools.MaxConcurrent,
		MaxOps:        cfg.Tools.MaxOps,
		MaxOpsPerTool: toolMaxOps(cfg),
		// Usage bookkeeping for LRU eviction (§9.2). Best-effort and after the
		// fact: a touch failure must not fail the tool call the model is waiting on.
		TouchGenerated: touchGenerated(store),
		// The durable store behind the `state` capability. Nil when the daemon has
		// no store, which leaves a granted tool with a named error rather than a
		// store that silently forgets.
		StateStore: newToolStateStore(store),
	})
	if err != nil {
		slog.Error("sandboxed tools disabled: cannot open the wasm host", "err", err)
		return nil
	}

	// The generated tier's operator policy — the ceiling, the cap, whether it is on
	// at all — must be installed before the first LoadGenerated, which refuses to
	// register anything while the tier is off.
	//
	// The ceiling comes from the store, and `nine.toml` is reconciled into it here:
	// the file's grants are rewritten as `config` rows and the workspace fallback as
	// `default` rows, while an operator's `approved` rows survive untouched
	// (capability_grants.go). One code path then reads the ceiling whether a grant
	// came from the file or from an approval, and a grant the file stops declaring
	// stops applying.
	ac := agentConfig(cfg)
	if grants, err := ReconcileCapabilityGrants(store, cfg); err != nil {
		// The file's own ceiling is still available, so a store that cannot be
		// reconciled costs the approved grants rather than the whole tier.
		slog.Error("capability grants not reconciled; using the config ceiling alone", "err", err)
	} else if store != nil {
		ac.Ceiling = CeilingFromGrants(grants)
	}
	host.SetAgentConfig(ac)

	// The workspace a shipped tool that declares fs is mounted at. Same root the
	// `files` plugin used, so a model's /work paths keep meaning what they meant.
	//
	// Created if absent: the mount, and `shell`'s working directory, are the same
	// directory, and a tool that declares fs fails to load against a root that is
	// not there. An operator who configured a root meant for it to exist.
	// Resolved rather than read: an unset [workspace].root would skip every shipped
	// tool that declares fs, which with this tier on by default would be the
	// out-of-the-box state (config.WorkspaceRoot).
	root := cfg.WorkspaceRoot()
	if root != "" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			slog.Warn("workspace root could not be created", "root", root, "err", err)
		}
		// The trash and its .gitignore, so the first delete does not have to
		// create them and a mounted repository is never dirtied.
		if err := PrepareWorkspaceState(root); err != nil {
			slog.Warn("workspace state directory could not be prepared", "root", root, "err", err)
		}
	}
	host.SetShippedWorkspace(toolvm.ShippedWorkspace{Host: root})

	// First-party tools first: the namespace rule is first-registered wins, so a
	// developer or generated tool must not be able to take a shipped tool's name
	// and silently replace first-party behavior.
	host.LoadShipped(ctx, pluginCollides(mgr))

	// Then the operator's own directory. Load merges into the registry rather
	// than replacing it, so the shipped tier above survives this and every later
	// `nine tools reload`.
	host.Load(ctx, pluginCollides(mgr))

	// Project the stored catalog into the host, so tools the agent wrote in a
	// previous run are callable from this one's first turn.
	LoadGeneratedTools(ctx, store, host, mgr)
	return host
}

// agentConfig translates `[tools.agent]` into the host's generated-tier policy.
// The ceiling is expressed in exactly the grant terms a developer tool uses, so
// the same mount/http translators serve both.
func agentConfig(cfg *config.Config) toolvm.AgentConfig {
	a := cfg.Tools.Agent
	caps := a.Capabilities

	// The default ceiling is the workspace, derived the same way the shipped tools'
	// mount is (SetShippedWorkspace above), so a generated tool and a shipped tool
	// name the same file the same way. Derived rather than written in TOML because
	// the root varies by deployment and nothing expands variables in config values.
	//
	// A fallback, not an override: an operator who granted any fs mount gets
	// exactly that and nothing added. And still a ceiling, not a grant — a tool
	// that declares no fs capability is mounted nothing, however wide this is
	// (toolvm.resolveCeiling).
	fsRead, fsWrite := mounts(caps.FS.Read), mounts(caps.FS.Write)
	if len(fsRead) == 0 && len(fsWrite) == 0 {
		if root := cfg.WorkspaceRoot(); root != "" {
			m := []toolvm.Mount{{Host: root, Guest: toolvm.ShippedWorkspaceGuest}}
			fsRead, fsWrite = m, m
		}
	}

	return toolvm.AgentConfig{
		Enabled:          cfg.Tools.GeneratedEnabled(),
		MaxTools:         a.MaxTools,
		AllowLongRunning: a.AllowLongRunning,
		Ceiling: toolvm.Ceiling{Grant: toolvm.Grant{
			FSRead:  fsRead,
			FSWrite: fsWrite,
			Env:     caps.Env,
			HTTP:    httpGrant(caps.Net.HTTP),
			State:   stateGrant(caps.State),
		}},
	}
}

// NewDepsBundler builds the external-dependency bundler from [tools.agent.deps],
// or returns nil when deps are off — in which case the write path refuses any
// external import outright (docs/sandboxed-tools.md §4.4). The registry client is
// the daemon's, with a bounded timeout; registry traffic is the daemon's, never a
// guest's, which is the whole reason resolution happens here.
func NewDepsBundler(cfg *config.Config) *deps.Bundler {
	d := cfg.Tools.Agent.Deps
	policy := deps.Policy{
		Mode:        cfg.Tools.Agent.DepsMode(),
		Registry:    d.Registry,
		MaxPackages: d.MaxPackages,
		MaxBundleKB: d.MaxBundleKB,
		MaxDepth:    d.MaxDepth,
		Frozen:      d.Frozen,
	}
	for _, a := range d.Allow {
		policy.Allow = append(policy.Allow, deps.Allow{Name: a.Name, Version: a.Version})
	}
	return deps.New(policy, toolsCacheDir(cfg), &http.Client{Timeout: 60 * time.Second})
}

// toolsCacheDir resolves [tools].cache_dir, defaulting under the user cache dir.
func toolsCacheDir(cfg *config.Config) string {
	if cfg.Tools.CacheDir != "" {
		return cfg.Tools.CacheDir
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "nine", "tools")
}

// touchGenerated is the host's usage hook, or nil when there is no store to
// record into.
func touchGenerated(store *memory.Store) func(string) {
	if store == nil {
		return nil
	}
	return func(name string) {
		if err := store.GeneratedToolTouch(name); err != nil {
			slog.Warn("touch generated tool", "tool", name, "err", err)
		}
	}
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
		// The generated tier's own meta-tools are core-intercepted too, but live
		// outside InterceptedDefs because they are registered conditionally
		// (register_tools.go). Reserve them explicitly, unconditionally: a generated
		// tool named `js_eval` would otherwise persist and then either shadow the
		// meta-tool or be silently shadowed by it, breaking "one name, one
		// implementation" (I-TVM.4). Reserved even when the tier is off, since the
		// names belong to a built-in capability regardless.
		for _, n := range agent.GeneratedToolNames {
			if n == name {
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
			HTTP:    httpGrant(caps.Net.HTTP),
			State:   stateGrant(caps.State),
		}
	}
	return out
}

// toolMaxOps collects the per-tool `[tool.<name>] max_ops` overrides. Unlike a
// timeout there is nothing to parse and nothing to reject, so a bad value is not
// a thing that exists here: 0 is "inherit" and negative is "off".
func toolMaxOps(cfg *config.Config) map[string]int {
	var out map[string]int
	for name, entry := range cfg.Tool {
		if entry.MaxOps == 0 {
			continue
		}
		if out == nil {
			out = make(map[string]int, len(cfg.Tool))
		}
		out[name] = entry.MaxOps
	}
	return out
}

// toolTimeouts collects the per-tool `[tool.<name>] timeout` overrides.
//
// A bad duration here disables the whole subsystem rather than being skipped,
// matching how a bad `[tools] timeout` behaves: a resource bound the operator
// wrote and Nine silently ignored is worse than a daemon that says why it will
// not start with this config.
func toolTimeouts(cfg *config.Config) (map[string]time.Duration, error) {
	var out map[string]time.Duration
	for name, entry := range cfg.Tool {
		if entry.Timeout == "" {
			continue
		}
		d, err := parseToolTimeout(entry.Timeout)
		if err != nil {
			return nil, fmt.Errorf("[tool.%s] timeout: %w", name, err)
		}
		if out == nil {
			out = make(map[string]time.Duration, len(cfg.Tool))
		}
		out[name] = d
	}
	return out, nil
}

// stateGrant translates the operator's state table. Config.Validate has already
// refused an absent or unknown scope and an unparseable ttl, so this is a pure
// translation — an error here would be a validation gap, not an operator error,
// and the duration is re-parsed rather than carried because config holds the
// operator's text and toolvm holds the resolved bound.
func stateGrant(in *config.ToolStateGrant) *toolvm.StateGrant {
	if in == nil {
		return nil
	}
	g := &toolvm.StateGrant{
		Scope:      in.Scope,
		MaxKeys:    in.MaxKeys,
		MaxValueKB: in.MaxValueKB,
		MaxTotalKB: in.MaxTotalKB,
	}
	if in.TTL != "" {
		if d, err := time.ParseDuration(in.TTL); err == nil {
			g.TTL = d
		}
	}
	return g
}

// httpGrant translates the operator's net.http table. Config.Validate has
// already refused the shapes that are config errors — a bare "*", an unknown
// method, an empty allowlist — so this is a pure translation.
func httpGrant(in *config.ToolHTTPGrant) *toolvm.HTTPGrant {
	if in == nil {
		return nil
	}
	return &toolvm.HTTPGrant{
		AllowHosts: in.AllowHosts,
		Methods:    in.Methods,
		MaxBytes:   int64(in.MaxBytes),
	}
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
