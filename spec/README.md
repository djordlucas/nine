# Nine — Generation Spec

This directory is a **generation playbook**: a complete, implementation-agnostic
specification for building Nine from scratch. It tells you *what* to build, *in what
order*, and *to what contract* — precisely enough that an independent implementation
(in any language) can be produced from these documents alone and checked for
conformance.

This spec is the **normative source of truth**. Where it disagrees with anything in
[`docs/`](../docs/) (which is kept as narrative/explanatory material) or with code
comments, this spec wins.

---

## What Nine is (one paragraph)

Nine is a self-contained AI agent **daemon**. A single long-lived process owns all
state (sessions, a prioritized LLM queue, plugin subprocesses, and a single
**SQLite** database file); the `nine` binary doubles as a thin client and as the daemon itself
(re-exec). Each unit of work is a **session** running a ReAct (reason → act → observe)
agent loop, serialized on its own worker, checkpointed after every turn so it
survives disconnects and restarts. Nine works in the background, pursues
open-ended goals autonomously between user turns, delegates to sub-agents, plans
multi-step work as workflows, reflects on itself on a timer, and improves itself by
writing skills — but it does **not** modify its own code or configuration at runtime.

For the full picture read [`overview.md`](overview.md) first.

---

## How to read this spec

Read in this order:

1. **[`overview.md`](overview.md)** — system topology, goals/non-goals, the work-unit
   taxonomy (conversation / task / goal / workflow / sub-agent), and the cross-cutting
   **invariants** every implementation must hold. Start here.
2. **[`build-order.md`](build-order.md)** — the playbook proper: an ordered set of
   build phases. Each phase states its goal, the components it introduces, how they are
   wired, and an **acceptance gate** that must pass before moving on. Each phase points
   at the contracts it depends on.
3. **[`contracts/`](contracts/)** — the stable interface boundaries the phases build
   against. One file per boundary. These are the parts an implementation may not
   reinterpret freely; they define wire formats, schemas, and observable behavior.
4. **[`conformance.md`](conformance.md)** — the full acceptance checklist: every
   numbered requirement mapped to an observable test. An implementation "is Nine" when
   it passes this.

If you only want to build one subsystem, jump to its contract and follow its
"Depends on" / "Used by" links.

---

## Contracts index

| Contract | Boundary it defines |
|----------|---------------------|
| [`wire-protocol.md`](contracts/wire-protocol.md) | Daemon ↔ client messages over the Unix socket; progress events; `EnsureDaemon`/re-exec |
| [`memory-store.md`](contracts/memory-store.md) | The single SQLite gateway: all tables, the agent/daemon access split, checkpoints |
| [`llm-provider.md`](contracts/llm-provider.md) | `Provider` interface (single `Complete` method) and the prioritized concurrency `Queue` in front of it |
| [`embedder.md`](contracts/embedder.md) | `Embedder` interface, providers, vector namespaces, where embeddings are used |
| [`plugin.md`](contracts/plugin.md) | Plugin contract (HTTP over a Unix socket; MCP over stdio), the plugin manager lifecycle, default plugins |
| [`event-journal.md`](contracts/event-journal.md) | The append-only `session_events` journal: producer sink, trace/replay, retention |
| [`subscriptions.md`](contracts/subscriptions.md) | Journal subscriptions (durable cursors, out-of-band), related-session indexer, pull surfacing |
| [`agent-loop.md`](contracts/agent-loop.md) | The ReAct loop: scratchpad, retries, empty-answer fallback, the checkpoint unit |
| [`dispatcher.md`](contracts/dispatcher.md) | Tool routing, the plugin/core taxonomy, depth-capping, post-call hooks, output cap |
| [`context-builder.md`](contracts/context-builder.md) | Per-turn context assembly: priority budget, tool relevance filtering, token model |
| [`agent-worker.md`](contracts/agent-worker.md) | Per-session serial worker: turn pipeline, stall detection, notifications, the replay buffer |
| [`session-plans.md`](contracts/session-plans.md) | The stage state machine, idle scheduler, `active`/`idle-reflection`/`pursue`, self-model |
| [`supervisor.md`](contracts/supervisor.md) | The oversight event loop, stall/gap handling, plugin-crash handling |
| [`orchestration.md`](contracts/orchestration.md) | Sub-agents (`run_agent`/`run_agents`), workflows, and goals — tools and data models |
| [`skills.md`](contracts/skills.md) | Skill file format, search/read/write tools, the self-improvement boundary |
| [`self-documentation.md`](contracts/self-documentation.md) | Addressing, chunking, and indexing of the bundled docs/spec; the `doc_search`/`doc_read` guarantees |
| [`roles.md`](contracts/roles.md) | Worker kinds as data: role frontmatter, tool allowlists, structural wiring, delegation-time selection |
| [`hitl.md`](contracts/hitl.md) | Human-in-the-loop: `ask_human`, approval gates, interactive-session gating |
| [`config.md`](contracts/config.md) | `nine.toml` reference, config resolution order, volume layout |

---

## Conventions

- **Requirement keywords** follow [RFC 2119](https://www.rfc-editor.org/rfc/rfc2119):
  **MUST** / **MUST NOT** are hard conformance requirements; **SHOULD** is a strong
  recommendation an implementation may deviate from only with cause; **MAY** is
  optional. Anything not phrased with these keywords is explanatory.
- **Requirement IDs** are stable: `R-<area>.<n>` (e.g. `R-LOOP.3`). They are referenced
  from [`conformance.md`](conformance.md). Do not renumber existing IDs; append.
- **Symbol names** (`AgentWorker`, `Dispatch`, `memory.Store`) refer to the reference
  Go implementation in this repository and are given as a navigation aid. A conforming
  implementation **MAY** use different internal names; it **MUST NOT** change observable
  contracts (wire formats, schemas, tool names, DB-visible behavior).
- **Concurrency is specified behaviorally, not by mechanism.** Where a contract names a
  Go primitive — goroutine, channel, `select`, min-heap, mutex, `WaitGroup`, `ctx.Done()`
  — it is naming the *reference* mechanism, never mandating it. What conforms is the
  **observable behavior** stated alongside it: serialization, single-slot hand-off,
  bounded buffering with a named overflow discipline (block or drop), priority ordering,
  cancellation, and join-on-completion. Any concurrency model that preserves that behavior
  — async tasks on an event loop, threads with locks, actors, fibers, coroutines — is
  conformant. The language-neutral terms used throughout are defined in
  [`overview.md`](overview.md) § 5.1.
- **Constants** stated in this spec (timeouts, caps, intervals) are the reference
  defaults and **MUST** be the defaults of a conforming implementation unless the
  contract marks them configurable.
- Code fences labeled `interface`, `schema`, or `sql` are normative; those labeled
  `text` or unlabeled diagrams are illustrative.

---

## Status of the reference implementation

These contracts describe the system **as currently built** in this repository. A few
load-bearing facts worth stating up front:

- Stall detection fires at **5** consecutive no-tool turns.
- `run_agents` default group timeout is the daemon task timeout, **1800s / 30 min**.
- There is **no runtime plugin generation, config rewrite, or core rebuild**. Plugins
  are immutable image content; self-improvement is skills only.
- Memory/file/vector operations are **core-intercepted** in-process, not a subprocess plugin.
- The store is a single **SQLite** file; the daemon fails fast if it is unusable.
  Native plugins use **HTTP over a Unix socket** (MCP uses stdio). The `Provider`
  interface is a **single** `Complete` method (streaming via `OnChunk`).
- The supervisor bus is **durable** — journal-backed and resumable.
- Every session is journaled to `session_events`; the journal is subscribable
  ([`event-journal.md`](contracts/event-journal.md), [`subscriptions.md`](contracts/subscriptions.md)).

Each contract carries a **Status** line — `Built` (present in the reference tree),
`Partial`, or `Planned` (specified as part of the target design but not yet implemented).
**All contracts are currently `Built`** (HITL now ships: `internal/runtime/hitl.go`,
`internal/agent/register_human.go`).
