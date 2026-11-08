# Architecture

This document walks the actual runtime: the process topology, the goroutine model,
the precise lifecycle of a turn, how state is assembled and budgeted, and how every
major component is wired together at boot. Function and type names refer to real
symbols in the tree, so this doubles as a map for reading the source.

§1 is the bird's-eye view; read it alone for the shape of the system, and the rest
when you need the mechanism.

Cross-references: [glossary.md](glossary.md) for term definitions,
[agent-loop.md](agent-loop.md), [daemon.md](daemon.md),
[context-builder.md](context-builder.md), [processes.md](processes.md),
[plugins.md](plugins.md).

---

## 1. System topology

Nine is a **daemon/client pair**. A single long-lived daemon process owns all
state; the `nine` binary is both the thin client and the daemon (it re-execs
itself with a hidden subcommand). Plugins are independent child processes. The
LLM endpoint is the only external dependency Nine always has; each declared
`[[mcp.server]]` adds another, because the manager connects to a server that is
fetched (`npx`) or hosted elsewhere (§10).

```
   ┌──────────────────────────────────────────────────────────────────────┐
   │  HOST / CONTAINER                                                      │
   │                                                                        │
   │   ┌─────────────────┐         ┌─────────────────┐                      │
   │   │  nine <command> │         │  nine "msg"     │   ← same binary,     │
   │   │  operator CLI   │         │  one-shot CLI   │     client mode      │
   │   └────────┬────────┘         └────────┬────────┘                      │
   │            │ newline-delimited JSON     │                              │
   │            │ over AF_UNIX socket        │                              │
   │            └─────────────┬──────────────┘                              │
   │           ┌──────────────┴──────────────┐                              │
   │           │  nine api (HTTP server)     │  ← REST, web UI; same socket  │
   │           └──────────────┬──────────────┘                              │
   │                          ▼                                             │
   │           ┌──────────────────────────────┐                            │
   │           │   DAEMON  (nine, daemon mode) │                            │
   │           │   /tmp/nine.sock              │                            │
   │           │                               │                            │
   │           │   Daemon ─ AgentWorker(s)     │                            │
   │           │     │         │               │                            │
   │           │     │         ├─ agent.Loop   │                            │
   │           │     │         └─ its process  │                            │
   │           │     │                         │                            │
   │           │   LLM Queue ── Provider ──────┼──────►  LLM endpoint       │
   │           │   Supervisor                  │        (Ollama)            │
   │           │   Plugin Manager              │                            │
   │           │   Core dispatcher (built-ins) │                            │
   │           └───────┬───────────────┬───────┘                            │
   │                   │ HTTP over     │ database/sql (modernc sqlite)      │
   │                   │ unix socket   │                                    │
   │      ┌────────────┴───────┐       ▼                                    │
   │      ▼                 ▼       ┌──────────────────┐                     │
   │   shell              mcp:*     │  SQLite (one file)                    │
   │   (plugin subprocesses)        │  nine database   │                    │
   │                                └──────────────────┘                    │
   └──────────────────────────────────────────────────────────────────────┘
```

Memory, the durable file store, and skills are **in-process** capabilities of
`memory.Store`, not plugin subprocesses. The plugin subprocesses are `shell`, any user plugin, and one spawned
process per declared `[[mcp.server]]` that uses `command` (a `url` server is
hosted and has no process). The workspace file tools, the fetching tools and
`time` are **Go built-ins** compiled into the nine binary — direct in-process
calls into the same `memory.Store` and workspace backend every other component
uses (adr/modular_nine.md §9.10; the wasm sandbox they used to run in is gone).

Key consequences of this shape:

- **All durable state is in the daemon + its SQLite file.** Clients are disposable.
  Closing the web UI does not stop work; reattaching replays what was missed.
- **Plugins are isolation boundaries.** A crashing or hanging plugin is a child
  process, not a daemon panic. Tools are reached only through the manager.
- **There is one kind of isolation that matters.** A plugin is isolated for
  *reliability* — it is a separate process, so it can crash or hang without
  taking the daemon down; its reach is bounded by `[plugins] disabled` and the
  process boundary (MCP is the only wire protocol that crosses it).
- **The LLM is behind a queue.** No agent ever calls the provider directly; the
  queue is the single choke point for concurrency and prioritization.

---

## 2. Process & deployment model

| Process | Binary | Role | Lifetime |
|---------|--------|------|----------|
| Client  | `nine` | One-shot request or operator command; connects to socket | Per invocation |
| Daemon  | `nine` (re-exec) | Owns sockets, sessions, queue, plugins, DB connection | Long-lived |
| API server | `nine` (re-exec) | HTTP/REST server; translates requests to the daemon socket. Serves OpenAPI/Swagger at `/api/v1/swagger/` (see [api.md](api.md)) | Long-lived (s6-managed in Docker) |
| Plugin  | `bin/<name>`, or `nine` (re-exec) for the built-ins | One tool provider, HTTP over a unix socket | Spawned by daemon, killed on stop |
| MCP server | `command` child, or hosted `url` | One declared `[[mcp.server]]`: the manager connects over stdio or streamable HTTP | Spawned by daemon (command form), killed on stop |

The CLI auto-starts the daemon if the socket is dead. One binary is the entry
point for everything: it dispatches to the one-shot client, an operator
command, the API server, or the daemon, depending on how it was invoked.

### Volume layout (Docker)

```
/data                 mutable state only (the "nine-data" volume)
├── nine.db      the SQLite database (+ its -wal/-shm sidecars)
└── workspace/   the workspace the file tools read and write

/opt/nine             immutable image content (not in a volume)
└── bin/         empty by default — the built-in plugins live in the nine
                 binary; a user plugin's binary can be mounted here

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


Config is resolved in order: `$NINE_CONFIG` → `./nine.toml` → `/nine.toml`
(Docker bind-mount) → `~/.nine/nine.toml`.

---

## 3. The daemon: socket server & message router

`runtime.Daemon` is a Unix-socket server. Its
core state:

```
Daemon
 ├─ socketPath   string
 ├─ agent        Agent                // builds each session (InternalAgent)
 ├─ ckpt         CheckpointStore      // save/load serialized loop state
 ├─ sessions     map[string]*AgentWorker   ← the live session registry (RWMutex)
 ├─ names        map[string]string         // agentID → display name
 ├─ mgr          *plugin.Manager      // tool listing / direct calls
 ├─ store        queryBackend         // goal/reflection/workflow read proxy
 └─ procs        ProcessBackend       // processes: goal sessions, the roles of process sessions
```

The daemon asks its `Agent` for each session it creates or resumes
(adr/agent-boundary.md). `Agent.NewSession` returns the loop and the hooks its
worker runs around turns: stall detection, the notifications prepended to a user
turn, and a completion callback. `InternalAgent` is Nine's own agent: its loop
comes from `AgentBuilder.BuildForRole`, its notifications from the
`NotifStore`, and its stall and completion events go to the `Supervisor`. Only
a session that owns a goal gets stall detection, since a stall pauses the goal;
a conversation or a reflection that answers without calling a tool is not
stuck.

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

The queue also absorbs a provider's rate limit. A request refused with HTTP 429
is sent again after 5, 15, 30 and 60 seconds, or after the provider's
`Retry-After` when it gives one, capped at 2 minutes. It keeps its slot while it
waits, since the limit is the provider's and the next request would be refused
too. Nothing has streamed when a 429 comes back, so a retry repeats nothing.
Only after the last retry does the turn fail.

---

## 5. The AgentWorker — session lifecycle

`runtime.AgentWorker` wraps one
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
 ├─ plan        *sessionPlanState             ← routines + persistence
 ├─ idleTimer   *time.Timer                   ← per-routine idle scheduler
 ├─ idleSince   map[routine]time.Time
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
 checkStall(ctx)                   ← if LastRunToolCount()==0, stallN++
        │                            at Limit → OnStall (a goal-bound session
        │                            pauses its goal)
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
arrive. This is how the web UI shows tool calls live, through the API server.

Simultaneously, `emitEvent` pushes `tool_start`/`tool_end`/`sub_agent_*` events
into a **bounded ring buffer** (`replayBuffer`, cap 200, sends up to 50 on
attach) and records the last completed response. When a client reattaches
(`attach`), the daemon hands back a snapshot so a reconnecting client can redraw
the recent history and any answer it missed while disconnected.

```
 loop event ──► emitEvent ──┬──► progressFn (live, if a client is attached)
                            └──► replay.push (always, for later reattach)
```

---

## 6. The agent loop (ReAct)

`agent.Loop` implements Reason → Act → Observe. One
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

`agent.Dispatcher` is a name → handler map with
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

Tools come in two flavors, both appearing in the same LLM tool list and sharing
**one namespace** — a name resolves to exactly one backend, and a collision is a
load failure, not a silent override:

```
   ┌────────────────────────────────────────────────────────────┐
   │  TOOL CALL from the model                                    │
   └──────────────────────────┬───────────────────────────────────┘
                              ▼
                      Dispatcher.handlers[name]
       ┌──────────────────────────────┬──────────────────────────────┐
       ▼                              ▼                              ▼
 PLUGIN TOOLS                 CORE-INTERCEPTED (built-ins)
 (RegisterPlugin)             (Register* in builder.go)
 handler =                    handler = direct Go function call:
   m.Call(plugin, …)            • memory_* / skill_* / goal_* / workflow_*
 → MCP tools/call               • process_* / job_* / queued_* / doc_*
 shell, mcp:<server> tools    • run_agent/run_agents, gap_report,
                              • read_file, write_file, edit_file, list_files,
                                file_search_text, http_get, web_search, time, …
                              • tool_list / tool_search / capability_request
```

The two differ in *isolation*: a plugin is a separate process reached over MCP,
so it can crash without touching the daemon; a built-in is in-process because
the daemon owns every database handle, and a loopback protocol would add
complexity without isolation (adr/modular_nine.md §9.8). Both go through the
same dispatch, ref-parameter expansion, and output capping.

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

`ninectx.Builder` packs one LLM request into a
fixed token budget (`context_budget`, defaults to `num_ctx`). Token counting is
a deliberate approximation: **3.45 bytes ≈ 1 token**, no tokenizer dependency.
The divisor sits just below the lowest ratio measured against provider-reported
usage, because under-counting spends context headroom that was never there
([context-builder.md](context-builder.md#token-counting)).

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

`llm.Queue` is a **priority min-heap** in front of the
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

Priority constants, lower = served first:

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

`plugin.Manager` owns plugin subprocess
lifecycle. A plugin **is an MCP server**: it speaks MCP streamable HTTP on
**a per-plugin Unix socket** (`NINE_PLUGIN_SOCKET`, `POST /mcp`) by default, or
stdio when its manifest opts in: the manager spawns the process, waits for the
socket, and handshakes (`initialize` -> `tools/list`), which gives free
per-request concurrency and context-based cancellation on the socket transport
(plugins-http-transport.md). An external **MCP** server is a plugin row too,
reached by the manager's one MCP client — streamable HTTP when the
`[[mcp.server]]` declares a `url`, a spawned `command` over stdio otherwise.
Everything past the dial is transport-agnostic: one connection, one handshake,
one tool-prefixing rule.

The Go default plugins are not separate executables: their handlers live in
`builtins`, and `Manager.StartMCPBuiltin` spawns them by re-executing the
nine binary as `nine mcp serve <name>`. That is a packaging difference only —
each still gets its own process, socket, sanitized environment, and crash
isolation.

```
   Manager.Start(binaryPath, extraEnv…)        (user plugins, socket HTTP)
   Manager.StartStdio(binaryPath, extraEnv…)   (user plugins, stdio)
   Manager.StartMCPBuiltin(name, extraEnv…)    (shell)
   Manager.StartMCPServer(spec)                (one per [[mcp.server]])
        │  spawn process with NINE_PLUGIN_SOCKET (+ NINE_BIN, extra env)
        │  wait for the socket, then MCP handshake on POST /mcp
        ▼
   ─► POST /mcp {"method":"initialize",…} ────────►  plugin
   ─► POST /mcp {"method":"tools/list",…} ────────►  plugin
   ◄─ {result:{tools:[…]}} ◄─  plugin
        │
        ▼  Manager tracks the *Plugin{Name, client, Tools}
        ▼
   Dispatcher.RegisterPlugin → handlers[tool] = m.Call(p, tool, args)
        │
        │  on each tool invocation during a turn (one MCP request each):
   ─► POST /mcp {"method":"tools/call","params":{name, arguments}} ─►  plugin
   ◄─ {result:{content:[…]}}  (or {error:{…}}) ◄────  plugin
```

Plugins are fixed: there is no runtime generation, build, or hot-swap. Each is
compiled into the image at build time and started at daemon boot:

```
   go build (image build) ──► TryStartMCPBuiltin (daemon boot) ──► (in use) ──► SIGTERM
   the nine binary            spawn `nine mcp serve <name>`,  tools/call  (shutdown)
                              handshake, register
```

An MCP server is the exception to "compiled into the image": nothing is built for
it, and the manager reaches a server that is fetched (`npx`) or hosted elsewhere.
The server process is a direct child of the daemon — no bridge in between — so the
lifecycle above is unchanged from the daemon's side. [Browser automation](browser.md) is the
worked example: Playwright's MCP server declared in `nine.toml`, no plugin
written and nothing added to the image.

Tool definitions are registered with the dispatcher at start time. (Their
description embeddings for context-builder relevance ranking are computed lazily
and cached by the `AgentBuilder` on first use — in memory, not in the `vectors`
table.) A crashed
subprocess is isolated from the daemon; restart from the existing binary is the
manager's responsibility.

Default plugins started at boot: `shell` — then one connection per
`[[mcp.server]]`, then user plugins. Reading and writing files, fetching over
HTTP and reading the clock are **built-ins** compiled into the binary, like the
memory/vector and skill tools: all of them are core-intercepted handlers called
in-process, not subprocesses holding the daemon's own authority (adr/modular_nine.md
§9.8 — the wasm tool host they used to run in is removed).

---

## 11. Tool tiers that no longer exist

The sandboxed-tool host (`internal/toolvm`, a wazero/QuickJS sandbox with a
capability-grant model and a model-authored "generated" tier) was removed
(adr/modular_nine.md §9.10). What it carried is now:

- the **shipped tools** (read_file, write_file, edit_file, http_get, web_search,
  time, …) are Go built-ins registered on the core dispatcher like every other
  `memory_*` handler — see §7;
- **developer tools** (`[tools].user_dir`) are MCP user plugins under
  `[plugins].user_dir` (§10, [plugins.md](plugins.md));
- **model-authored tools** (`tool_write`/`js_eval`, the `tools` store tables)
  are gone with it. The store tables remain (dormant) so an existing database
  still opens; the wire verbs that read them answer with the removal.

## 11. Memory & persistence

A single **SQLite** database file (driver: `modernc.org/sqlite` via `database/sql`
— pure Go, no cgo) holds everything. `Store` is the **sole owner**
of the database handles — the "single gateway" invariant. Since SQLite serializes
writes, that is a one-connection writer pool plus a concurrent read-only pool, with
statements routed by leading keyword. A fresh database gets the current schema
idempotently on `Open` (`CREATE TABLE IF NOT EXISTS`) stamped at the current
`PRAGMA user_version`; an existing one is brought forward by a sequential,
forward-only migration runner, each step in its own transaction
([versioning.md](versioning.md#3-memory-db-schema-version)). Either path fails
fast if the file cannot be opened.

```
   nine.db (SQLite)
   ├─ kv                 agent K/V memory          (memory_get/set/delete/list)
   ├─ files              spilled tool output + FTS5 (read_file, file_search_text)
   ├─ vectors            float32 blob embeddings   (skills, session-index, agent namespaces)
   ├─ conversations      message history, scratchpad, status
   ├─ goals              open-ended intentions, parent link for sub-goals
   ├─ notifications      pending push messages → next active turn
   ├─ user_notifications human-facing feed (nine notifications)
   ├─ workflows          multi-step plans (steps as JSON array on the row)
   ├─ skills             built-in (seeded) + agent-authored skills, by source
   ├─ processes          what runs between turns: definition, run state, triggers
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
  `notifications`, `workflows`, `processes`, and the HITL
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
                                        ▲          └─► OnJournal → daemon.JournalWritten → followers
   store state changes (approvals,      │
   questions, goals, workflows, …) ─────┘
```

Four consumers sit on top of the journal:

- **Read / replay.** `nine trace <agent-id>` renders a session's trajectory
  straight from the table (works with the daemon down). `replay`
  reconstructs a session and re-executes it on a real `agent.Loop` wired to a
  *recorded* provider/dispatcher — deterministic, no live LLM/tool calls
  (`nine replay`). Journal-backed reattach lets a revived session show real
  history. Design: [event log](event-journal.md).
- **Retention.** `store.SessionEventsScrub(keepTurns, maxAge)` runs at boot to
  bound growth (`[daemon] event_retention_turns` / `event_retention_days`).
- **Subscriptions.** `subscribe` gives each `Handler` a durable cursor
  (`event_cursors`) over `seq`, an in-process wake (from the sink's flush) plus
  catch-up on restart, at-least-once delivery, and poison-event skip. Subscribers
  are programmatic and **out-of-band** — they enrich derived stores, never make a
  generative LLM call and never touch the active session. Two exist:
  `subscribers.RelatedIndexer` links topically-similar
  sessions into `related_sessions` (surfaced back into context on a later turn —
  pull, not push; on by default when an embedder is configured), and the
  Supervisor itself (its control-plane bus folded onto the journal). Design:
  [reactive events](event-journal.md).
- **Followers.** `journal_follow` streams the journal to a client from a `seq`,
  woken by the store's `OnJournal` hook on every write. The API serves it as
  `GET /api/v1/events`, which the web UI's live views run on.

### Checkpoints

```
   end of every turn:
     loop.SaveState()  →  JSON({history, scratchpad})  →  ckpt.Save(agentID, data)

   attach / restart resume:
     ckpt.Load(agentID) → loop.LoadState(data) → AgentWorker rebuilt
```

Checkpoints are what make reattaching to a session and Level-4 restart survival work:
a session is fully reconstructable from its serialized loop state, plus — for
a process session — the process that owns it, which supplies its role.

---

## 12. Processes — the autonomy substrate

Everything Nine does between turns is a **process** ([processes.md](processes.md)):
a built-in tool or an MCP plugin tool the process runner drives on its triggers.
A worker never starts a turn of its own; a person, a process, or a pipe submits
each one.

```
   process runner (RunStandingTools, every [plugins] job_poll_seconds, and on Wake)
     ├─ slice processes   due → call the tool once → cycle complete / report
     └─ live processes    start once (StartLive) → next() ← clock ticks, piped reports
                                                   turn() → Daemon.ProcessTurn(session, role)
                                                   report() → pipe → owner's next() or human feed

   goal binding:  goal not active → process stopped (stopped_by "goal")
                  goal active again → process running again
```

```
                 ┌──────── ordinary conversation ────────┐
   driven by:    │ a person                               │  attach-on-demand
                 └────────────────────────────────────────┘

                 ┌──────── self-reflection session ───────┐
   session:      │ "self-reflection"  (fixed)             │  reflect, every
   process:      │ reflect, role reflection               │  [daemon] self_reflection
                 └────────────────────────────────────────┘

                 ┌──────── goal session ──────────────────┐
   session:      │ == goalID  (1:1 with a top-level goal) │  pursue, every 5 min,
   process:      │ "goal:<id>", pursue, bound to the goal │  capped by max_running(14)
                 └────────────────────────────────────────┘
```

Processes live in the `processes` table, so background autonomy survives
restarts: at boot the runner starts every live process that should run, and
ordinary conversations come back on demand via `attach`.

---

## 13. Autonomy & oversight components

```
   ┌─────────────────────────────────────────────────────────────────┐
   │                         SUPERVISOR                                │
   │   durable: Post appends to the journal (supervisor event type),   │
   │   consumed via a resumable cursor that survives restart           │
   │   events:  EventAgentCompletes | EventGoalStalls                  │
   │            EventGapReported    | EventPluginCrashed               │
   │   actions: every event is logged, and nothing yet acts on the    │
   │            first three; a crashed plugin is the manager's to     │
   │            restart. The switch arm is where a reaction plugs in. │
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

   WORKFLOWS
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
a scrub marks any step still `running` as `failed` with reason
`interrupted` — the ungraceful-shutdown case — while workflows that still hold
`pending` steps stay `active` so a later turn can resume them.

### Goals — state, write paths, status

Goal state lives in the `goals` table: a description, a status, an optional
`parent_id`/`parent_type` (the conversation or goal that spawned it; null for a
top-level goal). Sub-goals are goals naming this one as their parent, so the
hierarchy is read from the parent link rather than kept in a second place.

1. **LLM tools** — `goal_create`, `goal_get`, `goal_list`, `goal_update_status`.
   Delegating roles only, gated as
   the workflow tools and `run_agent` are. `goal_create` defaults
   `parent_id`/`parent_type` to the owning conversation when no parent is given.
2. **Daemon read path** — `list_goals` is a thin handler proxying to
   `store.GoalList()` so `nine goals` and `/goals` can list without an agent loop
   running. The daemon does not otherwise interpret or act on goal data (invariant 4).

Status is one of `active` | `paused` | `done` | `archived`, where `done` means
explicitly completed or resolved and `archived` means retired without completion —
the distinction matters because only `active` goals are pursued.

---

## 14. Self-improvement — skills

Nine improves what it **knows** by writing skills. It does not write tools,
plugins, grants, configuration, or its own source at runtime (adr/modular_nine.md
§9.10 removed the model-authored tool tier), a deliberate decision to keep the
running system from drifting away from its source. Consequently the runtime
container carries no Go toolchain, no git, and no source tree.

```
   AGENT SKILLS      skill_write / skill_modify → skills table
                     no compile   no restart   no approval
```

- Skills live in the `skills` table. **Built-in** skills are embedded
  in the binary and seeded as immutable on every boot; **agent** skills are written
  via `skill_write`/`skill_modify`, which refuse to touch a built-in. Each write
  embeds the description into the `skills` vector namespace so the self-model can
  surface it on the next turn.
- Configuration changes are made by the operator editing `nine.toml` and restarting
  the daemon. Adding a plugin or changing a built-in skill means editing the source
  repo and rebuilding the image. See [self-modification.md](self-modification.md).

---

> **Wiring detail** — the boot order, the end-to-end data flows, and the
> component relationship map live in
> [adr/architecture-wiring.md](../adr/architecture-wiring.md). They name types
> and call sites, which is what tracing a flow needs and what this document
> deliberately avoids.

## 15. Key invariants (the rules that keep it coherent)

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
   it exists purely for display.
8. **Background autonomy is resumable.** Processes are persisted and started at
   boot; ordinary conversations are attach-on-demand.
9. **The journal is append-only and reactions are out-of-band.** Every step is
   written to `session_events`; subscribers enrich derived stores off the turn
   path and never make a generative LLM call or mutate an active session (enrich,
   don't interject).
10. **A plugin runs only if `nine.toml` lets it.** `[plugins].disabled` blocks
    both start paths (spawned and hosted), and a user plugin is never executed
    without its manifest (R-PLUG.9).
11. **One tool name, one backend.** Built-in and plugin tools share a single
    namespace; later registrations are skipped with a reported reason, never
    allowed to shadow an earlier one.

---

## Limits

| Limit | Detail |
|-------|--------|
| Single host, single daemon | All state lives in one daemon process and one SQLite file. There is no clustering, no replication, and no remote client — the socket is local. |
| One SQLite writer | Every durable write goes through one file, so concurrent writes serialize. |
| Turns are sequential per session | Each worker's inbox holds one turn. Concurrency is across sessions, not within one. |
| Token budgeting is estimated | Context accounting uses a 4-characters-per-token approximation calibrated against one tokenizer. |
| Trimming, not compaction | Over-budget history is dropped from the front rather than summarized. |
| One LLM provider at a time | No routing across models within a deployment. |
| Three supervisor events unhandled | `EventGoalStalls`, `EventGapReported` and `EventAgentCompletes` are delivered and logged, and nothing acts on them. The switch arm is where a reaction would plug in. |
