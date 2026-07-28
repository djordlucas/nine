# Nine — Detailed Architecture

This document is the deep-dive companion to [architecture.md](architecture.md).
Where that doc gives the bird's-eye view, this one walks the actual runtime:
the process topology, the goroutine model, the precise lifecycle of a turn, how
state is assembled and budgeted, and how every major component is wired
together at boot. Function and type names refer to real symbols in the tree, so
this doubles as a map for reading the source.

Cross-references: [glossary.md](glossary.md) for term definitions,
[agent-loop.md](agent-loop.md), [daemon.md](daemon.md),
[context-builder.md](context-builder.md), [session-plans.md](session-plans.md),
[plugins.md](plugins.md).

---

## 1. System topology

Nine is a **daemon/client pair**. A single long-lived daemon process owns all
state; the `nine` binary is both the thin client and the daemon (it re-execs
itself with a hidden subcommand). Plugins are independent child processes. The
LLM and PostgreSQL (with the `pgvector` extension) are the two external
dependencies.

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
   │           │   Supervisor                  │        (Ollama/Anthropic)  │
   │           │   Plugin Manager              │                            │
   │           └───────┬───────────────┬───────┘                            │
   │                   │ HTTP over     │ database/sql (pgx v5)              │
   │                   │ unix socket   │                                    │
   │      ┌────────────┴───────┐       ▼                                    │
   │      ▼     ▼     ▼     ▼   ▼   ┌──────────────────┐                     │
   │   shell files http time browser│  PostgreSQL      │  (+ pgvector)      │
   │   (plugin subprocesses)        │  nine database   │                    │
   │                                └──────────────────┘                    │
   └──────────────────────────────────────────────────────────────────────┘
```

`memory`, `files` (durable store), and `skills` are **in-process** capabilities
of `memory.Store`, not plugin subprocesses. The plugin subprocesses are `shell`,
`files` (workspace filesystem `read_file`/`write_file`), `http`, `time`, and the
optional `browser`.

Key consequences of this shape:

- **All durable state is in the daemon + PostgreSQL.** Clients are disposable.
  Closing a TUI does not stop work; reattaching replays what was missed.
- **Plugins are isolation boundaries.** A crashing or hanging plugin is a child
  process, not a daemon panic. Tools are reached only through the manager.
- **The LLM is behind a queue.** No agent ever calls the provider directly; the
  queue is the single choke point for concurrency and prioritization.

---

## 2. Process & deployment model

| Process | Binary | Role | Lifetime |
|---------|--------|------|----------|
| Client  | `nine` | TUI or one-shot request; connects to socket | Per invocation |
| Daemon  | `nine` (re-exec) | Owns sockets, sessions, queue, plugins, DB connection | Long-lived |
| Plugin  | `bin/<name>` | One tool provider, HTTP over a unix socket | Spawned by daemon, killed on stop |

The CLI auto-starts the daemon if the socket is dead (`EnsureDaemon` in
`internal/protocol/client.go`). Everything funnels through `cmd/nine/main.go`,
which dispatches to either the TUI, the one-shot client, or `runDaemon`
(`cmd/nine/daemon.go`).

### Volume layout (`/data` in Docker)

```
/data                 mutable state only
└── workspace/   files-plugin working directory

/opt/nine             immutable image content (not in the volume)
├── bin/         compiled default plugin binaries + browser launcher
└── browser/     browser plugin JS + node_modules
```

Primary state lives in **PostgreSQL**, which runs as its own service (the
`docker-compose` `pgvector/pgvector:pg17` image on port 5433, with its own
`nine-pgdata` volume) — *not* in the `/data` volume. Point the daemon at it with
`[memory].database_url` or `NINE_DATABASE_URL`.

Built-in skills are embedded in the `nine` binary (`//go:embed` in the `nine/skills`
package) and seeded into the `skills` table on every boot — there is no skills
directory in the image or the volume.

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
 ├─ idleTimer   *time.Timer                   ← per-stage idle scheduler
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
- **Empty answers are surfaced, not swallowed.** If the model ends a turn with
  no text and no tool calls, `emptyAnswerFallback` turns the accumulated tool
  errors into a visible message instead of returning blank.
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

Tools come in two flavors, both appearing in the same LLM tool list:

```
   ┌─────────────────────────────────────────────────────────────┐
   │  TOOL CALL from the model                                    │
   └───────────────────────────┬──────────────────────────────────┘
                               ▼
                       Dispatcher.handlers[name]
            ┌───────────────────┴────────────────────┐
            ▼                                         ▼
   PLUGIN TOOLS                            CORE-INTERCEPTED TOOLS
   (RegisterPlugin)                        (Register* in builder.go)
   handler = m.Call(plugin, …)             handled in-process, no subprocess:
   → JSON-RPC plugin.call                   • gap_report
                                            • memory_embed / memory_query
   shell, read_file, write_file,            • file_search_semantic
   http_get, web_search,                    • run_agent / run_agents
   skill_*, time, browser_*, …              • workflow_* / goal_*
```

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
context-based cancellation (docs/plugins-http-transport.md). External **MCP**
servers keep the legacy stdio JSON-RPC client.

```
   Manager.Start(binaryPath, extraEnv…)
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
   go build (image build) ──► TryStart (daemon boot) ──► (in use) ──► SIGTERM
   /opt/nine/bin/<name>       spawn, describe,            plugin.call   (shutdown)
                              register
```

Tool definitions are registered with the dispatcher at start time. (Their
description embeddings for context-builder relevance ranking are computed lazily
and cached by the `AgentBuilder` on first use — in memory, not in the `vectors`
table.) A crashed
subprocess is isolated from the daemon; restart from the existing binary is the
manager's responsibility.

Default plugins started at boot: `files`, `shell`, `http`, `time`, `browser`.
Memory/file/vector operations and the skill tools are **core-intercepted**
(handled in-process), not a subprocess.

---

## 11. Memory & persistence

A single **PostgreSQL** database (driver: pgx v5 via `database/sql`, with the
`pgvector` extension) holds everything. `internal/memory.Store` is the **sole
owner** of `*sql.DB` — the "single gateway" invariant. A thin `db` wrapper
rewrites `?` placeholders to `$N`; the schema is applied idempotently on `Open`
(`CREATE TABLE IF NOT EXISTS`, no migration table), which fails fast if the
database is unreachable.

```
   nine (PostgreSQL database)
   ├─ kv                 agent K/V memory          (memory_get/set/delete/list)
   ├─ files              content + tsvector/GIN FTS (file_store/fetch/list/search_text)
   ├─ vectors            pgvector embeddings, <=>   (skills, session-index, agent namespaces)
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
  history. Design: [event log](event-log.md).
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
  [reactive events](reactive-events.md).

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

## 12. Session plans & stages — the autonomy substrate

Every `AgentWorker` carries a **session plan**: a small state machine of
**stages** persisted in `session_plans`. This is the single mechanism behind all
between-turn autonomy.

```
   StageHandler interface (internal/runtime/session_plan.go)
     Init(ctx, agentID, cfg)
     OnTurnEnd(ctx, agentID, result, err)   ← after every turn (and on stall)
     OnIdle(ctx, agentID) (turnText, ok)    ← when this stage's idle interval elapses

   StageRegistry (kind → factory):
     "active"          → trivial no-op stage (every conversation)
     "idle-reflection" → self-reflection session    (registered at boot)
     "pursue"          → per-goal background session (registered at boot)
```

The idle scheduler lives in the worker's `select`:

```
   armIdleTimer():  next = min remaining idle_interval across active idle-capable stages
                    (no idle-capable stage → no timer; plan paused → no timer)

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
`active` plan that has an idle-capable stage (`planNeedsResume`) — so background
autonomy survives reboots. Ordinary `[active]` conversations are not auto-resumed;
they come back on demand via `attach`.

---

## 13. Autonomy & oversight components

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
[reactive events](reactive-events.md) phase 4).

---

## 14. Self-improvement (skills only)

Nine improves itself by writing **skills** and nothing else. It does not generate
plugins, change its configuration, or rebuild its source at runtime — a deliberate
decision to keep the running system from drifting away from its source. Consequently
the runtime container carries no Go toolchain, no git, and no source tree.

```
   AGENT SKILLS   skill_write / skill_modify → skills table   no compile   no restart   no approval
```

- Skills live in the `skills` table (PostgreSQL). **Built-in** skills are embedded
  in the binary and seeded as immutable on every boot; **agent** skills are written
  via `skill_write`/`skill_modify`, which refuse to touch a built-in. Each write
  embeds the description into the `skills` vector namespace so the self-model can
  surface it on the next turn.
- Configuration changes are made by the operator editing `nine.toml` and restarting
  the daemon. Adding a plugin or changing a built-in skill means editing the source
  repo and rebuilding the image. See [self-modification.md](self-modification.md).

---

## 15. End-to-end data flows

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

## 16. Startup sequence

`runDaemon` (`cmd/nine/daemon.go`) wires the object graph in this order:

```
 1.  config load  +  ApplyEnvOverrides
 2.  memory.Open(cfg.DatabaseURL())                 ← the single Postgres store (fail-fast)
 3.  plugin.NewManager + TryStart(files, shell, http, time, browser)
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

## 17. Component relationship map

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
   │  ckpt / notif / store ► memory.Store  (single *sql.DB → PostgreSQL)
   │  sink ─────────────────► EventSink ──► session_events journal
   │  subscribers[] ────────► RelatedIndexer (out-of-band, cursor-backed)
   └─────────────────────────────────────────────────────────────────┘
            │                       │                      │
            ▼                       ▼                      ▼
     agent.Dispatcher       ninectx.Builder          llm.Queue ──► llm.Provider
     (tool routing)         (context budget)         (priority)    (anthropic/
            │                                                       ollama)
            ├─ plugin tools  ──► plugin.Manager.Call
            └─ core tools    ──► in-process handlers
                                 (memory, embed, run_agent,
                                  workflow_*, goal_*, gap_report)

   embed.Embedder ──► used by: ninectx tool ranking, skill search,
                                memory_query / file_search_semantic,
                                selfmodel.Assembler
```

Dependency direction is acyclic and downward: `cmd` → `runtime` →
{`agent`, `plugin`, `memory`, `llm`, `context`, `selfmodel`, `embed`,
`workflow`}. The `protocol` package is shared by both the daemon and the
client/TUI but depends on neither, so client code never pulls in the runtime.

---

## 18. Key invariants (the rules that keep it coherent)

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
```
