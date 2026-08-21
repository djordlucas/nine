# Architecture — wiring reference

How Nine's object graph is assembled and how a turn travels through it, at the
level of concrete components and call order.

This is the implementation-facing companion to
[architecture.md](../docs/architecture.md), which describes the same system in
terms of what the pieces are and the rules they keep. That document should be
readable without the source open; this one deliberately is not — it names types
and call sites, because tracing a boot order or a data flow is exactly the task
where you want them.

Because it names code, it drifts faster than prose does. Where the two
disagree, the source is right.

---

## 16. End-to-end data flows

### A. Interactive user turn

```
 TUI ── user_turn{agentID,text} ──► daemon.dispatch ──► userTurn
                                                          │
   register progressFn (→ progressCh, cap 256)            │
   r.turnAsync(text) ──► AgentWorker.inbox                │
                              │                            │
   ┌──── connection goroutine selects ────┐               │
   │  progressCh → enc.Encode(event)  ◄────┼── emitEvent ◄─┤  worker goroutine:
   │  respCh     → final response + done   │               │  processTurn → loop.Run
   └────────────────────────────────────────┘             │     ├─ thinking
                                                           │     ├─ context_update
   loop.Run inner iterations:                              │     ├─ tool_start
     build context → queue.Submit → LLM                    │     ├─ tool_end
     dispatch tool calls → scratchpad                      │     ├─ thinking_chunk (reasoning trace)
                                                           │     └─ response_chunk
     final answer ─────────────────────────────────────────┘
                                                           ▼
                              notifyStages → checkStall → checkpoint → armIdleTimer
                                                           │
                              respCh ◄────────────────────┘
 TUI ◄── response{text} ── done ──────────────────────────
```

### B. Idle reflection (no client involved)

```
 idleTimer fires (2 min) ──► handleIdle
        │  find "idle-reflection" routine, interval elapsed
        ▼
   OnIdle() returns the reflection prompt, ok=true
        ▼
   processTurn(prompt)  ── same path as a user turn, Priority=Background ──► LLM
        ▼
   model calls memory_set self/capabilities, self/learned
        ▼
   the turn's result is recorded in the journal
        ▼
   armIdleTimer (re-arm for the next cycle)
```

### C. Sub-agent fan-out (`run_agents`)

```
 parent loop dispatches run_agents([t1,t2,t3], timeout)
        │  registerSubAgentTools → spawnOne per task
        ▼
   ctx, cancel = WithTimeout(≥300s)
   go spawnOne(t1) ─┐
   go spawnOne(t2) ─┼─► each: emit sub_agent_start (→ parent progress feed)
   go spawnOne(t3) ─┘           build child loop in its leaf role, depthGuard-1
                                RunSubAgentSync(child)
                                emit sub_agent_end{status}
        ▼  WaitGroup.Wait()
   results[] returned to the parent loop as the tool observation
```

---

## 17. Startup sequence

`runDaemon` (`cmd/nine/daemon.go`) wires the object graph in this order:

```
 1.  config load  +  ApplyEnvOverrides
 2.  memory.Open(cfg.DatabasePath())                ← the single SQLite store (fail-fast)
 3.  plugin.NewManager + TryStartBuiltin(shell)
     + startMCPServers([[mcp.server]])              ← one bridge each, before user plugins
     + LoadUserPlugins([plugins].user_dir)          ← after the built-ins; names reserved
 3a. OpenSandboxedTools(cfg, store, mgr)            ← nil unless [tools] enabled;
     → toolvm.Open (compile QuickJS) → SetAgentConfig → Load → LoadGeneratedTools
     after the plugins, so a colliding sandboxed tool is skipped, not honored
 3b. NewGeneratedToolStore(store, host, mgr, NewDepsBundler(cfg), …)
     ← the tool_write/tool_delete/js_eval backend; inert when [tools.agent] is off
 4.  NewStores(store) → checkpoint, notif, notifAdd
 5.  embed.Build(...)                               ← embedder (keyword default)
 6.  NewSupervisor(64)  +  supervisor.Attach(store) ← durable, journal-backed bus
 7.  selfmodel.New(store, embedder, listPlugins)    ← self-model assembler
 8.  BootstrapSelfKV(store, plugins)                ← seed self/identity, self/capabilities
 9.  StageRegistry["idle-reflection"] = …  +  BootstrapSelfReflection(2 min)
10.  StageRegistry["pursue"] = …
11.  store.WorkflowScrub()  +  store.SessionEventsScrub(turns, age)  ← bound workflow + journal growth
12.  NewHITL(store, timeout)  +  hitl.ExpireStale()
13.  NewAgentBuilder(AgentBuilderConfig{Loop{…, RelatedSessions}, …})
14.  runtime.New(socket, agentBuilder.BuildForRole, ckpt, notif)   ← the daemon
15.  NewSQLEventSink(store, daemon.NotifySubscribers) + daemon.SetEventSink   ← journal writer
16.  daemon.Configure{HITL, Memory, Plugins, Supervisor, PlanStore}
     daemon.SetMaxGoalSessions
17.  if related_sessions_index (default on) && embedder: daemon.AddSubscriber(RelatedIndexer)
18.  agentBuilder.SetGoalSessionSpawnFn(daemon.SpawnGoalSession)   ← goal-spawning roles only
     agentBuilder.SetEmitProgressFn(daemon.EmitProgress)          ← sub-agent events
19.  go supervisor.Run(ctx)
20.  reconcileStandingAgents(...)                   ← seed config-owned [[agent]] goals
21.  daemon.ResumeSessions(ctx)                     ← restart idle-capable sessions
22.  daemon.Start(ctx)                              ← accept loop (blocks)
```

The ordering matters: the builder is constructed before the daemon, but the
goal-spawn and progress-emit functions are injected *after* the daemon exists
(they close over the daemon's session registry), and only then are sessions
resumed.

---

## 18. Component relationship map

```
                         cmd/nine (main, daemon)
                                  │ wires
                                  ▼
   ┌──────────────────────── runtime.Daemon ────────────────────────┐
   │  factory ─────────────► runtime.AgentBuilder ──builds──► agent.Loop
   │  sessions[] ──────────► runtime.AgentWorker ──wraps────► agent.Loop
   │  plans ───────────────► PlanStore / sessionPlanState ─► StageHandler
   │  sup ─────────────────► runtime.Supervisor
   │  mgr ─────────────────► plugin.Manager ──spawns──────► plugin subprocs
   │  tools ───────────────► toolvm.Host ──instantiates──► wazero (in-process)
   │  ckpt / notif / store ► memory.Store  (sole handle → SQLite file)
   │  sink ─────────────────► EventSink ──► session_events journal
   │  subscribers[] ────────► RelatedIndexer (out-of-band, cursor-backed)
   └─────────────────────────────────────────────────────────────────┘
            │                       │                      │
            ▼                       ▼                      ▼
     agent.Dispatcher       ninectx.Builder          llm.Queue ──► llm.Provider
     (tool routing)         (context budget)         (priority)    (ollama)
            │
            ├─ plugin tools  ──► plugin.Manager.Call
            ├─ core tools    ──► in-process handlers
            │                    (memory, embed, run_agent,
            │                     workflow_*, goal_*, gap_report)
            └─ sandboxed     ──► toolvm.Host.Call
                                 (developer tools.d + generated store rows)

   embed.Embedder ──► used by: ninectx tool ranking, skill search,
                                memory_query / file_search_semantic,
                                selfmodel.Assembler
```

Dependency direction is acyclic and downward: `cmd` → `runtime` →
{`agent`, `plugin`, `memory`, `llm`, `context`, `selfmodel`, `embed`,
`workflow`, `toolvm`}. The `protocol` package is shared by both the daemon and
the client/TUI but depends on neither, so client code never pulls in the runtime.
`agent` reaches the sandbox only through the two-method `SandboxedHost`
interface — it imports `toolvm` for the `Tool` type but never touches the wazero
API itself, which is what lets a dispatcher test substitute a fake host with no
wasm runtime in it.

---
