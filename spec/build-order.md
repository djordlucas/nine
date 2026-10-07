# Nine — build order (the playbook)

This is the procedural core of the spec. Build Nine in the phases below, in order. Each
phase has:

- **Goal** — what becomes possible when the phase is done.
- **Build** — the components to implement (each links to its contract).
- **Wire** — how they connect to what already exists.
- **Gate** — an observable acceptance check that **MUST** pass before the next phase.

The ordering is a real dependency order: every phase depends only on earlier ones. The
dependency direction is acyclic and downward — `cmd` → `runtime` → {`agent`, `plugin`,
`memory`, `llm`, `context`, `embed`, `selfmodel`, `workflow`}, with `protocol` shared by
client and daemon but depending on neither.

A useful mental milestone: **Phase 6 yields a working single-turn interactive
conversation.** Everything before it is plumbing; everything after it is durability,
autonomy, and delegation layered on.

---

## Phase 0 — foundations: config, store, protocol types

**Goal.** A process can load configuration, open the one database, and speak the wire
format (even if nothing answers yet).

**Build.**
- Config loader → [`contracts/config.md`](contracts/config.md). Resolve `nine.toml` via
  `$NINE_CONFIG` → `./nine.toml` → `/nine.toml` → `~/.nine/nine.toml`; apply defaults.
- The memory store → [`contracts/memory-store.md`](contracts/memory-store.md). Open the
  **SQLite** database at `[memory].path`, creating the file if absent, **fail fast** if it
  is unusable, create the full schema idempotently (`CREATE TABLE IF NOT EXISTS`; no
  migration table), and expose the agent-facing and daemon-internal method sets. This
  object is the **only** holder of the DB handle (invariant I3).
- Wire types → [`contracts/wire-protocol.md`](contracts/wire-protocol.md). The `Msg`
  struct, `ProgressEvent`, `StatusInfo`, and the newline-delimited JSON framing, in a
  package that depends on neither client nor daemon.

**Gate.** Point at a fresh database file; run schema creation; round-trip a
`memory_set`/`memory_get` through the store; marshal/unmarshal every `Msg` type. The store
passes its contract's unit tests against a real database (the reference gives each test
its own file in a temp directory, so the suite needs no external service).

---

## Phase 1 — LLM provider and the priority queue

**Goal.** Code can request a completion through a single prioritized choke point.

**Build.** → [`contracts/llm-provider.md`](contracts/llm-provider.md)
- The `Provider` interface — a **single** `Complete(ctx, Request) (Response, error)`,
  streaming via the request's `OnChunk` callback. The **Ollama** local-model adapter is the
  only one, is required (R-LLM.7), and is hermetically unit-tested. There is no OpenAI
  chat adapter and no hosted-API adapter.
- The `Queue`: a priority-ordered queue bounding in-flight calls to `max_concurrent`, with
  priorities `Supervisor(1) < Conversation(2) < Background(3)` (lower served first).

**Wire.** The queue wraps the provider. Nothing else calls the provider directly
(invariant I2).

**Gate.** With `max_concurrent = 1`, submit three requests at priorities 3, 2, 1 while
one is in flight; verify they are served 1 then 2 then 3 after the in-flight call
finishes. The Ollama adapter, driven against a mock `/api/chat` server, accumulates
streamed chunks in order into the response and maps `done_reason`/tool calls correctly
(R-LLM.7).

---

## Phase 2 — embedder

**Goal.** Text can be embedded for relevance ranking and semantic search.

**Build.** → [`contracts/embedder.md`](contracts/embedder.md)
- The `Embedder` interface (`Embed(ctx, text) ([]float32, error)`).
- Providers: `keyword` (built-in, no network — the default), `ollama`, and
  `none` (returns no vector / disables ranking). There is no OpenAI embedder.

**Wire.** Construct from `[embeddings]` config. The store gains vector put/query backed
by these embeddings under namespaced keys (`skills`, `session-index`, per-agent memory
namespaces — there is no `tools:` namespace).

**Gate.** Embedding the same text twice is deterministic for `keyword`; cosine ranking
returns the more similar of two candidates for an obvious query. With provider `none`,
ranking is bypassed (all candidates returned).

---

## Phase 3 — plugin manager and default plugins

**Goal.** External capabilities exist as subprocesses behind one contract.

**Build.** → [`contracts/plugin.md`](contracts/plugin.md)
- The two-method plugin contract (`plugin.describe`, `plugin.call`) served as `POST /rpc`
  over a per-plugin **Unix socket** (`NINE_PLUGIN_SOCKET`), and the `plugin.Serve` server
  loop (in `internal/plugin`) so a plugin's `main` passes `[]ToolDefinition` + a
  `name → ToolHandler` map. This is the only transport: an MCP server is reached
  through the `mcp` bridge plugin (R-PLUG.15), whose stdio client is internal to it.
- The `Manager`: spawn a binary (env `NINE_PLUGIN_SOCKET`, `NINE_BIN`, plus any per-plugin
  extras), wait for the socket, call `plugin.describe`, track `*Plugin{Name, client,
  Tools}`, and forward `plugin.call` over a pooled `http.Client`.
- Default plugins started at boot: `files`, `shell`, `http`, `time`. (Skills
  and memory are core-intercepted, not subprocesses.)
- The Go four are not separate binaries: they live in the `nine` binary and are spawned
  as `nine plugin serve <name>` (R-PLUG.13), so `StartBuiltin` carries the plugin's name
  explicitly rather than deriving it from the executable path. Nine ships no other
  plugin artifact; a capability it does not implement is an `[[mcp.server]]`
  (R-PLUG.15), including browser automation.

**Wire.** Tool definitions are registered with the dispatcher on start (they are **not**
embedded — there is no `tools:` vector namespace). A crashed subprocess is isolated
(invariant I9).

**Gate.** Start the `shell` plugin; `plugin.describe` returns its tool; a direct
`plugin.call` runs a command and returns output. Kill a plugin subprocess mid-run; the
manager reports the failure without crashing the host.

---

## Phase 4 — context builder

**Goal.** A turn's full LLM request can be assembled within a fixed token budget.

**Build.** → [`contracts/context-builder.md`](contracts/context-builder.md)
- Priority allocation: P1 system core (never trimmed), P2 tool defs (relevance-filtered,
  default top-20 + always-include), P2.5 self-model (cap ~600 tokens), P3 history
  (trim oldest), P4 scratchpad (trim oldest), P5 extras (dropped if < 200 tokens
  remain).
- Token model: **3.45 bytes ≈ 1 token**, no tokenizer dependency.
- Tool relevance filtering by cosine similarity to the query vector; always-include
  tools (memory/file + core-intercepted) bypass filtering.

**Gate.** With a tiny budget, assembly keeps system core and drops extras; with many
tools, only top-N-by-similarity plus always-include tools appear; always-include tools
are never dropped.

---

## Phase 5 — agent loop and dispatcher

**Goal.** A single ReAct turn runs end to end against the queue and tools.

**Build.**
- The dispatcher → [`contracts/dispatcher.md`](contracts/dispatcher.md): a name→handler
  map (empty at construction), post-call hooks, and the 2048-token output cap.
  Core-intercepted handlers are added by the `Register*` functions at startup; an
  unregistered tool dispatches as `unknown tool`. The large-output paths (spilling an
  over-cap result to the store, R-DISP.2; expanding `x-nine-ref` arguments, R-DISP.7)
  are additive — the dispatcher works without a spill sink or ref resolver registered,
  so they can be deferred until the memory store exists.
- The loop → [`contracts/agent-loop.md`](contracts/agent-loop.md): `Run(ctx, text)`
  embeds the query once, assembles context, submits to the queue, and either returns a
  final answer (no tool calls → clear scratchpad, fold into history) or dispatches each
  tool call (retry up to `maxToolRetries = 2`), appends observations to the scratchpad,
  and loops. An empty response (no text, no tool calls) is re-drawn up to
  `maxEmptyAnswerRetries = 2` times; a still-empty answer then surfaces accumulated tool
  errors rather than returning blank.

**Wire.** Loop ← context builder, queue, dispatcher, embedder, optional self-model fn.

**Gate.** A loop with a stub echo tool completes a turn that calls the tool once and
then returns a final answer; the scratchpad is cleared and the exchange is in history. A
tool that always errors is retried 3 times total and the failure becomes a visible
observation.

---

## Phase 6 — agent worker, daemon socket server, checkpoints, notifications

**Goal.** **A real client can hold an interactive conversation with streaming output.**
This is the first end-to-end milestone.

**Build.**
- The agent worker → [`contracts/agent-worker.md`](contracts/agent-worker.md): one
  serial worker per session; single-slot inbox (invariant I1); per-turn pipeline
  (prepend notifications → wire progress callbacks → `loop.Run` → check stall →
  checkpoint → respond); the replay buffer (ring cap 200, send ≤ 50 on attach).
- The daemon → [`contracts/wire-protocol.md`](contracts/wire-protocol.md): a Unix-socket
  server handling each connection concurrently, routing `Msg` by type. Implement
  `new_conversation`, `user_turn`, `status`, `list_tools`, `plugin_call` (LLM bypass).
- Checkpoint persistence and notification fetch through the store (invariant I5).
- Client `EnsureDaemon`: auto-start the daemon (re-exec) if the socket is dead.

**Wire.** Daemon owns the session registry (`map[agentID]*AgentWorker`). `user_turn`
registers a `progressFn` that streams `tool_start`/`tool_end`/`context_update`/
`response_chunk`, then `response` + `done`.

**Gate.** `nine "hello"` from a cold start auto-starts the daemon and prints a reply. A
second `nine "..."` continues the same thread (history retained). Tool calls stream to
the client. `nine status` lists uptime, active agents, and loaded plugins.

---

## Phase 7 — attach and reconnect

**Goal.** A disconnected session can be resumed and its missed output redrawn.

**Build.** `attach` message handling → load from checkpoint if the session is not live,
then return a replay snapshot (recent events + last response) from the worker's replay
buffer.

**Gate.** Start a turn; disconnect the client mid-turn; reconnect with `nine attach
<id>`; the client redraws recent tool activity and receives the final answer. After a
daemon restart, `attach` rebuilds the session from its checkpoint.

---

## Phase 8 — processes and live instances

**Goal.** Work runs between turns, driven by processes rather than by people.

**Build.** → [`contracts/processes.md`](contracts/processes.md)
- `Host.StartLive` and `nine:process` (`next`, `turn`, `report`), in a live pool apart
  from the call slots, with the work budget refilled per trigger.
- The `processes` table: definition owned by configuration, run state by the runtime.
- The process runner: slice processes called when due; live processes started, fed
  clock ticks and piped reports, their turns run through `Daemon.ProcessTurn`.

**Gate.** A live test tool that loops on `next()` and `turn()` gets a clock tick one
cadence after it starts, and its turn runs in its own session labelled `idle`.

---

## Phase 9 — self-model and the reflection process

**Goal.** Nine keeps a current self-description and reflects on a clock.

**Build.** → [`contracts/processes.md`](contracts/processes.md) (R-PROC.9)
- The self-model assembler reading `self/identity`, `self/capabilities`, `self/learned`
  from K/V and injecting them as P2.5 of the context (cap ~600 tokens).
- `BootstrapSelfKV` to seed `self/identity` and `self/capabilities`.
- The shipped `reflect` process, driving a single fixed session
  (`agentID = "self-reflection"`) under the reflection role, every **2 min**, asking the
  model to update `self/capabilities` and `self/learned`.

**Wire.** `ReconcileSelfReflection` writes the process; the runner starts it at every
boot.

**Gate.** With the reflection interval shortened, the session takes a reflection turn
that writes `self/learned`; subsequent turns include the self-model block.
---

## Phase 10 — goals and pursue sessions

**Goal.** Open-ended goals are tracked and pursued autonomously in the background.

**Build.** → [`contracts/orchestration.md`](contracts/orchestration.md) (§ Goals)
- Goal data model and store methods; the core-intercepted tools `goal_create`,
  `goal_get`, `goal_list`, `goal_update_status` (depth-capped).
- The `pursue` routine (idle interval **5 min**): `OnIdle` reads the goal + its derived children and
  acts; `OnTurnEnd` syncs the routine status from `goals.status` and pauses the goal on
  stall.
- `SpawnGoalSession`: idempotent, capped by `max_goal_sessions` (default **10**), wired
  only at depth 0. `goal_create` reports `pursue_session: "spawned" | "limit_reached"`.

**Gate.** `goal_create` for a top-level goal records the goal and spawns one pursue
session; a duplicate spawn is a no-op; at the cap, the goal is recorded but no session
spawns and the response says `limit_reached`. `nine goals` lists goals.

---

## Phase 11 — sub-agents

**Goal.** A turn can delegate one task or a parallel batch to child agents.

**Build.** → [`contracts/orchestration.md`](contracts/orchestration.md) (§ Sub-agents)
- `run_agent` (one child) and `run_agents` (parallel batch; default group timeout =
  daemon `TaskTimeoutSeconds` = **1800s / 30 min**; agents still running at timeout are
  cancelled and marked `timed_out`).
- A child runs a fresh loop at depth+1 synchronously (`RunSubAgentSync`); lifecycle
  streams to the parent as `sub_agent_start` / `sub_agent_end`.
- Depth-capping: delegation tools unavailable at depth ≥ 2 (invariant I6).

**Gate.** `run_agents` with three independent tasks runs them concurrently and returns
all three results to the parent as one observation. A child at depth 1 can `run_agent`;
a grandchild at depth 2 cannot (the tool is absent).

---

## Phase 12 — workflows

**Goal.** Multi-step plans are durable, inspectable, and resumable.

**Build.** → [`contracts/orchestration.md`](contracts/orchestration.md) (§ Workflows)
- A `workflow.Service` depending only on a narrow `Repository` interface
  (`Insert/Load/Save/ListActive/ListRecent/Notify`); the store implements it (I3 intact).
- Core-intercepted tools `workflow_create`, `workflow_update`, `workflow_get`,
  `workflow_list`, `workflow_retry_step` (depth-capped). Auto-close when all steps are
  terminal.
- Operator paths: `workflow_stop` (live cancel) and `workflow_fail` (post-mortem, works
  with the daemon down via the store as a one-shot).
- Startup scrub: mark orphaned `running` steps `failed (interrupted)`, auto-close
  workflows now fully terminal, leave those with `pending` steps `active`.

**Gate.** A 3-step workflow advances and auto-closes `done` when the last step
completes; `workflow_stop` marks it `cancelled` with pending→`skipped`, running→`failed`;
after an unclean restart the scrub fails the interrupted step.

---

## Phase 13 — supervisor, stall detection, gaps

**Goal.** A single oversight loop reacts to stalls, gaps, and plugin crashes.

**Build.** → [`contracts/supervisor.md`](contracts/supervisor.md)
- The supervisor: a **durable, journal-backed** bus. `Post` synchronously
  appends each event (`EventAgentCompletes`, `EventGoalStalls`, `EventGapReported`,
  `EventPluginCrashed`) to `session_events` as the `supervisor` type; the supervisor
  consumes them via a cursor-backed `subscribe.Subscription` (it implements
  `subscribe.Handler`), resuming from its durable cursor on boot. (Requires the journal +
  subscription primitive from Phase 18.)
- Stall detection in the worker: count consecutive turns with zero tool calls; at
  `Limit = 5`, fire `OnStall` (post `EventGoalStalls`) and reset.
- The `gap_report` core tool posting `EventGapReported`.

**Wire.** Supervisor priority is `1` (preempts background work). `Post` is a synchronous
journal append (durable, never dropped). Plugin crash → restart-from-binary via the
manager.

**Gate.** Five consecutive no-tool turns post exactly one stall event. A `gap_report`
call posts a gap event. A full event queue drops events instead of blocking the
caller.

---

## Phase 14 — human-in-the-loop

**Goal.** Interactive sessions can pause for human input and gate risky tools.

**Build.** → [`contracts/hitl.md`](contracts/hitl.md)
- `ask_human` (interactive sessions only): create/reuse a `human_requests` row, emit
  `human_input_required`, block until answer / cancellation / expiry, return the answer.
- Approval gates: tools named in `[hitl].require_approval` trigger an auto-generated
  `ask_human` before dispatch; a `y…` answer proceeds.
- Interactive-session gating via the `new_conversation` `Interactive` flag, persisted in
  `interactive_sessions`. Background sessions and sub-agents are non-interactive.
- Restart recovery: pending requests survive restarts and are re-emitted on resume;
  stale ones expire at boot.

**Gate.** In a TUI session, `ask_human` blocks the turn until `human_input_answer`
arrives, then resumes. With `require_approval = ["shell"]`, a `shell` call prompts and a
non-`y` answer fails it. A background session offered the same tool name never prompts.

---

## Phase 15 — skills and the self-improvement boundary

**Goal.** Nine can write and retrieve skills — and nothing more invasive.

**Build.** → [`contracts/skills.md`](contracts/skills.md)
- The `skills` plugin: `skill_list`, `skill_read`, `skill_write` (create),
  `skill_modify` (update). Files are markdown with `name`/`description`/`tags`
  frontmatter in the skills directory.
- A post-call hook embedding a skill's description into the `skills:` namespace after
  `skill_write`/`skill_modify`.
- `skill_search` (embedding similarity over name+description only); content read via
  `skill_read`.
- Enforce the boundary: **no** tools for plugin generation, config rewrite, or core
  rebuild (N1–N3, I10).

**Gate.** Writing a skill creates the file, embeds its description, and makes it
discoverable via `skill_search` on the next turn without restart. No tool exists that
mutates `nine.toml`, builds a plugin, or rebuilds the binary.

---

## Phase 16 — CLI commands and TUI

**Goal.** Full operator surface.

**Build.** → [`contracts/wire-protocol.md`](contracts/wire-protocol.md) (client methods)
- CLI verbs (reference set): `<message>`, `daemon`, `goals`, `reflections`, `workflows`,
  `workflow stop|fail [--all]`, `status`, `attach <id>`.
- TUI: interactive input, live progress rendering, the HITL question panel (Phase 14),
  and locally-handled slash commands (`/help`, `/status`, `/config`, `/tools [filter]`,
  `/skills [name]`, `/memory [key]`, `/goals`, `/workflows`, `/new`, `/clear`) — these
  consume no LLM tokens.

**Gate.** Each list verb renders the matching read-only daemon query; slash commands run
without an LLM call; the TUI shows tool calls live. (Once Phase 14 ships, a pending
`ask_human` question renders with a `?` badge.)

---

## Phase 17 — startup wiring and resume

**Goal.** A single `runDaemon` brings the whole graph up in the correct order and
restores background autonomy.

**Build / Wire (order matters).**
1. Load config (+ env overrides). 2. Open the SQLite store (fail-fast). 3. Start the
plugin manager + default plugins. 4. Build checkpoint/notification stores. 5. Build the
embedder. 6. Build the supervisor and `Attach(store)` (durable bus). 7. Build the
self-model assembler. 8. `BootstrapSelfKV`. 9. `ReconcileSelfReflection(2m)`.
10. `ReconcileProcesses`. 11. Startup scrubs: `WorkflowScrub`
+ `SessionEventsScrub(retention)`. 12. Build HITL (`NewHITL` + `ExpireStale`). 13. Build
the agent builder (loop factory) with task timeout + `RelatedSessions`. 14. Construct the
daemon. 15. Build the `EventSink` and `SetEventSink`. 16. Configure it (HITL, store,
plugins, process store and runner, `max_goal_sessions`); if `related_sessions_index` (default
on) and an embedder is present, `AddSubscriber(RelatedIndexer)`. 17. Inject the
goal-session spawn fn and the progress-emit fn — these close over the now-existing daemon.
18. Start the supervisor loop and the process runner. 19. Reconcile goal processes
(standing agents). 20. The runner starts every live process that should run. 21. Start the
accept loop.

**Gate.** Cold boot seeds self KV and the reflection session, scrubs stale workflows and
journal events, seeds config-declared standing agents, and resumes any idle-capable
sessions from a prior run. Killing and restarting the daemon brings back the reflection
session and all active goal pursue sessions
automatically; ordinary conversations come back on `attach`.

---

## Phase 18 — event journal, retention, and subscriptions

**Goal.** Every session's trajectory is durably recorded, replayable, and subscribable —
the substrate the durable supervisor (Phase 13) and reactive enrichment build on.

**Build.** → [`contracts/event-journal.md`](contracts/event-journal.md),
[`contracts/subscriptions.md`](contracts/subscriptions.md)
- The `session_events` journal + an async batched `EventSink` wired into each worker via
  per-turn hooks (`turn_start`, `llm_request`/`llm_response`, `tool_*`, `context_update`,
  `turn_end`). Off the turn's critical path.
- Read surface: `nine trace` (render the journal, daemon-down capable) and `nine replay`
  (deterministic re-execution on a recorded provider/dispatcher — no live LLM/tool calls).
- Retention: `SessionEventsScrub(keepTurns, maxAge)` at boot (`event_retention_turns/days`).
- The subscription primitive (durable cursors over `seq`, in-process wake + catch-up,
  at-least-once, poison-skip) and the first subscriber, the related-session indexer
  (config-gated `related_sessions_index`, on by default with an embedder), plus its
  pull-surfacing into the context builder (`SystemEnrichment`).

**Wire.** The sink's flush wakes subscribers (`NotifySubscribers`). The supervisor (Phase
13) is itself a subscriber over the `supervisor` event type.

**Gate.** A completed turn's tool trajectory and exact model I/O are reconstructable from
`session_events`; record-then-replay reproduces the answer with no live calls; a boot scrub
bounds the journal; a fresh subscription resumes from its cursor and processes only new
events; two topically-linked sessions produce a `related_sessions` link that a later
on-topic turn surfaces into context (and an off-topic turn does not).

> **Ordering note.** The supervisor (Phase 13) depends on the journal + subscription
> primitive from this phase. A from-scratch build **MAY** bring this phase earlier (right
> after Phase 6) so the supervisor is durable from the start.

---

## Done

When every phase gate passes, validate against [`conformance.md`](conformance.md) for
the full requirement-by-requirement checklist.
