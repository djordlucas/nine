# Nine — Architecture

This document walks the actual runtime: the process topology, the goroutine model,
the precise lifecycle of a turn, how state is assembled and budgeted, and how every
major component is wired together at boot. Function and type names refer to real
symbols in the tree, so this doubles as a map for reading the source.

§1 is the bird's-eye view; read it alone for the shape of the system, and the rest
when you need the mechanism.

Cross-references: [glossary.md](glossary.md) for term definitions,
[agent-loop.md](agent-loop.md), [daemon.md](daemon.md),
[context-builder.md](context-builder.md), [session-plans.md](session-plans.md),
[plugins.md](plugins.md), [sandboxed-tools.md](sandboxed-tools.md).

---

## 1. System topology

Nine is a **daemon/client pair**. A single long-lived daemon process owns all
state; the `nine` binary is both the thin client and the daemon (it re-execs
itself with a hidden subcommand). Plugins are independent child processes. The
LLM endpoint is the only external dependency Nine always has; each declared
`[[mcp.server]]` adds another, because the bridge reaches a server that is
fetched (`npx`) or hosted elsewhere (§10).

```
   ┌──────────────────────────────────────────────────────────────────────┐
   │  HOST / CONTAINER                                                      │
   │                                                                        │
   │   ┌─────────────────┐         ┌─────────────────┐                      │
   │   │  nine (TUI)     │         │  nine "msg"     │   ← same binary,     │
   │   │  interactive    │         │  one-shot CLI   │     client mode      │
   │   └────────┬────────┘         └────────┬────────┘                      │
   │            │ newline-delimited JSON     │                              │
   │            │ over AF_UNIX socket        │                              │
   │            └─────────────┬──────────────┘                              │
   │                          ▼                                             │
   │           ┌──────────────────────────────┐                            │
   │           │   DAEMON  (nine, daemon mode) │                            │
   │           │   /tmp/nine.sock              │                            │
   │           │                               │                            │
   │           │   Daemon ─ AgentWorker(s)     │                            │
   │           │     │         │               │                            │
   │           │     │         ├─ agent.Loop   │                            │
   │           │     │         └─ SessionPlan  │                            │
   │           │     │                         │                            │
   │           │   LLM Queue ── Provider ──────┼──────►  LLM endpoint       │
   │           │   Supervisor                  │        (Ollama)            │
   │           │   Plugin Manager              │                            │
   │           │   toolvm.Host ── wazero       │                            │
   │           │     (in-process, no subproc)  │                            │
   │           └───────┬───────────────┬───────┘                            │
   │                   │ HTTP over     │ database/sql (modernc sqlite)      │
   │                   │ unix socket   │                                    │
   │      ┌────────────┴───────┐       ▼                                    │
   │      ▼     ▼     ▼     ▼   ▼   ┌──────────────────┐                     │
   │   shell files http time  mcp:* │  SQLite (one file)                    │
   │   (plugin subprocesses)        │  nine database   │                    │
   │                                └──────────────────┘                    │
   └──────────────────────────────────────────────────────────────────────┘
```

`memory`, `files` (durable store), and `skills` are **in-process** capabilities
of `memory.Store`, not plugin subprocesses. The plugin subprocesses are `shell`,
`files` (workspace filesystem `read_file`/`write_file`), `http`, `time`, one
`mcp` bridge per declared `[[mcp.server]]`, and any user plugin.

**Sandboxed tools** (`toolvm.Host`, §11) are in-process too, but for the opposite
reason: not because they are trusted, but because a wasm module needs no process
of its own to be contained. JS and wasm tools from `[tools].user_dir` and from
the store execute inside the daemon under a wazero sandbox with only the
capabilities `nine.toml` conferred. The host is **nil** unless `[tools] enabled`
is set, which is the shipped default.

Key consequences of this shape:

- **All durable state is in the daemon + its SQLite file.** Clients are disposable.
  Closing a TUI does not stop work; reattaching replays what was missed.
- **Plugins are isolation boundaries.** A crashing or hanging plugin is a child
  process, not a daemon panic. Tools are reached only through the manager.
- **There are two kinds of isolation, and they are not the same kind.** A plugin
  is isolated for *reliability* — it is a separate process, but it runs with the
  daemon's own reach. A sandboxed tool is isolated for *authority* — it shares
  the process, but has no filesystem, no network, and no environment except what
  was conferred.
- **The LLM is behind a queue.** No agent ever calls the provider directly; the
  queue is the single choke point for concurrency and prioritization.

---

## 2. Process & deployment model

| Process | Binary | Role | Lifetime |
|---------|--------|------|----------|
| Client  | `nine` | TUI or one-shot request; connects to socket | Per invocation |
| Daemon  | `nine` (re-exec) | Owns sockets, sessions, queue, plugins, DB connection | Long-lived |
| Plugin  | `bin/<name>`, or `nine` (re-exec) for the built-ins | One tool provider, HTTP over a unix socket | Spawned by daemon, killed on stop |
| MCP bridge | `nine` (re-exec) | One declared `[[mcp.server]]`: the plugin contract to the daemon, stdio or HTTP to the server | Spawned by daemon, killed on stop |
| Sandboxed tool | *(none)* | JS/wasm in a wazero instance inside the daemon | One instance per call, closed on return |

The CLI auto-starts the daemon if the socket is dead (`EnsureDaemon` in
`internal/protocol/client.go`). Everything funnels through `cmd/nine/main.go`,
which dispatches to either the TUI, the one-shot client, or `runDaemon`
(`cmd/nine/daemon.go`).

### Volume layout (Docker)

```
/data                 mutable state only (the "nine-data" volume)
├── nine.db      the SQLite database (+ its -wal/-shm sidecars)
└── workspace/   files-plugin working directory

/opt/nine             immutable image content (not in a volume)
└── bin/         empty by default — the built-in plugins live in the nine
                 binary; a user plugin's binary can be mounted here

/tools.d              developer sandboxed tools, bind-mounted (manifest + .js/.wasm)
                      inert unless the mounted nine.toml sets [tools] enabled
```

Primary state is the SQLite file on that same `/data` volume, so one volume carries
the database and the workspace together (see
[Single-container Nine](../adr/single-container.md)). Point the daemon elsewhere with
`[memory].path` or `NINE_DB_PATH`; with no override it resolves to `/data/nine.db`
whenever that volume is present, and `~/.nine/nine.db` natively. Note that a backup
must capture the `-wal` and `-shm` sidecars alongside `nine.db`, or use
`VACUUM INTO`.

Built-in skills are embedded in the `nine` binary (`//go:embed` in the `nine/skills`
package) and seeded into the `skills` table on every boot — there is no skills
directory in the image or the volume.

Developer sandboxed tools are the one exception to "image content is immutable":
`[tools].user_dir` is mounted at `/tools.d` and read at boot and on
`nine tools reload`. The QuickJS interpreter they execute on is *not* built there
— it is a pre-built wasm artifact committed to the repo and compiled into the
binary, which is why the runtime image still carries no toolchain. Generated
tools need no path at all; they live in the database on `/data`.

Config is resolved in order: `$NINE_CONFIG` → `./nine.toml` → `/nine.toml`
(Docker bind-mount) → `~/.nine/nine.toml`.

---

## 3. The daemon: socket server & message router

`runtime.Daemon` (`internal/runtime/daemon.go`) is a Unix-socket server. Its
core state:

```
Daemon
 ├─ socketPath   string
 ├─ factory      LoopFactory          // agentID → *agent.Loop  (AgentBuilder.Build)
 ├─ ckpt         CheckpointStore      // save/load serialized loop state
 ├─ notif        NotifStore           // pending notifications per agent
 ├─ sessions     map[string]*AgentWorker   ← the live session registry (RWMutex)
 ├─ names        map[string]string         // agentID → display name
 ├─ mgr          *plugin.Manager      // tool listing / direct calls
 ├─ store        queryBackend         // goal/reflection/workflow read proxy
 ├─ plans        PlanStore            // session_plans persistence
 └─ sup          *Supervisor          // event sink for stalls / gaps
```

Every client connection is handled by one goroutine (`handleConn`), which reads
newline-delimited JSON and routes each `protocol.Msg` through `dispatch`:

```
              client connection (one goroutine per conn)
                          │
                bufio.Scanner → protocol.Msg
                          │
                     d.dispatch(msg.Type)
   ┌──────────────────────┼─────────────────────────────────────┐
   │ new_conversation  → newConversation() → makeAgentWorker     │
   │ attach            → attach() (+ replay snapshot)            │
   │ user_turn         → userTurn() ───────────► AgentWorker     │
   │ status            → handleStatus()                         │
   │ list_goals/…      → handle* (read-only DB proxy)           │
   │ workflow_stop/fail→ store.WorkflowCancel / WorkflowFail    │
   │ list_tools        → handleListTools()                      │
   │ plugin_call       → handlePluginCall() (bypass the LLM)    │
   └────────────────────────────────────────────────────────────┘
```

The daemon is a **router, not a brain**: handlers like `list_goals` are thin
read proxies over the shared store (`queryBackend`). The daemon never
interprets goal/workflow data — that logic lives in the agent loops and the
domain services.

---

## 4. Concurrency model (goroutine topology)

This is the part the high-level doc glosses over. At steady state the daemon
runs this set of goroutines:

```
                         ┌─────────────────────────────┐
                         │  Daemon.Start accept loop    │  (1)
                         │  net.Listener.Accept()       │
                         └──────────────┬───────────────┘
                                        │ go handleConn(conn)
                  ┌─────────────────────┼─────────────────────┐
                  ▼                     ▼                     ▼
          handleConn #1          handleConn #2          handleConn #N   (per client)
                  │  (during a user_turn, registers a progressFn
                  │   and selects on progressCh + respCh)
                  ▼
   ┌──────────────────────────────────────────────────────────┐
   │  AgentWorker.run()   — ONE goroutine per session          │  (per agentID)
   │  select {                                                 │
   │    case req := <-inbox:   processTurn(req)   ← serialized │
   │    case <-idleTimer.C:    handleIdle()                    │
   │  }                                                        │
   └──────────────────────────────────────────────────────────┘
                  │ processTurn → loop.Run → queue.Submit
                  ▼
   ┌──────────────────────────────────────────────────────────┐
   │  llm.Queue                                                │
   │   go run(item)  — up to maxConcurrent of these at once    │  (bounded)
   └──────────────────────────────────────────────────────────┘

   ┌──────────────────────────────────────────────────────────┐
   │  Supervisor.Run()   — single event-handling goroutine     │  (1)
   └──────────────────────────────────────────────────────────┘

   sub-agents: each run_agents fan-out spawns one goroutine per child task,
   each running a fresh agent.Loop synchronously (RunSubAgentSync).
```

The crucial invariant:

> **One session = one goroutine = strictly serialized turns.**

`AgentWorker.inbox` is a **buffered-1 channel**. A session processes exactly one
turn at a time; the agent loop within it is explicitly *not* safe for concurrent
use (`agent.Loop` doc comment), and the worker goroutine is what guarantees that
safety. Idle-triggered turns go through the *same* `processTurn`, so an idle
reflection and a user turn can never run concurrently on the same session.

Cross-session parallelism (two different conversations, or a conversation plus
its pursue session) is real, but it is bounded downstream by the LLM queue's
`maxConcurrent`. For a local Ollama model you set `max_concurrent = 1` and the
queue serializes *everything*, ordered by priority.

---

## 5. The AgentWorker — session lifecycle

`runtime.AgentWorker` (`internal/runtime/agent_worker.go`) wraps one
`agent.Loop` and is the unit of session liveness. It is used identically for
interactive conversations, the self-reflection session, goal pursue sessions,
and (indirectly) checkpoints.

```
AgentWorker
 ├─ id          string
 ├─ loop        *agent.Loop
 ├─ inbox       chan turnReq        (cap 1)   ← turns enter here
 ├─ stopped     chan struct{}                 ← closed when run() exits
 ├─ saveCkpt    func(id, data)                ← checkpoint persistence
 ├─ getNotif    func(id) []string             ← pending notifications
 ├─ stall       StallConfig {Limit, OnStall}
 ├─ stallN      int                           ← consecutive no-tool turns
 ├─ plan        *sessionPlanState             ← stages + persistence
 ├─ idleTimer   *time.Timer                   ← per-routine idle scheduler
 ├─ idleSince   map[stage]time.Time
 ├─ replay      replayBuffer (ring, cap 200)  ← for reattach
 └─ progressFn  func(protocol.Msg)            ← set during a live turn
```

### Turn lifecycle (`processTurn`)

```
 turnReq arrives on inbox
        │
        ▼
 turnN++ ; replay.clearResponse()
        │
        ▼
 prependNotifications(text)        ← pull pending notifications, splice in
        │
        ▼
 wire loop callbacks → emitEvent:
     OnContextUpdate / OnToolStart / OnToolEnd / OnChunk / OnThinking
        │
        ▼
 result, err = loop.Run(ctx, text) ─────────► (see §6 ReAct loop)
        │   (streams tool_start/tool_end/response_chunk/… as it goes)
        ▼
 unwire callbacks
        │
        ▼
 notifyStages(result, err)         ← StageHandler.OnTurnEnd for each active stage
        │                            then persist/refresh session_plans row
        ▼
 checkStall(ctx)                   ← if LastRunToolCount()==0, stallN++
        │                            at Limit → OnTurnEnd(ErrStall) + OnStall
        ▼
 checkpoint()                      ← loop.SaveState() → ckpt.Save(id, data)
        │
        ▼
 armIdleTimer()                    ← recompute next idle wake-up
        │
        ▼
 respCh <- {result, err}           ← unblocks the waiting handleConn
        │
        ▼
 onComplete(id)                    ← Supervisor EventAgentCompletes
```

### Progress streaming & the replay buffer

During a turn, `userTurn` registers a `progressFn` that pushes every event onto
a buffered (cap 256) `progressCh`; the connection goroutine `select`s on that
channel and the response channel, forwarding events to the client as they
arrive. This is how the TUI shows tool calls live.

Simultaneously, `emitEvent` pushes `tool_start`/`tool_end`/`sub_agent_*` events
into a **bounded ring buffer** (`replayBuffer`, cap 200, sends up to 50 on
attach) and records the last completed response. When a client reattaches
(`attach`), the daemon hands back a snapshot so a reconnecting TUI can redraw
the recent history and any answer it missed while disconnected.

```
 loop event ──► emitEvent ──┬──► progressFn (live, if a client is attached)
                            └──► replay.push (always, for later reattach)
```

---

## 6. The agent loop (ReAct)

`agent.Loop` (`internal/agent/loop.go`) implements Reason → Act → Observe. One
`Run` call = one user turn = possibly many LLM round-trips (inner loop).

```
Run(ctx, userText):
  history += {user, userText}
  scratchpad = []
  queryVec = embed(userText)              ← embedded ONCE, reused all iterations
  selfModel = SelfModelFn(queryVec)       ← assembled ONCE

  ┌────────────────────── inner loop ──────────────────────┐
  │ llmCallN++ ; onThinking(llmCallN)                       │
  │                                                         │
  │ req = builder.BuildWithUsage({                          │
  │     SystemCore (+ current time), SystemExtras,          │
  │     SystemSelf, Tools, QueryVector,                     │
  │     History, Scratchpad })                              │
  │ onContextUpdate(used, budget)                           │
  │                                                         │
  │ resp = queue.Submit(ctx, Priority, req)  ──► LLM        │
  │                                                         │
  │ if resp.ToolCalls is empty:                             │
  │     answer = resp.Text or emptyAnswerFallback(toolErrs) │
  │     history += {assistant, answer}                      │
  │     scratchpad = []                                     │
  │     return answer            ◄── TURN COMPLETE          │
  │                                                         │
  │ for each toolCall tc:                                   │
  │     onToolStart(tc.Name, displayName, tc.Input)         │
  │     result, _, err = dispatchWithRetry(tc) ──► §7       │
  │     observation = result.Output or failure note         │
  │     onToolEnd(...)                                       │
  │     scratchpad += {thought, tc, observation}            │
  │                                                         │
  │ (loop back: rebuild context WITH the new scratchpad)    │
  └─────────────────────────────────────────────────────────┘
```

Salient details:

- **The scratchpad is the working memory of a turn.** Each entry expands into an
  assistant message (the thought + tool call) and a user message (the tool
  result) when the context is rebuilt — so the model sees its own prior actions
  on the next iteration. It is cleared on the final answer and folded into
  `history`.
- **`dispatchWithRetry`** retries a failing tool up to `maxToolRetries` (2) more
  times, returning `(CallResult, time.Duration, error)`.
- **Empty responses are retried, then surfaced.** A response with no text and no
  tool calls is a failed sample rather than an answer — common on small local
  models right after a tool observation — so the loop re-issues the call up to
  `maxEmptyAnswerRetries` (2) more times. If it is still empty,
  `emptyAnswerFallback` turns the accumulated tool errors into a visible message
  instead of returning blank.
- **Checkpointing happens outside the loop**, in the worker, after `Run`
  returns. `ConversationState{History, Scratchpad}` is the serialized unit.

---

## 7. Tool dispatch & the tool taxonomy

`agent.Dispatcher` (`internal/agent/dispatcher.go`) is a name → handler map with
post-call hooks and a hard output cap.

```
Dispatch(toolName, args):
   fn = handlers[toolName]            ← unknown tool → error
   callArgs = expandRefs(args)        ← x-nine-ref params: path → stored content
   output, err = fn(ctx, callArgs)
   if err: return err
   for h in hooks[toolName]: h(toolName, args, output)   ← e.g. embed a new skill
   return capOrSpill(output)          ← over 2048 tokens × 4 chars: store it,
                                        return a head+tail preview + the path
```

Tools come in three flavors, all appearing in the same LLM tool list and all
sharing **one namespace** — a name resolves to exactly one backend, and a
collision is a load failure, not a silent override:

```
   ┌─────────────────────────────────────────────────────────────┐
   │  TOOL CALL from the model                                    │
   └───────────────────────────┬──────────────────────────────────┘
                               ▼
                       Dispatcher.handlers[name]
       ┌───────────────────────┼───────────────────────┐
       ▼                       ▼                       ▼
 PLUGIN TOOLS          CORE-INTERCEPTED         SANDBOXED TOOLS
 (RegisterPlugin)      (Register* in            (RegisterSandboxed)
 handler =             builder.go)              handler = host.Call(name, …)
   m.Call(plugin, …)   in-process, no           → wazero instance, in-process,
 → HTTP plugin.call      subprocess:              capabilities only (§11)
                        • gap_report
 shell, read_file,      • memory_embed /        tools.d/*.js|.wasm  (developer)
 write_file, http_get,    memory_query          store rows          (generated)
 web_search, skill_*,   • file_search_semantic  tool_write / tool_delete /
 time, mcp tools, …     • run_agent/run_agents    js_eval are themselves core
                        • workflow_* / goal_*
```

The three differ in *isolation*, which is the reason to have three: a plugin is
a separate process with the daemon's own reach, a core tool is a Go function
with the daemon's own reach, and a sandboxed tool is in-process but structurally
unable to touch anything the operator did not confer.

### Role-gated registration

Tool registration is **role-aware** (`AgentBuilder.build(agentID, role, depthGuard)`;
see [Roles](roles.md)). The role's `Delegates`/`SpawnsGoals` flags and tool allowlist
govern what is registered, with `depthGuard` (default 2, decremented per spawn) as the
recursion backstop. For the default roles the delegation surface by depth is:

```
 orchestrator (top-level conversation)  executor (guard 1)      executor (guard 0)
 ─────────────────────────────────────  ───────────────────     ──────────────────
 plugin tools             ✓             plugin tools      ✓      plugin tools  ✓
 core memory/file tools   ✓             core mem/file     ✓      core mem/file ✓
 gap_report               ✓             same              ✓      same          ✓
 run_agent/run_agents     ✓             run_agent         ✓      run_agent     ✗
 workflow_* / goal_*      ✓             workflow/goal     ✓      workflow/goal ✗
 goal pursue-session spawn ✓ (SpawnsGoals)                ✗                    ✗
```

Delegation tools are gated behind `role.Delegates && depthGuard > 0`, preventing
infinite sub-agent recursion; coarse leaf roles (`software-dev`, `sysadmin`,
`report-writer`) cannot delegate at all. Only goal-spawning roles (the orchestrator)
get the goal-session spawn function, so only top-level goals get background pursue
sessions. Allowlist roles are additionally pruned at both the advertised tool list and
the dispatcher handler set.

---

## 8. Context assembly & token budgeting

`ninectx.Builder` (`internal/context/builder.go`) packs one LLM request into a
fixed token budget (`context_budget`, defaults to `num_ctx`). Token counting is
a deliberate approximation: **4 characters ≈ 1 token**, no tokenizer dependency.

Allocation is strictly by priority — higher priorities are subtracted from the
budget first; lower ones get whatever remains:

```
   budget = context_budget
   ┌──────────────────────────────────────────────────────────────┐
   │ P1  System core         ALWAYS included (current time + prompt)│  ── budget
   ├──────────────────────────────────────────────────────────────┤
   │ P2  Tool definitions    relevance-filtered (top-N, default 20) │  ── budget
   │       always-include: memory/file + intercepted core tools     │
   ├──────────────────────────────────────────────────────────────┤
   │ P2.5 Self-model         capped at 600 tokens, else dropped     │  ── budget
   ├──────────────────────────────────────────────────────────────┤
   │ P3  Message history     keep newest, trim oldest (trimFront)   │  ── budget
   ├──────────────────────────────────────────────────────────────┤
   │ P4  Scratchpad          keep newest entries, trim oldest       │  ── budget
   ├──────────────────────────────────────────────────────────────┤
   │ P5  System extras       included only if ≥ extrasBudget (200)  │  ── leftover
   └──────────────────────────────────────────────────────────────┘
```

### Tool relevance filtering

```
   queryVec = embed(user query)
   for each tool:  score = cosineSim(queryVec, toolVector)   (0 if no embedding)
   sort:  always-include first, then by descending score
   take:  all always-include tools
        + top-N others that still fit the remaining budget
```

This keeps the prompt compact when dozens of plugin tools are loaded: the small
fixed set (memory, file, and intercepted core tools) is always present so the
agent never "loses" its fundamental capabilities, and the rest are ranked by
relevance and capped at top-N. The `AgentBuilder` populates each `toolVector` by
embedding the tool's `name: description` once and caching it by name (tool
descriptions are static after boot), so the descending-score ordering is live
whenever an embedder is configured; with `provider = "none"` the vectors are nil
(every tool scores 0) and the cap plus the always-include set shape the list.
These per-tool vectors live in memory on the builder — they are **not** stored in
the `vectors` table (which holds skills, `session-index`, and agent embeddings).

---

## 9. The LLM queue

`llm.Queue` (`internal/llm/queue.go`) is a **priority min-heap** in front of the
provider, bounding in-flight calls to `maxConcurrent`.

```
   Submit(ctx, priority, req)
        │
        ▼
   inflight < maxConcurrent ?
     ├── yes → inflight++ ; go run(item)  ─────────► provider.Complete
     └── no  → heap.Push(pending, item)   (waits for a slot)

   run(item) finishes:
        inflight--
        if pending not empty:
            next = heap.Pop(pending)   ← lowest priority value wins
            inflight++ ; go run(next)
```

Priority constants (`internal/llm/provider.go`), lower = served first:

| Value | Constant | Who |
|-------|----------|-----|
| 1 | `PrioritySupervisor` | supervisor agent |
| 2 | `PriorityConversation` | active user conversations (someone is waiting) |
| 3 | `PriorityBackground` | tasks, goals, reflection, pursue sessions |

So when a slot frees up, a waiting user turn always jumps ahead of a queued
background reflection. The provider interface itself is tiny — a single
`Complete(ctx, Request) (Response, error)` — so adding a backend is a one-method
job. Streaming is delivered through the request's `OnChunk` callback, not a
separate method.

---

## 10. Plugin subsystem

`plugin.Manager` (`internal/plugin/manager.go`) owns plugin subprocess
lifecycle. A native plugin speaks a small two-method protocol (`plugin.describe`,
`plugin.call`) over **HTTP on a per-plugin Unix socket** (`NINE_PLUGIN_SOCKET`,
`POST /rpc`): the manager spawns the process, waits for the socket, and drives it
with an `http.Client`, which gives free per-request concurrency and
context-based cancellation (docs/plugins-http-transport.md). There is no second
transport on the daemon's side: an external **MCP** server is a plugin too,
reached through the `mcp` bridge, which speaks this same contract to the daemon.
How the bridge reaches the server is the part that varies — `dialSpec`
(`internal/builtins/mcp.go`) opens HTTP when the `[[mcp.server]]` declares a
`url`, and spawns the `command` over stdio otherwise. Everything past that dial
is transport-agnostic: one `mcpConn`, one handshake, one tool-prefixing rule.

The Go default plugins are not separate executables: their handlers live in
`internal/builtins`, and `Manager.StartBuiltin` spawns them by re-executing the
nine binary as `nine plugin serve <name>`. That is a packaging difference only —
each still gets its own process, socket, sanitized environment, and crash
isolation.

```
   Manager.Start(binaryPath, extraEnv…)        (user plugins)
   Manager.StartBuiltin(name, extraEnv…)       (shell/files/http/time)
   Manager.StartBuiltinInstance("mcp", …)      (one per [[mcp.server]])
        │  spawn process with NINE_PLUGIN_SOCKET (+ NINE_BIN, extra env)
        │  wait for the socket, then use an http.Client on POST /rpc
        ▼
   ─► POST /rpc {method:"plugin.describe"} ───────────►  plugin
   ◄─ {result:{protocol_version, max_concurrent, tools:[…]}} ◄──  plugin
        │
        ▼  Manager tracks the *Plugin{Name, client, Tools}
        ▼
   Dispatcher.RegisterPlugin → handlers[tool] = m.Call(p, tool, args)
        │
        │  on each tool invocation during a turn (one HTTP request each):
   ─► POST /rpc {method:"plugin.call", params:{tool, args}} ─►  plugin
   ◄─ {result:{output}}  (or {error:{code,message}}) ◄────────  plugin
```

Plugins are fixed: there is no runtime generation, build, or hot-swap. Each is
compiled into the image at build time and started at daemon boot:

```
   go build (image build) ──► TryStartBuiltin (daemon boot) ──► (in use) ──► SIGTERM
   the nine binary            spawn `nine plugin serve <name>`,  plugin.call  (shutdown)
                              describe, register
```

An MCP server is the exception to "compiled into the image": nothing is built for
it, and the bridge reaches a server that is fetched (`npx`) or hosted elsewhere.
The bridge process itself is still `nine plugin serve mcp`, so the lifecycle above
is unchanged from the daemon's side. [Browser automation](browser.md) is the
worked example: Playwright's MCP server declared in `nine.toml`, no plugin
written and nothing added to the image.

Tool definitions are registered with the dispatcher at start time. (Their
description embeddings for context-builder relevance ranking are computed lazily
and cached by the `AgentBuilder` on first use — in memory, not in the `vectors`
table.) A crashed
subprocess is isolated from the daemon; restart from the existing binary is the
manager's responsibility.

Default plugins started at boot: `files`, `shell`, `http`, `time` — then one
`mcp` bridge per `[[mcp.server]]`, then user plugins.
Memory/file/vector operations and the skill tools are **core-intercepted**
(handled in-process), not a subprocess.

---

## 11. Sandboxed tools — the in-process wasm host

`toolvm.Host` (`internal/toolvm/host.go`) is the dispatcher's **third backend**,
and the only one that is neither a subprocess nor a core handler: JS and wasm
tools run **inside the daemon process**, in a wazero sandbox, with exactly the
capabilities the operator conferred — by default, none.

The subsystem is additive by construction. `runtime.OpenSandboxedTools`
(`internal/runtime/sandboxed.go`) returns **nil** unless `[tools] enabled` is
set, every consumer downstream treats a nil host as "no sandboxed tools", and a
failure to open the wasm runtime is logged and degraded to nil rather than
aborting the boot — an operator whose sandbox will not start should lose the
tools, not the daemon.

### Host lifecycle

```
 OpenSandboxedTools(cfg, store, pluginManager)       ← nil when [tools] disabled
   │
   ├─ toolvm.Open(Config{UserDir, Grants, Timeout, MemoryMB, TouchGenerated})
   │     compiles the QuickJS-NG blob ONCE (~1 MB of wasm — the expensive step)
   │     wazero runtime: no FS mounted, no env passed, no network to configure
   │
   ├─ host.SetAgentConfig(agentConfig(cfg))   ← [tools.agent]: on/off, ceiling, cap
   ├─ host.Load(ctx, pluginCollides(mgr))     ← walk [tools].user_dir manifests
   └─ LoadGeneratedTools(store, host, mgr)    ← project the stored catalog in
```

The ordering is load-bearing twice. The host opens **after** the plugin manager,
so every plugin tool name is already reserved and a colliding sandboxed tool is
*skipped* rather than allowed to override (`pluginCollides`); and
`SetAgentConfig` runs **before** the first `LoadGenerated`, which refuses to
register anything while the generated tier is off. Compilation happens once per
tool; instantiation happens once per **call**.

| Kind | Module | ABI |
|------|--------|-----|
| `js` | the shared QuickJS blob + the tool's source | default-exported function, ES2023 only |
| `wasm` | the tool's own compiled module | `nine_alloc` / `nine_run`, UTF-8 JSON in and out |

### One call

```
 Dispatcher.handlers[name] ──► Host.Call(ctx, name, args)
        │
        ├─ fresh wazero instance from the compiled module   ← ONE PER CALL
        │     no globals, no cache, no credential survives it
        ├─ ctx deadline = [tools].timeout (default 5s)
        │     wazero has no fuel metering, so this is the ONLY CPU bound
        ├─ linear memory capped at [tools].memory_mb (default 16 MiB, 256 pages)
        ├─ stdout/stderr → io.Discard, argv denied wholesale (no argv in a call)
        └─ on return: instance closed, TouchGenerated(name) recorded for LRU
```

A returned string reaches the model untouched, anything else is
JSON-stringified, and a throw becomes an ordinary tool failure the model can
retry. Output then passes through the dispatcher's usual `capOrSpill` (§7), so a
sandboxed tool is bounded on the way out like any other.

### Capabilities are conferred, never claimed

Resolution is a two-sided exact match: the manifest **declares** a need
(`toolvm.Declaration`), `nine.toml` **grants** it (`toolvm.Grant`), and either
side alone is a load failure — declaring something ungranted fails, and being
granted something undeclared *also* fails. Both are loud by design.

| Capability | Mechanism | Default |
|------------|-----------|---------|
| `clock`, `random`, `log` | host functions | granted |
| `fs.read` / `fs.write` | wazero pre-opened directories, addressed by *guest* path | declare + grant |
| `env` | named keys only; `Config.Validate` refuses the `NINE_*` and `*_API_KEY` patterns outright | declare + grant |
| `net.http` | a host function — wazero has no network | declare + grant |

What makes this a boundary rather than a policy: a capability is either a wazero
pre-open or a host function the daemon exports, so anything else is not "denied"
— it is **structurally absent**, with no function to call. A sandboxed tool
cannot spawn a process, open a socket, load a native library, or call another
tool.

`net.http` is the exception with no primitive underneath it, so its security is
Nine's own problem (`nethttp.go`, `ssrf.go`). The guest never touches a socket
and never learns an IP. Two independent gates must both pass: the hostname
matches the tool's `allow_hosts`, **and** the address actually being dialed is
publicly routable, checked immediately before connect so there is no window to
re-resolve into. Loopback, link-local (`169.254.169.254` included) and RFC 1918
are refused regardless of the allowlist, on every redirect hop, and
`Authorization`/`Cookie` are stripped across origins. The `AuditHTTP` hook is how
a package with no journal of its own still reaches the event journal.

### The generated tier

Tools Nine writes itself are **rows in the store** rather than files on disk, and
they run in the identical sandbox under identical rules — `toolvm.Generated`
differs from a developer tool in provenance, not in enforcement.

```
 tool_write  ─► deps.Bundler at WRITE time, IN THE DAEMON: resolve imports,
     │           verify each tarball checksum, run no install scripts, inline
     │        ─► store row + host.LoadGenerated
     │        ─► visible NEXT TURN (loops in flight keep the tool set they began with)
 tool_delete ─► row removed, tool unregistered
 js_eval     ─► same sandbox, same rules, persists nothing
```

`agent.RegisterGeneratedTools` (`internal/agent/register_tools.go`) registers the
three meta-tools only when the tier is on, and `js_eval` additionally needs its
own `eval` switch. With the tier off they are neither registered nor advertised,
and a loop is identical to one built before the tier existed.

`[tools.agent.capabilities]` is a **ceiling**, never an automatic grant: the most
any generated tool may be conferred. Declarations are re-resolved on every load,
so narrowing the ceiling disables a tool that no longer fits rather than leaving
it running with reach the operator withdrew. `MaxTools` caps the catalog with
least-recently-called eviction fed by `TouchGenerated` — every generated tool
competes in the same tool-ranking budget (§8), so an unbounded catalog would
degrade selection for the built-ins too.

### Dispatcher integration & reporting

The `agent` package deliberately does not depend on the wasm runtime. It sees
two methods:

```go
type SandboxedHost interface {
    Tools() []*toolvm.Tool
    Call(ctx context.Context, name string, args json.RawMessage) (string, error)
}
```

`Dispatcher.RegisterSandboxed` indexes every loaded tool into the same `handlers`
map a plugin tool lands in, so dispatch, ref-parameter expansion, and output
capping are identical; a nil host registers nothing. Unlike a plugin there is no
process to ask `plugin.describe`, so the **manifest** is authoritative for name,
description, and schema.

Every load attempt — the failures included — is retained as a `toolvm.Status` and
surfaced by `nine tools` with its reason. Generated statuses are kept in a
separate slice so reloading the developer directory does not erase the generated
tier's outcomes, or vice versa. A tool an operator installed that is *not*
running is exactly the thing they need told.

The normative contract is `spec/contracts/toolvm.md` (`nine spec toolvm`); the
design rationale is [sandboxed-tools.md](sandboxed-tools.md).

---

## 12. Memory & persistence

A single **SQLite** database file (driver: `modernc.org/sqlite` via `database/sql`
— pure Go, no cgo) holds everything. `internal/memory.Store` is the **sole owner**
of the database handles — the "single gateway" invariant. Since SQLite serializes
writes, that is a one-connection writer pool plus a concurrent read-only pool, with
statements routed by leading keyword. The schema is applied idempotently on `Open`
(`CREATE TABLE IF NOT EXISTS`, no migration runner), which fails fast if the file
cannot be opened.

```
   nine.db (SQLite)
   ├─ kv                 agent K/V memory          (memory_get/set/delete/list)
   ├─ files              content + FTS5 index      (file_store/fetch/list/search_text)
   ├─ vectors            float32 blob embeddings   (skills, session-index, agent namespaces)
   ├─ conversations      message history, scratchpad, status
   ├─ goals              open-ended intentions, subtree JSON
   ├─ notifications      pending push messages → next active turn
   ├─ user_notifications human-facing feed (nine notifications)
   ├─ reflections        idle-reflection summaries
   ├─ workflows          multi-step plans (steps as JSON array on the row)
   ├─ skills             built-in (seeded) + agent-authored skills, by source
   ├─ session_plans      per-session stage state + idle config
   ├─ human_requests     HITL question/answer state
   ├─ interactive_sessions  which sessions are HITL-eligible
   ├─ session_events     append-only execution journal (seq, span, JSONB payload)
   ├─ event_cursors      per-subscriber durable journal position
   └─ related_sessions   derived cross-session links (pull-surfaced in context)
```

Two access tiers:

- **Agent-facing tools**: K/V, file storage, skills, and (core-intercepted)
  vector ops are exposed to the model as tools.
- **Daemon-only `internal.*` methods**: `conversations`, `goals`,
  `notifications`, `reflections`, `workflows`, `session_plans`, and the HITL
  tables are touched only by the daemon — never advertised as tools. This stops
  an agent from directly rewriting its own conversation state. (There is no
  `tasks` table — finite work is a sub-agent or a workflow step.)

### The event journal & subscriptions

Alongside the checkpoint snapshot, every session's full execution trajectory is
written to an append-only journal — the `session_events` table (`seq` BIGSERIAL,
`agent_id`, `turn`, `span_id`/`parent_span_id`, `type`, `ts`, JSONB `payload`).
The producer is `runtime.NewSQLEventSink`, an **async batched** writer wired into
each `AgentWorker` off the turn's critical path; loop hooks emit `turn_start`,
`llm_request`/`llm_response` (with the exact assembled system prompt and
messages), `tool_start`/`tool_end`, `context_update`, and `turn_end`.

```
   AgentWorker turn ─► loop hooks ─► EventSink.Append (buffered)
                                        │  batch flush
                                        ▼
                                   session_events  ──► onFlush → daemon.NotifySubscribers
```

Three consumers sit on top of the journal:

- **Read / replay.** `nine trace <agent-id>` renders a session's trajectory
  straight from the table (works with the daemon down). `internal/replay`
  reconstructs a session and re-executes it on a real `agent.Loop` wired to a
  *recorded* provider/dispatcher — deterministic, no live LLM/tool calls
  (`nine replay`). Journal-backed reattach lets a revived session show real
  history. Design: [event log](event-journal.md).
- **Retention.** `store.SessionEventsScrub(keepTurns, maxAge)` runs at boot to
  bound growth (`[daemon] event_retention_turns` / `event_retention_days`).
- **Subscriptions.** `internal/subscribe` gives each `Handler` a durable cursor
  (`event_cursors`) over `seq`, an in-process wake (from the sink's flush) plus
  catch-up on restart, at-least-once delivery, and poison-event skip. Subscribers
  are programmatic and **out-of-band** — they enrich derived stores, never make a
  generative LLM call and never touch the active session. Two exist:
  `subscribers.RelatedIndexer` (`internal/subscribers`) links topically-similar
  sessions into `related_sessions` (surfaced back into context on a later turn —
  pull, not push; on by default when an embedder is configured), and the
  Supervisor itself (its control-plane bus folded onto the journal). Design:
  [reactive events](event-journal.md).

### Checkpoints

```
   end of every turn:
     loop.SaveState()  →  JSON({history, scratchpad})  →  ckpt.Save(agentID, data)

   attach / restart resume:
     ckpt.Load(agentID) → loop.LoadState(data) → AgentWorker rebuilt
```

Checkpoints are what make `nine attach <id>` and Level-4 restart survival work:
a session is fully reconstructable from its serialized loop state plus its
`session_plans` row.

---

## 13. Session plans & stages — the autonomy substrate

Every `AgentWorker` carries a **session plan**: a small state machine of
**stages** persisted in `session_plans`. This is the single mechanism behind all
between-turn autonomy.

```
   StageHandler interface (internal/runtime/session_plan.go)
     Init(ctx, agentID, cfg)
     OnTurnEnd(ctx, agentID, result, err)   ← after every turn (and on stall)
     OnIdle(ctx, agentID) (turnText, ok)    ← when this routine's idle interval elapses

   StageRegistry (kind → factory):
     "active"          → trivial no-op stage (every conversation)
     "idle-reflection" → self-reflection session    (registered at boot)
     "pursue"          → per-goal background session (registered at boot)
```

The idle scheduler lives in the worker's `select`:

```
   armIdleTimer():  next = min remaining idle_interval across active idle-capable routines
                    (no idle-capable routine → no timer; plan paused → no timer)

   run() select:
     case <-inbox:      processTurn        ← real turn
     case <-idleTimer:  handleIdle         ← find the due stage, OnIdle(),
                                             run returned text as a turn if ok
```

```
                 ┌──────── ordinary conversation ────────┐
   profile:      │ [active]                               │  no idle work,
                 │ lazy-persisted on first checkpoint     │  attach-on-demand
                 └────────────────────────────────────────┘

                 ┌──────── self-reflection session ───────┐
   agentID:      │ "self-reflection"  (fixed)             │  wakes every 2 min,
   profile:      │ [idle-reflection]                      │  updates self/* KV,
                 │ eager-persisted, resumed at boot       │  writes reflections row
                 └────────────────────────────────────────┘

                 ┌──────── goal pursue session ───────────┐
   agentID:      │ == goalID  (1:1 with a top-level goal) │  wakes every 5 min,
   profile:      │ [pursue]                               │  acts on the goal,
                 │ eager, capped by max_goal_sessions(10) │  syncs goals.status
                 └────────────────────────────────────────┘
```

On daemon restart, `ResumeSessions` walks `session_plans` and restarts every
`active` plan that has an idle-capable routine (`planNeedsResume`) — so background
autonomy survives reboots. Ordinary `[active]` conversations are not auto-resumed;
they come back on demand via `attach`.

---

## 14. Autonomy & oversight components

```
   ┌─────────────────────────────────────────────────────────────────┐
   │                         SUPERVISOR                                │
   │   durable: Post appends to the journal (supervisor event type),   │
   │   consumed via a resumable cursor that survives restart           │
   │   events:  EventAgentCompletes | EventGoalStalls                  │
   │            EventGapReported    | EventPluginCrashed               │
   │   actions: log events, diagnose gaps, or surface them to the user │
   │            (a crashed plugin is logged; restart is the manager's) │
   └───────────▲───────────────────────▲──────────────────────────────┘
               │ gap_report tool         │ stall detector (Limit=5 no-tool turns)
               │                          │
   ┌───────────┴──────────┐    ┌──────────┴───────────┐
   │  any agent.Loop      │    │  any AgentWorker      │
   └──────────────────────┘    └───────────────────────┘

   SUB-AGENTS (run_agent / run_agents)
     parent loop → spawnOne → fresh agent.Loop in its leaf role, depthGuard-1 (RunSubAgentSync)
     lifecycle streamed to parent: sub_agent_start / sub_agent_end
     run_agents fans out: one goroutine per task, WaitGroup join, ≥300s timeout

   WORKFLOWS (internal/workflow.Service)
     named multi-step plans; steps as JSON on the row; auto-close when all
     steps terminal; startup scrub marks interrupted "running" steps failed.

   GOALS
     open-ended intentions; goal_create spawns a pursue session (goal-spawning roles only).
```

The supervisor's priority sits between user turns and background work
(value 1 — actually highest), so its diagnostic LLM calls preempt background
reflection but it never blocks. Its bus is **durable**: `Post` synchronously
appends each control-plane event to the journal (the `supervisor` event type),
and the supervisor consumes them via a cursor-backed subscription that resumes
from its last position on boot, so reactions survive a restart (see
[reactive events](event-journal.md) phase 4).

### Workflows — state and write paths

Workflow state lives entirely in the `workflows` table. Steps are stored as a JSON
array on the workflow row rather than as separate rows: reads and writes stay simple
(load the row, update one step, write it back) with no join for the common case of
reading the whole plan. Each step records the sub-agent that executed it in its
`agent_id` field, so a step is linked to its execution without a second table.

Two write paths reach it, and only one of them is agent-reachable:

1. **LLM tools** — `workflow_create`, `workflow_update`, `workflow_get`,
   `workflow_list`, `workflow_retry_step`, core-intercepted and registered through
   `RegisterWorkflowTools` on the dispatcher. Delegating roles only, with the
   delegation depth guard as the backstop — gated exactly as `run_agent` is
   ([roles](roles.md)).
2. **Operator commands** — `WorkflowCancel` (stop) and `WorkflowFail` (post-mortem),
   daemon-private methods on the store. They are invoked in response to the
   `workflow_stop` / `workflow_fail` protocol messages, or directly by the CLI, which
   opens the store in-process via `memory.Open` when the daemon is down.

`workflow_update` **auto-closes**: marking a step `done` or `failed` checks whether
every step is terminal and, if so, closes the workflow as `done` (all succeeded) or
`failed` (any failed), so the model never needs an explicit close call. At boot,
`internal/workflow.scrub` marks any step still `running` as `failed` with reason
`interrupted` — the ungraceful-shutdown case — while workflows that still hold
`pending` steps stay `active` so a later turn can resume them.

### Goals — state, write paths, status

Goal state lives in the `goals` table: a description, a status, an optional
`parent_id`/`parent_type` (the conversation or goal that spawned it; null for a
top-level goal), and an append-only `subtree` JSON array recording the sub-goals and
sub-work it has spawned.

1. **LLM tools** — `goal_create`, `goal_get`, `goal_list`, `goal_update_status`,
   `goal_append_subtree`, registered through `RegisterGoalTools`
   (`RegisterGoalCreate` + `RegisterGoalManagement`). Delegating roles only, gated as
   the workflow tools and `run_agent` are. `goal_create` defaults
   `parent_id`/`parent_type` to the owning conversation when no parent is given.
2. **Daemon read path** — `list_goals` is a thin handler proxying to
   `store.GoalList()` so `nine goals` and `/goals` can list without an agent loop
   running. The daemon does not otherwise interpret or act on goal data (invariant 4).

Status is one of `active` | `paused` | `done` | `archived`, where `done` means
explicitly completed or resolved and `archived` means retired without completion —
the distinction matters because only `active` goals are pursued.

---

## 15. Self-improvement — skills and generated tools

Nine improves what it **knows** by writing skills, and — where the operator turned
that tier on — what it can **do** by writing sandboxed tools (§11). It does not
generate plugins, write itself a capability grant, change its configuration, or
rebuild its source at runtime, a deliberate decision to keep the running system
from drifting away from its source. Consequently the runtime container carries no
Go toolchain, no git, and no source tree.

```
   AGENT SKILLS      skill_write / skill_modify → skills table
                     no compile   no restart   no approval

   GENERATED TOOLS   tool_write / tool_delete → store rows → toolvm.Host
                     no compile   no restart   approval per require_approval
                     capabilities: declared by the agent, GRANTED by the operator
```

- Skills live in the `skills` table. **Built-in** skills are embedded
  in the binary and seeded as immutable on every boot; **agent** skills are written
  via `skill_write`/`skill_modify`, which refuse to touch a built-in. Each write
  embeds the description into the `skills` vector namespace so the self-model can
  surface it on the next turn.
- Generated tools live in the store and run in the same wasm sandbox as a
  developer tool. Both tiers are **store state** — listable and deletable like a
  goal or a workflow — which is what keeps them inside the same boundary as
  skills rather than being a new kind of self-modification.
- The line that makes this safe is one column wide: **the agent writes the code,
  the operator writes the grants, and they are never the same actor.**
  `tool_write` writes JavaScript and a capability *declaration*; it has no path
  to write a grant, and a declaration past the `[tools.agent.capabilities]`
  ceiling is a refusal the model can act on.

| | Code | Capabilities |
|---|---|---|
| Native plugin | operator (build time) | operator (`nine.toml`) |
| Developer sandboxed tool | developer (file on disk) | operator (`nine.toml`) |
| Generated sandboxed tool | **Nine** (runtime) | operator (`nine.toml`) |

- Configuration changes are made by the operator editing `nine.toml` and restarting
  the daemon. Adding a plugin or changing a built-in skill means editing the source
  repo and rebuilding the image. See [self-modification.md](self-modification.md).

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
        │  find "idle-reflection" stage, interval elapsed
        ▼
   OnIdle() returns the reflection prompt, ok=true
        ▼
   processTurn(prompt)  ── same path as a user turn, Priority=Background ──► LLM
        ▼
   model calls memory_set self/capabilities, self/learned
        ▼
   OnTurnEnd records a row in `reflections`
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
 3.  plugin.NewManager + TryStartBuiltin(files, shell, http, time)
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

## 19. Key invariants (the rules that keep it coherent)

1. **One session, one goroutine, serialized turns.** `agent.Loop` is never
   touched concurrently; the `AgentWorker` goroutine enforces it.
2. **The LLM is only reachable through the queue.** Concurrency and priority are
   centralized; no agent calls a provider directly.
3. **`memory.Store` is the only `*sql.DB` owner.** All persistence flows through
   it; domain services (e.g. `workflow.Service`) depend on narrow interfaces,
   not the DB.
4. **Operational tables are daemon-private.** Agents get K/V, files, and vectors
   as tools — never `conversations`, `goals`, `workflows`, etc. directly.
5. **Every turn ends with a checkpoint.** State survives disconnects and daemon
   restarts because `{history, scratchpad}` is always persisted.
6. **Sub-agent recursion is depth-capped (`< 2`).** Delegation cannot spiral.
7. **Display names never reach the LLM.** `DisplayName` is JSON-`-` on `ToolDef`;
   it exists purely for the TUI.
8. **Background autonomy is resumable.** Idle-capable session plans are
   eager-persisted and restarted at boot; ordinary conversations are lazy and
   attach-on-demand.
9. **The journal is append-only and reactions are out-of-band.** Every step is
   written to `session_events`; subscribers enrich derived stores off the turn
   path and never make a generative LLM call or mutate an active session (enrich,
   don't interject).
10. **A capability is conferred by config or it does not exist.** A manifest
    declares; only `nine.toml` grants; the two must match exactly in both
    directions or the tool does not load. Ungranted reach is not denied at call
    time — there is no host function to call.
11. **One sandbox instance per call.** No global state, no cache, and no
    credential survives a sandboxed tool call; the wall-clock deadline is the
    only CPU bound, since wazero has no fuel metering.
12. **One tool name, one backend.** Core, plugin, and sandboxed tools share a
    single namespace; later registrations are skipped with a reported reason,
    never allowed to shadow an earlier one.
```
