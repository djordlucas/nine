# Nine — overview

Read this before [`build-order.md`](build-order.md). It establishes the system shape,
the goals and explicit non-goals, the vocabulary of work units, and the invariants that
every later contract assumes.

---

## 1. Goals and non-goals

### Goals

- **G1 — Self-contained daemon.** One long-lived process owns all state and continues
  working when no client is attached.
- **G2 — Durable, resumable sessions.** Any unit of work survives client disconnects
  and daemon restarts and can be re-attached to.
- **G3 — Autonomy between turns.** Nine pursues open-ended goals and reflects on itself
  without user prompting.
- **G4 — Background and parallel work.** Background sessions (goal-pursue, reflection)
  and sub-agents run concurrently, bounded and prioritized.
- **G5 — Small-model friendly.** Every LLM turn is assembled to a fixed token budget so
  Nine runs usefully on small local models.
- **G6 — Capability via plugins.** Every external capability is a subprocess plugin
  behind a uniform contract; a plugin crash never takes down the daemon.
- **G7 — Self-improvement as data, not code.** Nine grows its knowledge by writing
  skills; its executable shape is fixed.
- **G8 — Local-first, and plural.** Nine runs against models the operator hosts. This
  is a **commitment, not a stopgap**: G5's token budgeting exists to serve it, and a
  hosted-API backend stays out of scope (see N5). It does **not** imply a single
  backend — llama.cpp and vLLM are planned alongside Ollama, and routing different work
  to different models within one deployment is a stated direction. The `Provider`
  interface is therefore justified by **plurality inside local-first**, not by an
  anticipated hosted provider; the two were never in tension.

### Non-goals (explicit MUST NOTs)

- **N1.** Nine **MUST NOT** generate, compile, or hot-swap plugins at runtime.
- **N2.** Nine **MUST NOT** rewrite its own configuration (`nine.toml`) at runtime.
- **N3.** Nine **MUST NOT** read, rebuild, or restart from its own source tree at
  runtime. The runtime container ships **no Go toolchain, no git, and no source tree**.
- **N4.** Agents **MUST NOT** be given direct tool access to operational/daemon-private
  state (conversation rows, goal rows, the plugin registry, etc.).
- **N5.** Nine **MUST NOT** require a hosted LLM API to function. Speaking a hosted
  provider's *wire format* is permitted where a self-hosted server uses it (vLLM serves
  the OpenAI chat API), because that is an operator-hosted endpoint; depending on a
  third-party service to run at all is not.

(N1–N3 are the boundary covered in [`contracts/skills.md`](contracts/skills.md). They
are deliberate: a daemon that changes its own form at runtime drifts from its source and
becomes hard to trust.)

---

## 2. System topology

```text
   ┌──────────────────────────────────────────────────────────────────┐
   │  HOST / CONTAINER                                                  │
   │                                                                    │
   │   nine (TUI)        nine "msg"      ← same binary, client mode     │
   │       │ newline-delimited JSON over AF_UNIX socket                 │
   │       └──────────────┬──────────────┘                             │
   │                      ▼                                             │
   │        ┌───────────────────────────────┐                          │
   │        │  DAEMON  (nine, re-exec mode)  │                          │
   │        │  /tmp/nine.sock                │                          │
   │        │                                │                          │
   │        │  Daemon ── AgentWorker(s)      │                          │
   │        │     │          ├─ agent.Loop   │                          │
   │        │     │          └─ its process  │                          │
   │        │  LLM Queue ── Provider ────────┼──► LLM endpoint          │
   │        │  Supervisor                    │   (ollama)               │
   │        │  Plugin Manager   EventSink    │                          │
   │        └──────┬──────────────────┬──────┘                          │
   │               │ HTTP/unix socket │ database/sql (modernc sqlite)   │
   │          shell files http …      ▼                                 │
   │          (plugin subprocs)   SQLite (one file)                     │
   └──────────────────────────────────────────────────────────────────┘
```

Three process kinds:

| Process | Binary | Role | Lifetime |
|---------|--------|------|----------|
| Client  | `nine` | TUI or one-shot request; connects to the socket | per invocation |
| Daemon  | `nine` (re-exec) | owns socket, sessions, queue, plugins, DB connection | long-lived |
| Plugin  | `bin/<name>` or the `nine` binary | one tool provider, HTTP over a Unix socket (an MCP server is one, via the `mcp` bridge) | spawned/killed by daemon |

Consequences that shape every contract:

- **All durable state is in the daemon + its database file. Clients are disposable.**
  Closing a client does not stop work; re-attaching replays what was missed. The daemon
  fails fast if the database is unusable.
- **Plugins are isolation boundaries.** A crashing/hanging plugin is a child process,
  reached only through the manager — never an in-daemon panic.
- **The LLM is reachable only through the queue.** It is the single choke point for
  concurrency and priority.

---

## 3. Work-unit taxonomy

Nine has exactly **two fundamental units of work** — the *session* (the container) and
the *tool call* (the atom) — and **two durable structures** that organize them — the
*goal* and the *workflow*.

### 3.1 Fundamental units

**Session** — an agent loop (`agent.Loop`) that runs turns. Every flavor of autonomous
or interactive work *is* a session; they differ only by who drives the loop and whether
the session is durable:

| Session | Driven by | Durable? |
|---------|-----------|----------|
| **Conversation** | a user, interactively | yes — registered, checkpointed, re-attachable |
| **Goal-pursue** | an idle timer, autonomously | yes — eager-persisted, resumed at boot |
| **Self-reflection** | an idle timer, autonomously | yes — same machinery as pursue |
| **Sub-agent** | a parent tool call, delegated | **no** — transient, runs synchronously inside the call |

The distinction that decides behavior is **durable vs. transient**, not interactive vs. not.
Conversations, goal-pursue, and self-reflection are durable sessions: each is an
`AgentWorker` in the daemon's session registry — the background ones driven by a
process — checkpointed so it survives restart (I5, I7). A **sub-agent is a bare loop run
synchronously inside its parent's tool call** — no process, not in the registry,
not independently resumable. It is a *nested, transient* session.

**Tool call** — the atomic action a session takes. Between session and tool call sits
the **turn**: one `Loop.Run`. A session runs turns one at a time (I1); each turn issues
zero or more tool calls in its ReAct inner loop. The hierarchy is therefore:

```text
session ──► turn ──► tool call
```

**Workflow** — a **ledger of delegated work**, kept by the model *inside* a turn (a row
in `workflows`, whose steps carry `Step.AgentID`). It sits here, beside the sub-agent,
rather than with the goal below, because that is what it records: a workflow step *is* a
sub-agent delegation, and `workflow_*` tool calls are how the model writes down what it
delegated and how far it got.

It is passive. A workflow has **no session, no scheduler, and no driver** —
`workflow.Service` is pure record-keeping — so it advances only when a model calls a
tool. That is the whole difference from a goal (§3.2): a goal is a **machine**, a
workflow is a **record**. Presenting the two as siblings, as this document previously
did, is why they read as duplicates of each other; the axis that separates them is
**autonomy**, not ordering.

### 3.2 Organizing structures

This is a durable structure imposed *over* sessions, and manipulated through tool calls.

- **Goal** — an open-ended *intention* (a row in `goals`, with a sub-goal/sub-work
  children, reached through their `parent_id`). It is not itself a session: the `goals`
  record persists independently of
  whether its background **goal-pursue session** is currently running (a goal can be
  `paused` with no live session). Top-level goals own one pursue session each.

A goal is the only member of this category, because it is the only structure that
**owns a worker and wakes itself**: `agentID == goalID`, an interval or cron decides
when it next runs, and its own pursue session advances it *between* turns. Nothing else
in the taxonomy has a scheduler behind it.

**Workflow used to be listed here and is not one of these.** It has no session and no
scheduler; it is advanced by the calling model inside a turn, which is why it now sits
in §3.1 beside the sub-agent whose delegations it records. The distinction matters
because merging the two — an appealing idea, since both are "an ordered thing a session
works through" — would put a scheduler behind something that must not have one. See
`adr/concept-consolidation.md` §4.

### 3.3 Relationships

```text
Conversation ──creates──► Goal        (open-ended intention; owns a pursue session)
Conversation ──keeps────► Workflow    (a ledger of its own delegations; no session)
Goal         ──spawns───► Goal        (sub-goals, no user approval)
Session      ──delegates► Sub-agent   (run_agent / run_agents, depth-capped — I6)
Workflow step─runs-as───► Sub-agent   (Step.AgentID)
Any session  ──issues───► Tool call   (the atom; may or may not advance a workflow)
Any session  ──writes───► Memory      (progress, results, learned facts)
```

### 3.4 Status vocabularies (normative)

- **Conversation:** `active` (client connected) | `archived` (disconnected, resumable).
- **Sub-agent:** `done` | `failed` | `timed_out`.
- **Goal:** `active` | `paused` | `done` | `archived`.
- **Workflow:** `active` | `done` | `failed` | `cancelled`; steps `pending` | `running`
  | `done` | `failed` | `skipped`.

Finite work is always a **sub-agent** (transient) or a **workflow step** (durable). The
config knob `task_timeout_seconds` bounds sub-agent / `run_agents` execution.

---

## 4. The turn — the central abstraction

The **turn** sits between the two fundamental units of §3 (`session ──► turn ──► tool
call`): it is one `agent.Loop.Run(text)` call, which may make many LLM round-trips
internally (the ReAct inner loop), issue zero or more tool calls, and ends with a final
answer. Every session — interactive or autonomous — does its work as a sequence of
turns. A turn is produced by exactly one of:

- a **user message** (interactive conversation),
- an **idle trigger** from a session routine (reflection, goal pursuit),
- a **notification-prepended** continuation,
- a **sub-agent** invocation (a fresh loop at depth+1).

Two hard rules about turns underlie the whole system:

- **One session = one serial worker = strictly serialized turns** (invariant I1 below).
- **Every turn ends with a checkpoint** (invariant I5 below).

---

## 5. Cross-cutting invariants

These hold across the entire system. Each later contract assumes them; the conformance
checklist tests them. They are the rules that keep an implementation coherent.

- **I1 — One session, one serial worker, serialized turns.** A session's agent loop is
  never used concurrently. A per-session worker runs turns one at a time, enforced by a
  single-slot inbox (a caller submitting a turn waits while one is in flight). An idle
  turn and a user turn can never run at once on the same session.
- **I2 — The LLM is reachable only through the queue.** No component calls a provider
  directly; concurrency (`max_concurrent`) and priority are centralized in the queue.
- **I3 — One database gateway.** A single store object owns the only database handle to
  the SQLite file. All persistence flows through it. Domain services depend on narrow
  repository interfaces, not on the database. The daemon fails fast if the database is
  unusable (it holds primary state). Because SQLite serializes writes, that one handle is
  a writer pool of exactly one connection plus a concurrent read-only pool.
- **I4 — Operational tables are daemon-private.** Agents get K/V, files, vectors, and
  skills as tools. They **never** get `conversations`, `goals`, `workflows`,
  `notifications`, `user_notifications`, `processes`, the HITL tables
  (`human_requests`, `interactive_sessions`), or the journal tables (`session_events`,
  `event_cursors`, `related_sessions`) as tools. Those are reached only via
  daemon-internal methods. (Enforces N4.)
- **I5 — Every turn ends with a checkpoint.** `{history, scratchpad}` is persisted after
  every turn, so a session is fully reconstructable from its checkpoint plus its session
  plan. This is what makes attach and restart-survival work.
- **I6 — Sub-agent recursion is depth-capped.** Delegation tools are unavailable at
  depth ≥ 2, so delegation cannot spiral.
- **I7 — Background autonomy is resumable.** Processes are persisted and started at
  daemon boot; ordinary conversations are attach-on-demand.
- **I8 — Display names never reach the LLM.** Human-friendly tool display names are for
  the client UI only; the model sees canonical tool names.
- **I9 — Plugins are isolation boundaries.** A plugin crash is contained to its
  subprocess; the daemon and active sessions continue. Recovery is restart-from-binary,
  never recompilation.
- **I10 — Self-improvement is data, not code.** The only self-modification is writing
  skill files. (Enforces N1–N3.)
- **I11 — The journal is append-only; reactions are out-of-band.** Every execution step is
  recorded to `session_events` (never mutated). Journal subscribers run off the turn path
  and may only enrich *derived* stores — they make no generative LLM call and never mutate
  an active session (enrich, don't interject). See [`event-journal.md`](contracts/event-journal.md)
  and [`subscriptions.md`](contracts/subscriptions.md).

---

## 5.1 Concurrency vocabulary (language-neutral)

The contracts describe coordination in terms of **behavior**, not Go primitives (see the
convention note in [`README.md`](README.md)). The reference implementation realizes each
with the Go mechanism in parentheses; any equivalent in your runtime conforms.

| Term used in contracts | Meaning (the conformance requirement) | Reference mechanism |
|------------------------|----------------------------------------|---------------------|
| **serial worker** | one independent activity that runs a session's turns strictly one at a time | a goroutine |
| **single-slot inbox** | a hand-off of capacity 1: submitting a turn while one is in flight waits; this is what serializes turns (I1) | a buffered-1 channel |
| **bounded buffer / queue** | a fixed-capacity queue. Every use states its overflow discipline explicitly — **block** (sender waits) or **drop** (event discarded, never back-pressures) | a buffered channel |
| **priority-ordered queue** | waiting items are served by ascending priority *value* (lowest value = highest urgency) | a min-heap |
| **wait-for-first-of** | block until the earliest of several events occurs (an inbox item, a timer firing, a cancellation, an answer) | `select` |
| **cancellation handle** | a propagating signal that aborts in-flight work and frees its resources; may carry a deadline | `context.Context` / `ctx.Done()` |
| **join** | start several activities concurrently and wait for all to finish, collecting their results | `sync.WaitGroup` |
| **guarded** | concurrent access to a field is serialized so it cannot race | `sync.Mutex` |

These terms appear in [`agent-worker.md`](contracts/agent-worker.md),
[`supervisor.md`](contracts/supervisor.md), [`llm-provider.md`](contracts/llm-provider.md),
[`orchestration.md`](contracts/orchestration.md), [`wire-protocol.md`](contracts/wire-protocol.md),
and [`hitl.md`](contracts/hitl.md).

---

## 6. Where to go next

- To build the system: [`build-order.md`](build-order.md).
- To implement a specific boundary: the matching file in [`contracts/`](contracts/).
- To verify an implementation: [`conformance.md`](conformance.md).
