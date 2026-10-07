package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

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
	// allowStanding and maxStanding bound the standing flavour: whether Nine may
	// ask for one at all, and how many may exist.
	allowStanding bool
	maxStanding   int
	// standingLog is the driver's recent-activity ring, so deleting a tool also
	// drops its buffer rather than leaking one entry set per deleted tool.
	standingLog *standingLog
}

// LinkStandingTools connects the generated-tool store to the standing driver, so
// deleting a generated tool also drops its recent-activity buffer.
//
// A free function rather than a constructor argument because the two are built
// in the opposite order at boot — the store first, the driver once the host and
// the config are resolved — and threading a not-yet-existent runner through the
// constructor would be worse than one small seam.
func LinkStandingTools(gts agent.GeneratedToolStore, r *StandingRunner) {
	g, ok := gts.(*generatedTools)
	if !ok || r == nil {
		return
	}
	g.standingLog = r.Log()
}

// NewGeneratedToolStore wires the generated tier, or returns nil when it is off.
// A nil result disables tool_write/tool_delete/js_eval end to end: the builder
// registers no handlers and advertises no defs, so a loop is identical to one
// built before the tier existed.
func NewGeneratedToolStore(store *memory.Store, host *toolvm.Host, mgr toolOwner, bundler *deps.Bundler, allowNetworkDeps bool) agent.GeneratedToolStore {
	return NewGeneratedToolStoreWithStanding(store, host, mgr, bundler, allowNetworkDeps, false, 0)
}

// DefaultMaxGeneratedStanding caps how many standing tools Nine may have written
// itself. Small on purpose: unlike a catalogued tool, which costs nothing until
// called, each of these consumes cadence forever.
const DefaultMaxGeneratedStanding = 4

// NewGeneratedToolStoreWithStanding is NewGeneratedToolStore with the standing
// flavour's operator policy.
func NewGeneratedToolStoreWithStanding(store *memory.Store, host *toolvm.Host, mgr toolOwner, bundler *deps.Bundler, allowNetworkDeps, allowStanding bool, maxStanding int) agent.GeneratedToolStore {
	if store == nil || host == nil || !host.AgentEnabled() {
		return nil
	}
	if maxStanding <= 0 {
		maxStanding = DefaultMaxGeneratedStanding
	}
	return &generatedTools{
		store: store, host: host, mgr: mgr, bundler: bundler,
		allowNetworkDeps: allowNetworkDeps,
		allowStanding:    allowStanding, maxStanding: maxStanding,
	}
}

// Write validates a proposed tool against the ceiling and the namespace, persists
// it, evicts down to the cap, and re-projects the catalog into the host.
func (g *generatedTools) Write(ctx context.Context, spec agent.GeneratedToolSpec) (agent.WriteResult, error) {
	decl, err := parseDeclaration(spec.Capabilities)
	if err != nil {
		return agent.WriteResult{}, err
	}
	// Refuse before persisting: a capability the ceiling excludes, a colliding
	// name, or a malformed name must come back as a message the model can act on
	// (§7), not as a row left behind that never loads.
	if _, err := g.host.CheckGenerated(spec.Name, decl, pluginCollides(g.mgr)); err != nil {
		return agent.WriteResult{}, err
	}
	// Same discipline for the long-running lifecycle. It is checked here rather
	// than only at load so the refusal reaches the model as a message it can act
	// on, instead of a tool that persists and then silently never registers.
	if spec.Standing != nil {
		if err := g.checkStandingRequest(spec); err != nil {
			return agent.WriteResult{}, err
		}
	}
	if spec.Resumable && !g.host.AllowLongRunningGenerated() {
		return agent.WriteResult{}, fmt.Errorf(
			"long-running generated tools are not enabled on this instance " +
				"([tools.agent] allow_long_running). Rewrite the tool to finish in one call, " +
				"or call capability_request to ask an operator to enable it")
	}

	// Resolve and inline external npm dependencies now, at write time, once (§4.4).
	// What lands in the row is the self-contained bundle; by call time it has no
	// imports but nine:* and no way to reach the network.
	source, lock, err := g.bundle(ctx, spec.Source, decl)
	if err != nil {
		return agent.WriteResult{}, err
	}
	var lockJSON json.RawMessage
	if !lock.Empty() {
		if lockJSON, err = json.Marshal(lock); err != nil {
			return agent.WriteResult{}, err
		}
	}

	// A rewrite that changes nothing is reported as such: it is the shape a
	// looping model produces, and saying "wrote it" again gives it no way to
	// notice. Compared before the upsert, which would overwrite the evidence.
	unchanged := false
	if prev, ok, perr := g.store.GeneratedToolGet(spec.Name); perr == nil && ok {
		// Capabilities are compared as parsed declarations, not as bytes: the store
		// normalises an omitted object to "{}", so the raw forms of two identical
		// writes differ.
		prevDecl, derr := parseDeclaration(prev.Capabilities)
		unchanged = derr == nil &&
			prev.Source == source &&
			prev.Description == spec.Description &&
			string(prev.InputSchema) == string(spec.InputSchema) &&
			reflect.DeepEqual(prevDecl, decl) &&
			prev.Resumable == spec.Resumable
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
		return agent.WriteResult{}, err
	}

	evicted, err := g.store.GeneratedToolEvictOldest(g.host.MaxGeneratedTools())
	if err != nil {
		return agent.WriteResult{}, err
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

	if spec.Standing != nil {
		if err := g.promoteToStanding(spec); err != nil {
			return agent.WriteResult{}, err
		}
	}

	g.reload(ctx)
	return agent.WriteResult{Evicted: evicted, Unchanged: unchanged}, nil
}

// checkStandingRequest refuses a standing promotion before anything is
// persisted, so the model gets a message it can act on rather than a tool that
// exists and never runs.
func (g *generatedTools) checkStandingRequest(spec agent.GeneratedToolSpec) error {
	if !g.allowStanding {
		return fmt.Errorf(
			"standing tools are not enabled for generated tools on this instance " +
				"([tools.agent] allow_standing). Write it as an ordinary tool and call it " +
				"when you need it, or call capability_request to ask an operator")
	}
	if !spec.Resumable {
		// A standing run is a sequence of cycles, and a tool that cannot end a
		// call with `continue` has no way to express one.
		return fmt.Errorf("a standing tool must also be resumable: set resumable = true, " +
			"and return again() from \"nine:job\" when a cycle has more work to do")
	}
	if spec.Standing.Interval == "" && spec.Standing.Schedule == "" {
		return fmt.Errorf("a standing tool needs interval or schedule — one with no cadence would never run")
	}
	if spec.Standing.Interval != "" && spec.Standing.Schedule != "" {
		return fmt.Errorf("give interval or schedule, not both")
	}
	if spec.Standing.Interval != "" {
		d, err := time.ParseDuration(spec.Standing.Interval)
		if err != nil {
			return fmt.Errorf("interval %q is not a duration like \"10s\": %w", spec.Standing.Interval, err)
		}
		if d <= 0 {
			return fmt.Errorf("interval %q must be positive", spec.Standing.Interval)
		}
	}

	// The cap counts only generated runs: an operator's own [[standing_tool]]
	// blocks are their business and are bounded by their file.
	existing, err := g.store.StandingToolList()
	if err != nil {
		return err
	}
	n := 0
	for _, st := range existing {
		if st.Generated && st.ID != standingIDFor(spec.Name) {
			n++
		}
	}
	if n >= g.maxStanding {
		return fmt.Errorf(
			"you already have %d standing tools, the maximum on this instance. "+
				"Stop one you no longer need before adding another", g.maxStanding)
	}
	return nil
}

// promoteToStanding records the run. Called after the tool row is written, so a
// refusal above never leaves a standing row pointing at a tool that does not
// exist.
func (g *generatedTools) promoteToStanding(spec agent.GeneratedToolSpec) error {
	interval := 0
	if spec.Standing.Interval != "" {
		d, _ := time.ParseDuration(spec.Standing.Interval) // validated above
		interval = int(d.Seconds())
	}
	args := "{}"
	if len(spec.Standing.Args) > 0 {
		args = string(spec.Standing.Args)
	}
	id := standingIDFor(spec.Name)
	if err := g.store.StandingToolUpsertDefinition(memory.StandingTool{
		ID: id, Tool: spec.Name, Args: args,
		IntervalSecs: interval, Schedule: spec.Standing.Schedule, Generated: true,
	}); err != nil {
		return fmt.Errorf("record standing tool: %w", err)
	}
	if _, err := g.store.StandingToolSetState(id, memory.StandingRunning); err != nil {
		return fmt.Errorf("start standing tool: %w", err)
	}
	slog.Info("generated standing tool started",
		"id", id, "tool", spec.Name, "interval_secs", interval, "schedule", spec.Standing.Schedule)
	return nil
}

// standingIDFor names a generated tool's standing run. Derived rather than
// random so rewriting the tool replaces its run instead of accumulating one per
// write.
func standingIDFor(tool string) string { return "gen:" + tool }

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
// next-built loop. Only a tool Nine wrote can be deleted: any other name is
// refused with the reason, rather than reported deleted when nothing was.
func (g *generatedTools) Delete(ctx context.Context, name string) error {
	if _, ok, err := g.store.GeneratedToolGet(name); err != nil {
		return err
	} else if !ok {
		return g.notDeletable(name)
	}
	if err := g.store.GeneratedToolDelete(name); err != nil {
		return err
	}
	// A standing run outliving the tool it runs would be a row the driver picks
	// up every tick and fails on, forever, with "unknown sandboxed tool" — and
	// it would eventually trip the breaker and notify a human about a tool that
	// no longer exists. Deleting the tool deletes its run.
	if err := g.store.StandingToolDelete(standingIDFor(name)); err != nil {
		slog.Warn("could not remove the standing run of a deleted tool", "tool", name, "err", err)
	}
	if g.standingLog != nil {
		g.standingLog.forget(standingIDFor(name))
	}
	slog.Info("generated tool deleted", "tool", name)
	g.reload(ctx)
	return nil
}

// notDeletable explains why name, which is not a tool Nine wrote, cannot be
// deleted. The API maps these phrases to status codes: "not found" to 404,
// "cannot be deleted" to 403.
func (g *generatedTools) notDeletable(name string) error {
	if g.host == nil {
		return fmt.Errorf("no tool named %q that Nine wrote: not found", name)
	}
	if t := g.host.Get(name); t != nil {
		if t.Shipped {
			return fmt.Errorf("tool %q is built into Nine and cannot be deleted", name)
		}
		return fmt.Errorf("tool %q was installed by the operator and cannot be deleted; remove its files instead", name)
	}
	return fmt.Errorf("no tool named %q that Nine wrote: not found", name)
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
		// A schema that is not a JSON object would become this tool's `parameters`
		// in every LLM request that advertises it, and a provider rejecting that
		// field fails the whole turn. Write-time validation refuses one now; a row
		// stored before that check is skipped here rather than allowed to break
		// every session that loads it.
		if len(r.InputSchema) > 0 {
			var obj map[string]json.RawMessage
			if jerr := json.Unmarshal(r.InputSchema, &obj); jerr != nil {
				slog.Warn("generated tool input_schema is not a JSON object; skipping",
					"tool", r.Name, "err", jerr)
				continue
			}
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
