package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"time"

	"nine/internal/agent"
	"nine/internal/config"
	"nine/internal/cron"
	"nine/internal/memory"
	"nine/internal/toolvm"
	"nine/internal/toolvm/deps"

	esbuild "github.com/evanw/esbuild/pkg/api"
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
	// policy bounds the processes Nine writes (GeneratedPolicy).
	policy GeneratedPolicy
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
	return NewGeneratedToolStoreWithPolicy(store, host, mgr, bundler, allowNetworkDeps, GeneratedPolicy{})
}

// GeneratedPolicy is the operator's policy for the processes Nine writes
// (adr/process-sessions.md §9).
type GeneratedPolicy struct {
	// AllowProcesses is [tools.agent] allow_processes.
	AllowProcesses bool
	// ProcessRoles is [tools.agent] process_roles; empty is ["process"].
	ProcessRoles []string
	// MaxRunning is [processes] max_running; <= 0 is the default.
	MaxRunning int
	// Budget is [processes] budget, the most a written process may ask for.
	Budget config.BudgetConfig
}

// NewGeneratedToolStoreWithPolicy is NewGeneratedToolStore with the operator's
// policy for the processes Nine writes.
func NewGeneratedToolStoreWithPolicy(store *memory.Store, host *toolvm.Host, mgr toolOwner, bundler *deps.Bundler, allowNetworkDeps bool, policy GeneratedPolicy) agent.GeneratedToolStore {
	if store == nil || host == nil || !host.AgentEnabled() {
		return nil
	}
	if policy.MaxRunning <= 0 {
		policy.MaxRunning = DefaultMaxRunning
	}
	if len(policy.ProcessRoles) == 0 {
		policy.ProcessRoles = []string{ProcessRole}
	}
	policy.Budget = config.ProcessesConfig{Budget: policy.Budget}.BudgetOrDefault()
	return &generatedTools{
		store: store, host: host, mgr: mgr, bundler: bundler,
		allowNetworkDeps: allowNetworkDeps, policy: policy,
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
	if err := checkSyntax(spec.Source); err != nil {
		return agent.WriteResult{}, err
	}
	if spec.Process != nil {
		if err := g.checkProcessRequest(spec); err != nil {
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
			prev.Resumable == spec.Resumable &&
			prev.Live == isLiveProcess(spec)
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
		Live:         isLiveProcess(spec),
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

	res := agent.WriteResult{Evicted: evicted, Unchanged: unchanged}
	if spec.Process != nil {
		if err := g.recordProcess(spec); err != nil {
			return agent.WriteResult{}, err
		}
		res.Process = standingIDFor(spec.Name)
	}

	g.reload(ctx)
	return res, nil
}

// checkSyntax refuses source that does not parse as an ES module, at write
// time, with where and why. Without it a syntax error surfaced only when the
// tool was called or its process started — for a process, in a log the model
// never reads, so it kept failing while the model believed it ran. Parsed with
// esbuild, already vendored for the dependency bundler (§4.4).
func checkSyntax(source string) error {
	res := esbuild.Transform(source, esbuild.TransformOptions{
		Loader: esbuild.LoaderJS, Format: esbuild.FormatESModule, Target: esbuild.ES2023,
	})
	if len(res.Errors) == 0 {
		return nil
	}
	e := res.Errors[0]
	where := ""
	if e.Location != nil {
		where = fmt.Sprintf(" at line %d, column %d", e.Location.Line, e.Location.Column+1)
		if e.Location.LineText != "" {
			where += fmt.Sprintf(" (%s)", strings.TrimSpace(e.Location.LineText))
		}
	}
	return fmt.Errorf("source does not parse%s: %s. Nothing was written; fix the source and write it again",
		where, e.Text)
}

// isLiveProcess reports whether a write asks for a live process: a process
// block on a tool that is not resumable. A resumable one runs as a slice
// process, a cycle at a time, as standing tools did.
func isLiveProcess(spec agent.GeneratedToolSpec) bool {
	return spec.Process != nil && !spec.Resumable
}

// checkProcessRequest refuses a process before anything is persisted, so the
// model gets a message it can act on rather than a tool that exists and never
// runs.
func (g *generatedTools) checkProcessRequest(spec agent.GeneratedToolSpec) error {
	p, pol := spec.Process, g.policy
	if !pol.AllowProcesses {
		return fmt.Errorf(
			"processes Nine writes are not enabled on this instance ([tools.agent] allow_processes). " +
				"Tell the person; the operator turns them on. Do not build a substitute — a script or schedule " +
				"you write does not run on its own. You can still do the work once, now")
	}
	if p.Every != "" && p.Schedule != "" {
		return fmt.Errorf("give every or schedule, not both")
	}
	if p.Every != "" {
		d, err := time.ParseDuration(p.Every)
		if err != nil {
			return fmt.Errorf("every %q is not a duration like \"30m\": %w", p.Every, err)
		}
		if d <= 0 {
			return fmt.Errorf("every %q must be positive", p.Every)
		}
	}
	if p.Schedule != "" {
		if _, err := cron.Parse(p.Schedule); err != nil {
			return fmt.Errorf("schedule %q: %w", p.Schedule, err)
		}
	}
	switch {
	case spec.Resumable && p.Every == "" && p.Schedule == "":
		// A slice process is called on its clock; with none it would never run.
		return fmt.Errorf("a resumable process needs every or schedule: it is called a cycle at a time on its clock")
	case !spec.Resumable && !strings.Contains(spec.Source, "nine:process"):
		return fmt.Errorf("a process's source is a live program: import { next, turn, report } from \"nine:process\" " +
			"and loop on next(), which waits for each trigger. A process cannot call other tools, yours included: " +
			"do the work in its own program — read and write files with \"nine:fs\", ask the model with turn(). " +
			"(A tool that does one bounded slice per call is resumable instead: set resumable = true.)")
	}
	role := p.Role
	if role == "" {
		role = ProcessRole
	}
	if !slices.Contains(pol.ProcessRoles, role) {
		return fmt.Errorf("role %q is not one the operator allows for processes Nine writes; allowed: %s "+
			"([tools.agent] process_roles)", role, strings.Join(pol.ProcessRoles, ", "))
	}
	if b := p.Budget; b != nil {
		switch {
		case b.TurnsPerDay < 0 || b.TokensPerDay < 0:
			return fmt.Errorf("budget turns_per_day and tokens_per_day cannot be negative")
		case b.TurnsPerDay > pol.Budget.TurnsPerDay || b.TokensPerDay > pol.Budget.TokensPerDay:
			return fmt.Errorf("budget may only lower [processes] budget (%d turns, %d tokens a day), never raise it",
				pol.Budget.TurnsPerDay, pol.Budget.TokensPerDay)
		}
	}
	if p.ReportTo != "" {
		// A process Nine writes pipes only into another one it wrote: never into
		// a goal session or a declared process, whose reach the operator chose.
		target, ok, err := g.store.ProcessGet(standingIDFor(p.ReportTo))
		if err != nil {
			return err
		}
		if !ok || !target.Generated || target.Mode != memory.ProcessLive {
			return fmt.Errorf("report_to %q names no live process you wrote; a process you write may pipe only "+
				"into another one you wrote", p.ReportTo)
		}
	}

	// The cap is max_running, the one cap on processes running at once. A
	// rewrite of a process that is already running does not count twice.
	n, err := g.store.ProcessesRunning()
	if err != nil {
		return err
	}
	if prev, ok, err := g.store.ProcessGet(standingIDFor(spec.Name)); err == nil && ok &&
		prev.State != memory.ProcessStopped {
		n--
	}
	if n >= pol.MaxRunning {
		return fmt.Errorf("%d processes are already running, the maximum on this instance ([processes] max_running). "+
			"Report this to the person rather than stopping another process unless they asked you to", n)
	}
	return nil
}

// recordProcess records the process a write asked for. Called after the tool
// row is written, so a refusal above never leaves a process pointing at a tool
// that does not exist. A new process starts running; a rewrite keeps the run
// state it had, so a process the operator stopped stays stopped.
func (g *generatedTools) recordProcess(spec agent.GeneratedToolSpec) error {
	p := spec.Process
	interval := 0
	if p.Every != "" {
		d, _ := time.ParseDuration(p.Every) // validated above
		interval = int(d.Seconds())
	}
	args := "{}"
	if len(p.Args) > 0 {
		args = string(p.Args)
	}
	role := p.Role
	if role == "" {
		role = ProcessRole
	}
	id := standingIDFor(spec.Name)
	row := memory.Process{
		ID: id, Tool: spec.Name, Args: args,
		IntervalSecs: interval, Schedule: p.Schedule, Generated: true,
		Mode: memory.ProcessSlice, Owner: true,
	}
	if isLiveProcess(spec) {
		row.Mode, row.SessionID, row.Role = memory.ProcessLive, id, role
	}
	if b := p.Budget; b != nil {
		row.BudgetTurns, row.BudgetTokens = b.TurnsPerDay, b.TokensPerDay
	}
	if p.ReportTo != "" {
		row.ReportTo = standingIDFor(p.ReportTo)
	}
	_, existed, err := g.store.ProcessGet(id)
	if err != nil {
		return err
	}
	if err := g.store.ProcessUpsertDefinition(row); err != nil {
		return fmt.Errorf("record process: %w", err)
	}
	if !existed {
		if _, err := g.store.ProcessSetState(id, memory.ProcessRunning); err != nil {
			return fmt.Errorf("start process: %w", err)
		}
	}
	slog.Info("process written by Nine", "id", id, "tool", spec.Name, "mode", row.Mode,
		"role", row.Role, "every_secs", interval, "schedule", p.Schedule)
	return nil
}

// standingIDFor names the process a tool Nine wrote runs. Derived rather than
// random so rewriting the tool replaces its process instead of accumulating one
// per write.
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
	return g.delete(ctx, name, false)
}

// DeleteByOperator is the operator's delete (nine tools delete): unlike a
// model's, it also removes a process the operator stopped.
func (g *generatedTools) DeleteByOperator(ctx context.Context, name string) error {
	return g.delete(ctx, name, true)
}

func (g *generatedTools) delete(ctx context.Context, name string, byOperator bool) error {
	if _, ok, err := g.store.GeneratedToolGet(name); err != nil {
		return err
	} else if !ok {
		return g.notDeletable(name)
	}
	// A model deleting a process the operator stopped would undo that stop, as
	// starting it would (adr/process-sessions.md §9).
	if p, ok, err := g.store.ProcessGet(standingIDFor(name)); err == nil && ok && !byOperator &&
		p.State == memory.ProcessStopped && p.StoppedBy == ByOperator {
		return fmt.Errorf("tool %q runs process %s, which the operator stopped; only the operator can delete it",
			name, p.ID)
	}
	if err := g.store.GeneratedToolDelete(name); err != nil {
		return err
	}
	// A process outliving the tool it runs would fail on every trigger with
	// "unknown sandboxed tool". Deleting the tool deletes its process; the next
	// runner pass closes a live one's instance, and its session is kept.
	if err := g.store.ProcessDelete(standingIDFor(name)); err != nil {
		slog.Warn("could not remove the process of a deleted tool", "tool", name, "err", err)
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
			Live:        r.Live,
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
