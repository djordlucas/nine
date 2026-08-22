package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/toolvm"
	"nine/internal/toolvm/deps"
)

// generatedTools bridges the agent-facing store (tool_write / tool_delete /
// js_eval) to the two subsystems that actually hold a generated tool: the memory
// store, which owns the row, and the wasm host, which compiles and runs it.
//
// The split is the design's core invariant (docs/sandboxed-tools.md §2). The
// store keeps the code and the tool's *declaration*; it has no column for a
// grant, and this bridge never writes one. The agent writes the code, the
// operator writes the grants, and they are never the same actor.
type generatedTools struct {
	store *memory.Store
	host  *toolvm.Host
	mgr   toolOwner
	// bundler resolves external npm imports at write time (§4.4), or nil when
	// [tools.agent.deps] is off — then any external import is refused outright.
	bundler *deps.Bundler
	// allowNetworkDeps lifts the deps+net.http interlock (§4.4).
	allowNetworkDeps bool
}

// NewGeneratedToolStore wires the generated tier, or returns nil when it is off.
// A nil result disables tool_write/tool_delete/js_eval end to end: the builder
// registers no handlers and advertises no defs, so a loop is identical to one
// built before the tier existed.
func NewGeneratedToolStore(store *memory.Store, host *toolvm.Host, mgr toolOwner, bundler *deps.Bundler, allowNetworkDeps bool) agent.GeneratedToolStore {
	if store == nil || host == nil || !host.AgentEnabled() {
		return nil
	}
	return &generatedTools{store: store, host: host, mgr: mgr, bundler: bundler, allowNetworkDeps: allowNetworkDeps}
}

// Write validates a proposed tool against the ceiling and the namespace, persists
// it, evicts down to the cap, and re-projects the catalog into the host.
func (g *generatedTools) Write(ctx context.Context, spec agent.GeneratedToolSpec) ([]string, error) {
	decl, err := parseDeclaration(spec.Capabilities)
	if err != nil {
		return nil, err
	}
	// Refuse before persisting: a capability the ceiling excludes, a colliding
	// name, or a malformed name must come back as a message the model can act on
	// (§7), not as a row left behind that never loads.
	if _, err := g.host.CheckGenerated(spec.Name, decl, pluginCollides(g.mgr)); err != nil {
		return nil, err
	}
	// Same discipline for the long-running lifecycle. It is checked here rather
	// than only at load so the refusal reaches the model as a message it can act
	// on, instead of a tool that persists and then silently never registers.
	if spec.Resumable && !g.host.AllowLongRunningGenerated() {
		return nil, fmt.Errorf(
			"long-running generated tools are not enabled on this instance " +
				"([tools.agent] allow_long_running). Rewrite the tool to finish in one call, " +
				"or use gap_report to ask an operator to enable it")
	}

	// Resolve and inline external npm dependencies now, at write time, once (§4.4).
	// What lands in the row is the self-contained bundle; by call time it has no
	// imports but nine:* and no way to reach the network.
	source, lock, err := g.bundle(ctx, spec.Source, decl)
	if err != nil {
		return nil, err
	}
	var lockJSON json.RawMessage
	if !lock.Empty() {
		if lockJSON, err = json.Marshal(lock); err != nil {
			return nil, err
		}
	}

	if err := g.store.GeneratedToolUpsert(memory.GeneratedTool{
		Name:        spec.Name,
		Description: spec.Description,
		InputSchema: spec.InputSchema,
		// Source is the BUNDLED JavaScript the host compiles and runs; Lockfile is
		// the exact third-party code it carries. Every tools row is agent-authored,
		// so there is no separate provenance to store.
		Source:       source,
		Capabilities: spec.Capabilities,
		Lockfile:     lockJSON,
		Resumable:    spec.Resumable,
	}); err != nil {
		return nil, err
	}

	evicted, err := g.store.GeneratedToolEvictOldest(g.host.MaxGeneratedTools())
	if err != nil {
		return nil, err
	}

	// Audit (§9.3). tool_write is an ordinary dispatched tool, so the loop already
	// journals the call — source, declared capabilities, result — to session_events,
	// attributed to the session and turn (journalToolStart/End). This line is the
	// supplementary operator breadcrumb: it lands in the daemon log, so "what did
	// Nine write, with what reach, and what did it evict" is greppable independent of
	// the journal and survives a session_events scrub.
	slog.Info("generated tool written",
		"tool", spec.Name, "fs", decl.FS, "net", decl.Net, "env", decl.Env,
		"resumable", spec.Resumable, "deps", lockNames(lock), "evicted", evicted)

	g.reload(ctx)
	return evicted, nil
}

// bundle resolves and inlines any external npm imports in source at write time,
// and enforces the deps+net.http interlock (§4.4). A source that imports only
// nine:* or nothing passes through untouched. Deps off (nil bundler) refuses an
// external import here, with a message the model can act on, rather than letting
// it fail cryptically at call time.
func (g *generatedTools) bundle(ctx context.Context, source string, decl toolvm.Declaration) (string, deps.Lockfile, error) {
	ext := deps.ExternalImports(source)
	if len(ext) == 0 {
		return source, deps.Lockfile{}, nil
	}
	if g.bundler == nil {
		return "", deps.Lockfile{}, fmt.Errorf(
			"tool imports external package(s) %v but external dependencies are disabled ([tools.agent.deps].mode); "+
				"import only nine:* modules, or ask an operator to enable deps", ext)
	}
	bundled, lock, err := g.bundler.Bundle(ctx, source)
	if err != nil {
		return "", deps.Lockfile{}, err
	}
	// The interlock: a package that can reach the network can exfiltrate whatever
	// the tool sees, so deps + net.http on one tool is refused by default (§4.4).
	if !lock.Empty() && declaresHTTP(decl) && !g.allowNetworkDeps {
		return "", deps.Lockfile{}, fmt.Errorf(
			"tool declares net.http and pulls external dependencies (%s); this combination is a "+
				"data-exfiltration risk and is refused unless [tools.agent] allow_network_deps = true", lockNames(lock))
	}
	return bundled, lock, nil
}

// declaresHTTP reports whether a declaration asks for net.http.
func declaresHTTP(decl toolvm.Declaration) bool {
	for _, n := range decl.Net {
		if strings.EqualFold(strings.TrimSpace(n), "http") {
			return true
		}
	}
	return false
}

// lockNames renders a lockfile's packages as "name@version, …" for audit and
// error messages; empty for a tool with no external dependencies.
func lockNames(l deps.Lockfile) string {
	if l.Empty() {
		return ""
	}
	names := make([]string, len(l.Packages))
	for i, p := range l.Packages {
		names[i] = p.Name + "@" + p.Version
	}
	return strings.Join(names, ", ")
}

// Delete removes a tool and re-projects the catalog, so it disappears from the
// next-built loop.
func (g *generatedTools) Delete(ctx context.Context, name string) error {
	if err := g.store.GeneratedToolDelete(name); err != nil {
		return err
	}
	slog.Info("generated tool deleted", "tool", name)
	g.reload(ctx)
	return nil
}

// Eval runs one snippet under the generated-tool rules and persists nothing
// (§5.3).
func (g *generatedTools) Eval(ctx context.Context, source string, caps, args json.RawMessage) (string, error) {
	decl, err := parseDeclaration(caps)
	if err != nil {
		return "", err
	}
	// js_eval runs under the identical rules, so its dependencies are resolved and
	// its interlock enforced exactly as a persisted tool's are (§5.3) — the lockfile
	// is simply discarded with everything else.
	bundled, _, err := g.bundle(ctx, source, decl)
	if err != nil {
		return "", err
	}
	return g.host.EvalGenerated(ctx, bundled, decl, args)
}

func (g *generatedTools) reload(ctx context.Context) {
	LoadGeneratedTools(ctx, g.store, g.host, g.mgr)
}

// LoadGeneratedTools projects the whole tools table into the host. Called at boot
// (OpenSandboxedTools) and after every write/delete/eviction, so the catalog the
// host holds always matches the store. Capabilities are re-resolved against the
// current ceiling on every projection, so narrowing the ceiling disables a tool
// that no longer fits under it rather than leaving it running (LoadGenerated).
func LoadGeneratedTools(ctx context.Context, store *memory.Store, host *toolvm.Host, mgr toolOwner) {
	if store == nil || host == nil || !host.AgentEnabled() {
		return
	}
	rows, err := store.GeneratedToolList()
	if err != nil {
		slog.Error("load generated tools: list", "err", err)
		return
	}
	gens := make([]toolvm.Generated, 0, len(rows))
	for _, r := range rows {
		decl, err := parseDeclaration(r.Capabilities)
		if err != nil {
			// A row whose stored declaration no longer parses is skipped rather than
			// aborting the whole projection. It was validated at write time, so this
			// is a should-not-happen guarded loudly, not a normal path.
			slog.Warn("generated tool declaration", "tool", r.Name, "err", err)
			continue
		}
		gens = append(gens, toolvm.Generated{
			Name:        r.Name,
			Description: r.Description,
			InputSchema: r.InputSchema,
			Source:      r.Source,
			Declaration: decl,
			Resumable:   r.Resumable,
		})
	}
	host.LoadGenerated(ctx, gens, pluginCollides(mgr))
}

// parseDeclaration decodes the agent-supplied capabilities object. An omitted or
// empty object is the common case — a pure transform declares nothing — and
// resolves to the zero Declaration, which grants nothing.
func parseDeclaration(raw json.RawMessage) (toolvm.Declaration, error) {
	if len(raw) == 0 {
		return toolvm.Declaration{}, nil
	}
	var d toolvm.Declaration
	if err := json.Unmarshal(raw, &d); err != nil {
		return toolvm.Declaration{}, fmt.Errorf("capabilities: %w", err)
	}
	return d, nil
}
