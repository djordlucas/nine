# Daemon assembly refactor — share one wiring path

**Status:** Proposed / not yet implemented · **Purpose:** eliminate the
hand-maintained duplication between the production daemon boot (`runDaemon`) and
the in-process eval harness by extracting one shared assembly function, so that
"the harness reproduces production" becomes a compile-time guarantee instead of a
review-time reminder.

## Problem

Two places construct a fully-wired `*runtime.Daemon` from the same set of
dependencies:

- **Production** — `runDaemon` in `cmd/nine/daemon.go`.
- **Evals** — `Harness.Run` in `tests/evals/runner/harness.go`, which stands up
  the real daemon in-process over a schema-per-run store and an ephemeral
  workspace so a case is graded against the same journal production writes.

The harness is a hand-written parallel of `runDaemon`. The Go compiler catches
*signature* drift (a changed constructor breaks the harness build), but not
*additive* drift: when `runDaemon` gains a new dependency, a new
`daemon.Configure*/Set*` call, or a new `AgentBuilderConfig`/`LoopConfig` field,
the harness keeps compiling while silently no longer reproducing production.

Today that gap is policed by a Stop hook (`.claude/hooks/eval-harness-guard.sh`)
and the `/sync-evals` command. Those are *detection*, not *prevention*. This
refactor removes the duplication so there is nothing to police.

## Current shape

`runDaemon` interleaves three kinds of work:

1. **Dependency construction** — open the store, start plugins, build the
   embedder, stores, supervisor, assembler, HITL, `AgentBuilder`, and the queue.
2. **Core wiring** — `runtime.New(...)`, `SetEventSink`, `ConfigureHITL`,
   `ConfigureMemory/Plugins/Supervisor/PlanStore`, `SetSubAgentLister`,
   `SetQueueStatFn`, `SetMaxGoalSessions`, `SetGoalSessionSpawnFn`,
   `SetEmitProgressFn`, register the `pursue`/`idle-reflection` stages, start
   `supervisor.Run`.
3. **Production-only bootstrap** — `SeedSkills` / `SeedUserSkills`,
   `BootstrapSelfKV`, `BootstrapSelfReflection`, `WorkflowScrub`,
   `SessionEventsScrub`, `reconcileStandingAgents`, `ResumeSessions`,
   `ResolveInstanceName`, the related-sessions subscriber.

The harness reproduces (1) with **injected** substitutes (memtest store,
ephemeral workspace, a per-case provider, a forced role) and reproduces (2)
almost verbatim, but **deliberately omits** (3): a case wants a clean, isolated
world, not resumed sessions or seeded standing agents.

So the duplication is exactly step (2) plus the *structure* of step (1). Step (3)
is genuinely production-only and should stay in `runDaemon`.

## Proposed design

Extract step (2) — and the parts of (1) that don't differ — into one function in
`internal/runtime`, parameterized by the dependencies that legitimately vary:

```go
// package runtime

// AssemblyDeps are the injectable dependencies of a wired daemon. Production
// fills these from config; the eval harness injects isolated substitutes.
type AssemblyDeps struct {
    SocketPath string
    Store      *memory.Store   // prod: cfg.DatabaseURL; harness: memtest schema
    Queue      *llm.Queue      // prod: cfg.BuildQueue; harness: per-case provider
    Plugins    *plugin.Manager
    Embedder   embed.Embedder  // may be nil
    Supervisor *Supervisor

    // Behavioral knobs (prod: from cfg; harness: per-case / defaults).
    ContextBudget      int
    SystemPrompt       string
    TaskTimeoutSeconds int
    DefaultLeafRole    string
    MaxDelegationDepth int
    PlanApproval       string
    PlanMode           string
    ApprovalTools      []string
    RelatedSessions    bool
    SurfaceMemories    bool
    MaxGoalSessions    int

    HITL     *HITL                       // nil disables human-in-the-loop
    NotifyUser func(agentID, text string) // nil disables notify_user

    // RoleOverride, when non-empty, forces every session's role — the hook the
    // eval harness uses to exercise a specific role. Empty keeps the daemon's
    // own plan-profile role resolution (production).
    RoleOverride string
}

// Assembled is the wired result: the daemon plus the handles callers still need.
type Assembled struct {
    Daemon  *Daemon
    Builder *AgentBuilder
    Sink    EventSink
}

// Assemble builds and fully wires a daemon from deps, registering the pursue /
// idle-reflection stages and starting nothing (the caller owns supervisor.Run
// and daemon.Start). It performs no production-only bootstrap (seeding, resume,
// standing agents, scrub, instance name) — those stay in runDaemon.
func Assemble(deps AssemblyDeps) (*Assembled, error)
```

`Assemble` contains today's step (2) verbatim, reading everything from `deps`.
The `RoleOverride` field folds in the harness's current factory wrapper:

```go
factory := builder.BuildForRole
if deps.RoleOverride != "" {
    factory = func(id string, p RoleParams) *agent.Loop {
        p.Role = deps.RoleOverride
        return builder.BuildForRole(id, p)
    }
}
daemon := New(deps.SocketPath, factory, ckpt, notif)
```

### Production after the refactor

`runDaemon` builds the dependencies from config, calls `Assemble`, then layers
its production-only bootstrap and starts the daemon:

```go
a, err := runtime.Assemble(runtime.AssemblyDeps{ /* from cfg */ })
// ... SeedSkills, BootstrapSelf*, reconcileStandingAgents, ResumeSessions,
//     ResolveInstanceName, AddSubscriber ...
go supervisor.Run(ctx)
a.Daemon.Start(ctx)
```

### Harness after the refactor

`Harness.Run` builds isolated dependencies and calls the same `Assemble`, with no
duplicated wiring to drift:

```go
a, err := runtime.Assemble(runtime.AssemblyDeps{
    SocketPath: sock, Store: store, Queue: llm.NewQueue(provider, 4),
    Plugins: pluginMgr, Embedder: h.Embedder, Supervisor: supervisor,
    ContextBudget: budget, SystemPrompt: runtime.BuildSystemPrompt(false),
    RoleOverride: c.Session.Role, HITL: hitl, /* ... */,
})
go supervisor.Run(dctx)
go a.Daemon.Start(dctx)
```

Now any new wiring added inside `Assemble` reaches both callers automatically. A
new *dependency* (a new `AssemblyDeps` field) still forces a decision at both call
sites — but that is a compile error, not a silent gap.

## Migration steps

1. Add `AssemblyDeps` / `Assembled` / `Assemble` to `internal/runtime`, moving the
   step-(2) wiring out of `runDaemon` unchanged. Keep `NotifyUser` and `HITL`
   nil-able (tests already rely on that).
2. Rewrite `runDaemon` to construct `AssemblyDeps` from `cfg`, call `Assemble`,
   then run the production-only bootstrap and `Start`. Diff the daemon behavior
   with an integration run — the wiring order must not change observably.
3. Rewrite `Harness.Run` to construct isolated `AssemblyDeps` and call `Assemble`,
   deleting the duplicated wiring block. Keep the isolated-store, workspace, and
   sink-drain lifecycle in the harness.
4. Run `make eval-replay` and (with Postgres) `go test ./tests/evals/...` — the
   harness self-tests already exercise the full path, so they are the regression
   gate for this refactor.
5. Once the harness calls `Assemble`, retire the drift tooling: delete
   `.claude/hooks/eval-harness-guard.sh`, unregister it from
   `.claude/settings.json`, and delete `.claude/commands/sync-evals.md` (or shrink
   `/sync-evals` to "add the new field at both `Assemble` call sites"). Update
   `tests/evals/README.md`.

## Risks & notes

- **Wiring order.** `runDaemon` wires some setters after `New` and before
  `Start`; a few have ordering constraints (e.g. `SetGoalSessionSpawnFn` needs the
  daemon; subscribers need `ConfigureMemory` first). Preserve the exact order
  inside `Assemble`; do not reorder while moving.
- **Production-only steps must stay out.** Resume, standing agents, and
  self-reflection revive/seed sessions — running them in an eval would break
  isolation. They belong to `runDaemon`, after `Assemble` returns.
- **`AssemblyDeps` is the new coupling surface.** Its fields are the things that
  legitimately differ between prod and evals. Adding one is the deliberate, typed,
  compiler-enforced act this refactor is designed to produce — the opposite of
  today's silent drift.
- **Scope.** This is a pure internal refactor: no wire-protocol, config-shape, or
  CLI change, so no `spec/` contract moves and no `plugin.ProtocolVersion` bump.
  Reconcile via `/sync-nine` as a `docs/` update only.
