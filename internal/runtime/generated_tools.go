package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/toolvm"
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
}

// NewGeneratedToolStore wires the generated tier, or returns nil when it is off.
// A nil result disables tool_write/tool_delete/js_eval end to end: the builder
// registers no handlers and advertises no defs, so a loop is identical to one
// built before the tier existed.
func NewGeneratedToolStore(store *memory.Store, host *toolvm.Host, mgr toolOwner) agent.GeneratedToolStore {
	if store == nil || host == nil || !host.AgentEnabled() {
		return nil
	}
	return &generatedTools{store: store, host: host, mgr: mgr}
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

	if err := g.store.GeneratedToolUpsert(memory.GeneratedTool{
		Name:        spec.Name,
		Description: spec.Description,
		InputSchema: spec.InputSchema,
		// Source is the tool's JavaScript, the code the host compiles and runs; the
		// row holds it verbatim (every tools row is agent-authored, so there is no
		// separate provenance to store).
		Source:       spec.Source,
		Capabilities: spec.Capabilities,
	}); err != nil {
		return nil, err
	}

	evicted, err := g.store.GeneratedToolEvictOldest(g.host.MaxGeneratedTools())
	if err != nil {
		return nil, err
	}

	// Audit (§9.3): what code was written and the reach it declared. Structured on
	// the daemon log, the same posture net.http calls have (R-TVM.12 item 8) — "what
	// was written, with what reach" is answerable now; per-turn attribution in the
	// session_events journal waits on the tool host carrying a session id.
	slog.Info("generated tool written",
		"tool", spec.Name, "fs", decl.FS, "net", decl.Net, "env", decl.Env, "evicted", evicted)

	g.reload(ctx)
	return evicted, nil
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
	return g.host.EvalGenerated(ctx, source, decl, args)
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
