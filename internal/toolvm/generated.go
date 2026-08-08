package toolvm

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// DefaultMaxGeneratedTools caps the generated catalog (§9.2).
const DefaultMaxGeneratedTools = 64

// GeneratedSource identifies a tool Nine wrote itself, mirroring how skills
// already split built-in from agent-authored.
const GeneratedSource = "agent"

// Generated is one agent-authored tool as the host receives it. The store owns
// the row; this is the shape the host needs to compile and run it.
type Generated struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Source      string
	// Declaration is what the tool says it needs, checked against the ceiling.
	Declaration Declaration
}

// AgentConfig is the generated tier's operator policy.
type AgentConfig struct {
	// Enabled turns the tier on. With it off, LoadGenerated registers nothing and
	// the host holds developer tools only.
	Enabled bool
	// Ceiling is the maximum a generated tool may be granted — never an automatic
	// grant (see resolveCeiling).
	Ceiling Ceiling
	// MaxTools caps the catalog; 0 uses DefaultMaxGeneratedTools.
	MaxTools int
}

// LoadGenerated registers agent-authored tools alongside the developer tools
// already loaded. It is called after Load, and re-called after each tool_write,
// so a newly written tool is visible to subsequently-built agent loops.
//
// Generated tools are checked against the same namespace rules as everything
// else — they lose every collision, including against developer tools. And their
// capabilities are re-resolved against the *current* ceiling on every load, so
// narrowing the ceiling disables a tool that no longer fits under it rather than
// leaving it running with reach the operator has since withdrawn.
func (h *Host) LoadGenerated(ctx context.Context, tools []Generated, collides Collides) {
	if !h.agent.Enabled {
		return
	}

	h.mu.Lock()
	// Drop the previously-loaded generated set, keeping developer tools: this
	// doubles as reload, and a tool deleted from the store must disappear.
	for name, t := range h.tools {
		if t.Generated {
			delete(h.tools, name)
		}
	}
	existing := make(map[string]*Tool, len(h.tools))
	for k, v := range h.tools {
		existing[k] = v
	}
	h.mu.Unlock()

	var status []Status
	for _, g := range tools {
		st := Status{Name: g.Name, Kind: string(KindJS), Generated: true}

		grant, err := resolveCeiling(g.Declaration, h.agent.Ceiling)
		if err != nil {
			status = append(status, skip(st, err, "ceiling"))
			continue
		}
		st.Capabilities = grant.Summary()

		if _, taken := existing[g.Name]; taken {
			status = append(status, skip(st,
				fmt.Errorf("tool %q is already provided by a developer tool or plugin", g.Name), "collision"))
			continue
		}
		if collides != nil {
			if owner, taken := collides(g.Name); taken {
				status = append(status, skip(st,
					fmt.Errorf("tool %q already provided by %q", g.Name, owner), "collision"))
				continue
			}
		}

		t := &Tool{
			Name:        g.Name,
			Description: g.Description,
			InputSchema: g.InputSchema,
			// Always js: the agent cannot supply a .wasm blob, because a binary
			// blob is not reviewable and there is no reason to accept one (§5.2).
			Kind:      KindJS,
			Grant:     grant,
			Generated: true,
			module:    h.qjs,
			source:    g.Source,
			// The `nine:*` stdlib is the generated tier's import allowlist (§4.2):
			// a tool may import any nine: module and nothing else. External npm
			// imports are bundled into Source at write time (§4.4), so by call time
			// the only imports left are these.
			imports: stdlibModules(),
		}

		h.mu.Lock()
		h.tools[g.Name] = t
		h.mu.Unlock()
		existing[g.Name] = t

		st.Loaded = true
		status = append(status, st)
	}

	h.mu.Lock()
	// Generated statuses are kept separate from the developer-tool statuses so a
	// Load() of the directory does not erase them, and vice versa.
	h.generatedStatus = status
	h.mu.Unlock()

	loaded := 0
	for _, s := range status {
		if s.Loaded {
			loaded++
		}
	}
	slog.Info("loaded generated tools", "loaded", loaded, "skipped", len(status)-loaded)
	_ = ctx
}

// SetAgentConfig installs the generated tier's policy. Called at open and
// whenever config is re-read.
func (h *Host) SetAgentConfig(c AgentConfig) {
	if c.MaxTools <= 0 {
		c.MaxTools = DefaultMaxGeneratedTools
	}
	h.mu.Lock()
	h.agent = c
	h.mu.Unlock()
}

// AgentEnabled reports whether the generated tier is on.
func (h *Host) AgentEnabled() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.agent.Enabled
}

// MaxGeneratedTools returns the catalog cap.
func (h *Host) MaxGeneratedTools() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.agent.MaxTools <= 0 {
		return DefaultMaxGeneratedTools
	}
	return h.agent.MaxTools
}

// CheckGenerated validates a proposed generated tool without registering it, so
// tool_write can refuse with a reason the model can act on *before* anything is
// written to the store. It returns the grant the tool would run with.
//
// Compiling is not part of this: a `js` tool is never compiled at install time,
// and the interpreter reports a syntax error at first call. Validating the
// source would mean running it, which is exactly what a write must not do.
func (h *Host) CheckGenerated(name string, decl Declaration, collides Collides) (Grant, error) {
	if !h.AgentEnabled() {
		return Grant{}, fmt.Errorf("generated tools are not enabled on this instance ([tools.agent] enabled)")
	}
	if !validToolName(name) {
		return Grant{}, fmt.Errorf("tool name %q must match [a-z0-9_]+", name)
	}

	h.mu.RLock()
	existing, taken := h.tools[name]
	ceiling := h.agent.Ceiling
	h.mu.RUnlock()

	// Rewriting one's own tool is the intended path; shadowing someone else's is
	// not, and no override is ever permitted.
	if taken && !existing.Generated {
		return Grant{}, fmt.Errorf("tool %q is already provided by a developer tool; generated tools never override", name)
	}
	if collides != nil {
		if owner, t := collides(name); t {
			return Grant{}, fmt.Errorf("tool %q is already provided by %q; generated tools never override", name, owner)
		}
	}

	return resolveCeiling(decl, ceiling)
}

// EvalGenerated runs one JS snippet under exactly the generated-tool rules and
// discards everything (§5.3). Nothing is registered, nothing is stored, and the
// snippet cannot see or be seen by any other tool.
//
// It exists because two of the three motivating uses — testing a library and
// iterating on a tool against candidates — do not want a persisted tool at all.
// Without it every experiment transits the catalog, and single-use tools
// accreting there is precisely what degrades tool ranking for everything else.
func (h *Host) EvalGenerated(ctx context.Context, source string, decl Declaration, args json.RawMessage) (string, error) {
	if !h.AgentEnabled() {
		return "", fmt.Errorf("generated tools are not enabled on this instance ([tools.agent] enabled)")
	}

	h.mu.RLock()
	ceiling := h.agent.Ceiling
	h.mu.RUnlock()

	grant, err := resolveCeiling(decl, ceiling)
	if err != nil {
		return "", err
	}

	// A throwaway Tool, never registered. It runs through the identical path a
	// catalogued tool does — same sandbox, same bounds, same capability
	// resolution — so js_eval is not a softer tier, only a less persistent one.
	t := &Tool{
		Name:        "js_eval",
		Description: "ephemeral evaluation",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		Kind:        KindJS,
		Grant:       grant,
		Generated:   true,
		module:      h.qjs,
		source:      source,
		// Same import surface as a catalogued generated tool (§4.2): js_eval is a
		// less-persistent tier, not a softer one.
		imports: stdlibModules(),
	}
	return h.call(ctx, t, args)
}
