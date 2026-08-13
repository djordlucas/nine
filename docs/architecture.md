# Architecture

## Overview

Nine is structured as a daemon/client pair. The daemon holds all long-lived state; the CLI is a thin client that opens a connection, sends a message, and waits for a response.

```
┌─────────────────────────────────────────────────────┐
│  nine <message>  (CLI client)                       │
│  Connects to Unix socket, sends message, prints reply│
└─────────────────┬───────────────────────────────────┘
                  │ JSON over Unix socket
┌─────────────────▼───────────────────────────────────┐
│  Daemon                                              │
│  ┌──────────────┐   ┌───────────────┐               │
│  │ Conversation │   │  Supervisor   │               │
│  │  Manager     │   │  Agent        │               │
│  └──────┬───────┘   └───────┬───────┘               │
│         │ spawns             │ monitors              │
│  ┌──────▼───────────────────▼───────┐               │
│  │           Agent Loop             │               │
│  │  (ReAct: reason → act → observe) │               │
│  └──────────────┬───────────────────┘               │
│                 │                                    │
│  ┌──────────────▼───────────────────┐               │
│  │         LLM Queue                │               │
│  │  priority: supervisor > active   │               │
│  │           > background           │               │
│  └──────────────┬───────────────────┘               │
│                 │                                    │
│  ┌──────────────▼───────────────────┐               │
│  │         LLM Provider             │               │
│  │  (Ollama)                        │               │
│  └──────────────────────────────────┘               │
│                                                      │
│  ┌────────────────────────────────────────────────┐ │
│  │  Plugin Manager                                │ │
│  │  shell  files  http  time  browser             │ │
│  │  (separate subprocesses; HTTP over unix socket) │ │
│  └────────────────────────────────────────────────┘ │
│                                                      │
│  ┌────────────────────────────────────────────────┐ │
│  │  memory.Store  →  SQLite (one file)             │ │
│  │  (in-process; all durable state + the event     │ │
│  │   journal; memory/file/skill tools are core,    │ │
│  │   not plugins)                                   │ │
│  └────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
```

> Memory, files, and skills are **core, in-process** capabilities backed
> directly by `internal/memory.Store` (SQLite), not plugin subprocesses. The
> plugin subprocesses are `shell`, `files`, `http`, `time`, and the optional
> `browser`. Note the `files` *plugin* provides workspace filesystem access
> (`read_file` / `write_file`), which is distinct from the in-process, durable
> `files` table (`file_store` / `file_fetch` / `file_list` / `file_search_text`).

---

## Components

### Daemon (`internal/runtime/`)

The daemon is the central orchestrator. It:

- Listens on a Unix domain socket for incoming client connections
- Creates or resumes `AgentWorker` instances per agent (one per session)
- Routes messages by agent ID
- Delivers pending notifications (from background sessions) to the next active conversation turn
- Detects stalled agents and notifies the supervisor
- Writes every session's execution trajectory to the durable event journal via an async batched `EventSink`, and hosts optional out-of-band journal subscribers
- Exposes goal, workflow, and plugin status to the `nine goals` / `nine workflows` / `nine status` commands, and the journal to `nine trace` / `nine replay`

The daemon is single-process. All agents share the same plugin registry and LLM queue.

### Agent Loop (`internal/agent/loop.go`)

Each active conversation runs one agent loop. The loop implements the **ReAct** pattern:

```
┌─────────────────────────────────────────────────────┐
│  Assemble turn (context builder)                    │
│    → system prompt                                  │
│    → tool definitions (relevance-filtered)          │
│    → message history                                │
│    → scratchpad                                     │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  Submit to LLM queue                                │
└──────────────────────┬──────────────────────────────┘
                       │
┌──────────────────────▼──────────────────────────────┐
│  LLM returns response + optional tool calls         │
└──────────────────────┬──────────────────────────────┘
                       │
           ┌───────────┴──────────┐
           │                      │
    tool calls?               final answer
           │                      │
┌──────────▼────────┐      ┌──────▼───────────────────┐
│  Dispatcher       │      │  Return to user           │
│  route each call  │      │  Checkpoint state         │
│  to plugin / core │      └──────────────────────────┘
└──────────┬────────┘
           │
┌──────────▼────────┐
│  Append result to │
│  scratchpad       │
└──────────┬────────┘
           │
           └──── (loop back to top)
```

The scratchpad accumulates tool calls and their outputs within a single turn. After a final answer, the full turn (human + scratchpad + assistant) is committed to message history and persisted as a checkpoint.

### Context Builder (`internal/context/builder.go`)

Each LLM request is assembled from multiple sources that compete for a fixed `context_budget` (tokens). The builder uses a priority queue:

| Priority | Content | Behavior |
|----------|---------|----------|
| 1 (must) | System prompt core | Always included |
| 2 | Tool definitions | Capped at top-N; always-include (core) tools always present |
| 2.5 | Self-model | Capped (~600 tokens); omitted when budget is tight |
| 2.6 | Enrichment (related prior session) | Capped (~300 tokens); dropped first when budget is tight ([reactive events](reactive-events.md)) |
| 3 | Message history | Rolling window; oldest messages trimmed first |
| 4 | Scratchpad | Oldest entries trimmed first |
| 5 (optional) | System extras | Dropped if over budget |

**Tool selection** caps the advertised set at top-N with always-include (core)
tools always present, and orders the rest by cosine similarity between the user's
query and each tool's description embedding. The `AgentBuilder` embeds each tool's
description once (cached by tool name, since descriptions are static after boot)
and sets it on the `ToolWithVector`, so ranking is live whenever an embedder is
configured; with embeddings disabled (`provider = "none"`) vectors are nil and
selection falls back to always-tools-first plus insertion order under the cap.

Token counting uses a 4-characters-per-token approximation, which is accurate enough for budget allocation without requiring a tokenizer.

### LLM Queue (`internal/llm/`)

The queue serializes and prioritizes LLM requests across all concurrent agents:

| Priority | Agent type |
|----------|-----------|
| 1 (highest) | Supervisor agent |
| 2 | Active conversations (user is waiting) |
| 3 | Background tasks and goals |

`max_concurrent` in `nine.toml` controls how many requests run in parallel. For local Ollama models, set this to `1`.

The LLM layer is a thin interface. Adding a new provider requires implementing a single method — `Complete(ctx, Request) (Response, error)` — where streaming is delivered through the request's `OnChunk` callback rather than a separate method.

### Plugin Manager (`internal/plugin/manager.go`)

Plugins are standalone executables. The manager:

1. **Spawns** the plugin process (passes `NINE_SRC`, `NINE_BIN`, and any custom env vars)
2. **Describes** — calls `plugin.describe` over JSON-RPC to retrieve tool definitions
3. **Registers** each tool with the dispatcher
4. **Stops** — sends `SIGTERM` and waits for clean exit

If a plugin subprocess crashes, the failure is isolated to that plugin; restarting it from its existing binary is the manager's responsibility and does not affect the daemon or active conversations.

### Tool Dispatcher (`internal/agent/dispatcher.go`)

The dispatcher routes tool calls from the agent to the appropriate handler:

- **Plugin tools**: Forwarded to the plugin via `plugin.call` JSON-RPC
- **Core-intercepted tools**: Handled directly in the daemon:
  - `gap_report` — signals the supervisor of an unresolvable capability gap
  - `memory_embed`, `memory_query`, `file_search_semantic` — embedding-backed memory operations
  - `run_agent` / `run_agents` — sub-agent delegation
  - `workflow_*` / `goal_*` — multi-step plans and open-ended goals

Tool output is capped at ~2048 tokens before being appended to the scratchpad. Longer output is **spilled** to the memory file store under `spill/<agent-id>/` and replaced with a head+tail preview naming the path, so the model can read the rest on demand (`file_fetch` with `offset`/`limit`) or hand it to another tool by reference. See [tool-output-spill.md](tool-output-spill.md).

---

## Memory and Persistence

All persistent state lives in a single **SQLite** database file, reached through
`internal/memory.Store`. The store opens `[memory].path` (default
`~/.nine/nine.db`, or `/data/nine.db` in the container), creating the file and its
parent directory if absent, fails fast if it is unusable (it holds primary state,
so an unopenable database is a startup error, not a degraded mode), and applies
its schema idempotently on `Open` with `CREATE TABLE IF NOT EXISTS` (there is no
migration runner; `PRAGMA user_version` records a generation). The driver is
`modernc.org/sqlite` via `database/sql` — a pure-Go translation of SQLite, so the
build needs no cgo.

Because SQLite serializes writes, the `db` wrapper holds two pools — one
read-write connection and a concurrent read-only pool — and routes each statement
by its leading keyword. The database runs in WAL mode, so `nine trace` can read a
live database from a second process without blocking the daemon. The schema:

| Table | Purpose |
|-------|---------|
| `kv` | Key-value store for agent memory (`memory_get/set/delete/list`) |
| `files` | File content, full-text indexed by a companion FTS5 table kept in sync by triggers |
| `vectors` | Text embeddings as packed float32 blobs; nearest-neighbour query by cosine similarity |
| `conversations` | Message history, scratchpad, agent status |
| `goals` | Open-ended goals with their sub-goal/sub-work subtree |
| `notifications` | Pending push notifications to active conversations |
| `user_notifications` | Human-facing notification feed posted by background agents (`nine notifications`) |
| `reflections` | Idle reflection summaries (one per reflection run) |
| `workflows` | Named multi-step execution plans with step statuses |
| `session_plans` | Per-session stage state (`active` / `idle-reflection` / `pursue`) and idle-scheduling config |
| `skills` | Built-in (seeded) and agent-authored skills, by source |
| `human_requests`, `interactive_sessions` | Human-in-the-loop request/answer state |
| `session_events` | Append-only execution journal, one row per step ([event log](event-log.md)) |
| `event_cursors` | Each journal subscriber's durable position ([reactive events](reactive-events.md)) |
| `related_sessions` | Derived cross-session links maintained by the related-session subscriber |

There is no `tasks` table (finite work is a sub-agent or a workflow step) and no
`plugin_registry` table (plugins are immutable image content). Operational tables are
reached only through daemon-private methods on the store — never advertised to agents as
tools — so an agent cannot directly manipulate conversation, goal, or workflow state.

Full-text search over `files` uses FTS5, ranked by `bm25` and returning highlighted
fragments via `snippet`. Because FTS5's query parser rejects malformed input where
the previous engine accepted anything, every user term is emitted as a quoted
literal: a search can return no results, but never a syntax error. Vector search
stores embeddings as packed float32 blobs and ranks by cosine similarity computed
in process — the previous backend had no approximate-nearest-neighbour index
either, so this is the same scan, on the other side of the driver boundary.

---

## Workflows

Workflows are persistent execution plans the LLM creates before delegating multi-step work to sub-agents.

### Package boundary

Workflow domain logic (creating plans, advancing steps, dependency gating, auto-close,
operator actions) lives in `internal/workflow` as a `Service` that depends only on a
narrow `Repository` interface (`Insert/Load/Save/ListActive/ListRecent/Notify`) — it
holds no database reference and can be tested without one (see
`internal/workflow/workflow_test.go`, which exercises it against a fake repository).

`internal/memory` remains the sole owner of `*sql.DB`: it implements `Repository` via
a `sqlWorkflowRepo` adapter and exposes the familiar `Store.Workflow*` methods as thin
delegations to a `*workflow.Service`. This keeps the "single database gateway" invariant
intact while moving the actual domain rules out of the persistence layer.

### State

Workflow state lives entirely in the `workflows` table. Steps are stored as a JSON array on the workflow row rather than as separate rows — this keeps reads and writes simple (load the row, update one step, write back) with no join overhead for the common case of reading the full plan.

Each step records the sub-agent that executed it via its `agent_id` field, so a step is linked to its execution without any separate table.

### Write paths

Two write paths exist:

1. **LLM tools** — `workflow_create`, `workflow_update`, `workflow_get`, `workflow_list`, `workflow_retry_step`. These are core-intercepted tools registered via `RegisterWorkflowTools` in the dispatcher. Available to delegating roles only, with the delegation depth guard as backstop (see [Roles](roles.md); gated the same way `run_agent` is).

2. **Operator commands** — `WorkflowCancel` (stop), `WorkflowFail` (post-mortem). These are daemon-private methods on the in-process store, invoked in response to `workflow_stop` / `workflow_fail` daemon messages, or directly by the CLI (which opens the store in-process via `memory.Open`) when the daemon is down.

### Auto-close

When `workflow_update` marks a step as `done` or `failed`, it checks whether all steps are terminal. If so, the workflow is automatically closed as `done` (all succeeded) or `failed` (any step failed). The LLM does not need an explicit close call.

### Startup scrub

On daemon start, `internal.workflow.scrub` marks all `running` steps as `failed` with reason `interrupted`. This handles ungraceful shutdowns where the daemon was killed while sub-agents were running. Workflows that still have `pending` steps are left `active` so the LLM can resume them.

See [Workflows](workflows.md) for user-facing documentation.

---

## Session Plans & Stages

Every session (`AgentWorker`) has a persistent **session plan**: a small state
machine of **stages** that hook into the turn loop (`OnTurnEnd`) and a per-stage idle
scheduler (`OnIdle`). This is the mechanism behind autonomous, between-turns behavior
— the dedicated self-reflection session and each goal's background "pursue" session
are both just stages running on this framework.

Plan/stage state lives in the `session_plans` table. Ordinary conversations get a
trivial `active` stage with no idle work; `idle-reflection` and `pursue` are
idle-capable stages that get their own background sessions, persisted eagerly and
resumed automatically on daemon restart.

See [Session Plans & Stages](session-plans.md) for the full design: the
`StageHandler` interface, idle scheduling, stall interaction, and the built-in
`active` / `idle-reflection` / `pursue` stages.

---

## Goals

Goals are persistent, open-ended intentions with no defined end condition (e.g. "monitor this repo for security issues"), as opposed to workflows, which are finite plans. The LLM creates and decomposes them autonomously — no user approval is required to spawn sub-goals or tasks.

### State

Goal state lives in the `goals` table: a description, status, an optional `parent_id`/`parent_type` (the conversation or goal that spawned it, null for top-level goals), and an append-only `subtree` JSON array recording the sub-goals and sub-work it has spawned.

### Write paths

1. **LLM tools** — `goal_create`, `goal_get`, `goal_list`, `goal_update_status`, `goal_append_subtree`. These are core-intercepted tools registered via `RegisterGoalTools` (`RegisterGoalCreate` + `RegisterGoalManagement`) in the dispatcher. Available to delegating roles only (gated the same way `run_agent` and the workflow tools are; see [Roles](roles.md)). `goal_create` defaults `parent_id`/`parent_type` to the owning conversation when no parent is given.

2. **Daemon read path** — `list_goals` is a thin daemon message handler that proxies to `store.GoalList()` so the CLI/TUI (`nine goals` / `/goals`) can list goals without an agent loop running. The daemon does not otherwise interpret or act on goal data.

### Status values

`active` | `paused` | `done` | `archived` (`done` = explicitly completed or resolved; `archived` = retired without completion).

### Background pursuit (pursue sessions)

Every top-level goal (`parent_type: "conversation"`) created via `goal_create` gets a
dedicated background session running the `pursue` stage (see
[Session Plans & Stages](session-plans.md)), keyed 1:1 by `agentID == goalID`. This
session wakes every 5 minutes to assess and act on the goal between user turns, and
syncs the stage's status from `goals.status`.

Spawning is capped by `daemon.max_goal_sessions` (default 10, concurrently-running
`pursue` stages). `goal_create`'s response includes `pursue_session: "spawned"` or
`"limit_reached"` accordingly; the goal itself is recorded either way. Sub-goals
(`parent_type: "goal"`) do not get their own session.

---

## Supervisor Agent

The supervisor is a special agent with elevated capabilities. It runs alongside regular conversations and monitors for problems.

**When the supervisor activates:**
- An agent calls `gap_report` (reports a capability it cannot fulfill)
- Stall detection fires (N consecutive turns without any tool calls)

The supervisor has higher LLM queue priority than background tasks but lower than user-facing conversations. Its control-plane bus is durable: lifecycle events are appended to the journal (`supervisor` event type) and consumed via a resumable cursor, so its reactions survive a restart.

---

## Event Journal & Subscriptions

Every session's execution trajectory — turn boundaries, exact LLM request/response,
tool I/O with latency and errors, context usage, sub-agent lifecycle — is written to
an append-only journal (the `session_events` table) through an async batched
`EventSink` off the turn's critical path. The journal is the substrate for three
things:

- **Observation & replay.** `nine trace <agent-id>` reads a session's trajectory
  (works with the daemon down); `nine replay` deterministically re-executes a
  recorded session on a real loop wired to a recorded provider/dispatcher
  (`internal/replay`), with no live LLM or tool calls. Journal-backed reattach lets a
  revived session show its real history.
- **Retention.** A boot-time scrub bounds journal growth: keep the last N turns per
  agent and/or drop events older than a max age (`[daemon] event_retention_turns` /
  `event_retention_days`).
- **Subscriptions.** The journal is *subscribable*: durable per-subscriber cursors
  (`event_cursors`) over `seq`, in-process wake plus catch-up after restart. Subscribers
  are programmatic, out-of-band handlers that **enrich derived stores** — never a
  generative LLM call, never a write into the active session. The first one, the
  related-session indexer, links topically-similar sessions (by vector similarity) into
  `related_sessions`; a later user turn *pulls* that link into context under the token
  budget (enrich, don't interject). On by default when an embedder is configured
  (`[daemon] related_sessions_index`).

See [Event log](event-log.md) and [Reactive events](reactive-events.md) for the full
designs.

---

## Self-Improvement

Nine improves itself by writing its own **skills** (markdown how-to notes stored
in the memory DB) and nothing more. It does not generate plugins, rewrite its
configuration, or rebuild its own source at runtime — its executable shape is
fixed. See [Self-Improvement & Boundaries](self-modification.md) for the
rationale and what is and isn't possible.

| What changes | Mechanism | Restart required | User approval |
|-------------|-----------|-----------------|---------------|
| Agent skills | `skill_write` / `skill_modify` (memory store) | No | No |

Built-in skills are immutable at runtime (edited in the repo + rebuild).
Configuration changes are made by editing `nine.toml` and restarting the daemon.

---

## Plugin Protocol

Native plugins speak a small request/reply protocol over **HTTP on a per-plugin
Unix socket**. The daemon spawns each plugin with `NINE_PLUGIN_SOCKET` set; the
plugin (`plugin.Serve`) listens there and answers `POST /rpc`. HTTP gives free
per-request concurrency (one goroutine per request), connection pooling, and
per-request cancellation/deadlines from `context` — see
[Plugins — HTTP transport](plugins-http-transport.md). Each call owns its
connection, so there is no JSON-RPC `id`. (External **MCP** servers are the
exception: they speak JSON-RPC 2.0 over stdio, kept on the legacy stdio client.)

The request envelope is `{"method": ..., "params": ...}`; the reply is
`{"result": ...}` or `{"error": {"code": ..., "message": ...}}`.

### `plugin.describe` (called once at startup)

Request → `POST /rpc`:
```json
{"method": "plugin.describe", "params": {}}
```

Reply:
```json
{
  "result": {
    "protocol_version": 1,
    "max_concurrent": 0,
    "tools": [
      {
        "name": "my_tool",
        "description": "Does something useful",
        "inputSchema": {
          "type": "object",
          "properties": {"input": {"type": "string", "description": "The input value"}},
          "required": ["input"]
        }
      }
    ]
  }
}
```

### `plugin.call` (called per tool invocation)

Request → `POST /rpc`:
```json
{"method": "plugin.call", "params": {"tool": "my_tool", "args": {"input": "hello"}}}
```

Reply:
```json
{"result": {"output": "hello, world"}}
```

Error:
```json
{"error": {"code": -1, "message": "something went wrong"}}
```
