# Session event log — full-fidelity execution persistence & replay

**Status:** Proposed (investigation report) · **Depends on:** loop, dispatcher, session workers, checkpoints, memory · **Overlaps:** checkpointing, the in-memory replay buffer, the supervisor event bus, `slog` logs, the `X-Nine-Request-ID` trace

> **Superseded in part — backend.** This report was written when the store was
> being moved onto PostgreSQL, and its phase log records that as it happened. The
> store has since moved to **SQLite** (one file, no server); the journal's design —
> append-only, monotonic `seq`, durable per-subscriber cursors — carried over
> unchanged, because it never depended on the engine. Where this document describes
> the *backend*, read [`../spec/contracts/memory-store.md`](../spec/contracts/memory-store.md)
> and [`../spec/contracts/event-journal.md`](../spec/contracts/event-journal.md) as
> current; the design reasoning below stands on its own.

This report investigates persisting **every step** of a Nine session's
execution — tool inputs/outputs, model replies, the exact context sent to the
model and how it changed — as a durable, ordered, queryable record that makes
sessions **replayable** and **debuggable**. It maps what is already persisted,
identifies the gaps, weighs an append-only event log against a graph model,
recommends a design, and spells out the seams and the overlap with the existing
checkpoint feature.

The same journal is designed to double as an **event-sourcing substrate for
Nine's internal processing** — not only an observability record of what a
separately-authoritative state machine did, but (selectively) the source of
truth that internal state is *projected from*. §8a makes this a first-class
design goal and sets the incremental path (supervisor-bus-first).

---

## 1. TL;DR — findings and recommendation

**Finding: the *outcome* of a session is persisted; the *process* that produced
it is not.** The `conversations` table durably stores the materialized
conversation (a JSON array of user/assistant messages). But the actual
execution trajectory — each inner LLM call, the exact request assembled for it,
the model's raw reply, every tool call's input/output/latency/error, context
trimming decisions, sub-agent trees — is either **ephemeral** (emitted as
in-memory events to a connected client and a bounded 200-entry ring) or
**partial and unstructured** (`slog` lines, mostly at `Debug`, without the
request/response bodies). After a daemon restart, all of it is gone.

**Sharpest finding: a turn's intra-turn tool trajectory is never persisted at
all.** The ReAct loop keeps tool calls in a *scratchpad* that it **clears on
turn success** (`loop.go` `Run`), and history only ever receives the opening
user message and the final assistant answer. So once a turn completes, the
sequence of tool calls that produced the answer exists in **no** durable
structure — only in the transient event stream. A crash mid-turn loses the
entire turn, including the record of tool side effects that already ran.

**Recommendation: add an append-only `session_events` journal** (event-sourcing
lite), written through a single new `EventSink` seam that the session worker
already funnels every event through, plus two new loop-level hooks to capture
the currently-unpersisted LLM request/response. Keep the `conversations`
checkpoint as a materialized **snapshot** for fast resume — the journal and the
snapshot are the standard journal+snapshot pairing, not competitors. Model
sub-agent trees with `span_id`/`parent_span_id` columns on the flat log rather
than a separate graph store; a flat append-only log with parent pointers **is**
a DAG when the viewer needs one. Frame "replay" as **observational** (always
available, the primary debugging win) plus **deterministic re-execution from
recorded responses** (for tests), explicitly *not* re-hitting live LLMs/tools.

Effort is moderate and cleanly phaseable; the risky parts are storage backend
throughput and payload size, both addressed in §9.

---

## 1a. Decisions taken (2026-07-06)

- **Backend: PostgreSQL, assembled via docker-compose — the whole store.**
  The **entire** `memory.Store` lives on Postgres, not just the journal (scope
  resolved below), because event volume needs real concurrent writers. This is a
  **prerequisite phase 0**: the event log lands on this backend. Postgres gives
  concurrent writers, `JSONB` payloads, and partitioning/retention tooling.
- **Postgres is a hard dependency (fail-fast).** The daemon refuses to start
  without a reachable database — Postgres now holds primary state, so a complete
  and available store is an invariant, not best-effort. This retires the
  "best-effort sink that never fails a turn" stance from §7 for *durability of
  primary state*; the event `Append` may still be async/batched for throughput,
  but a persistent write failure is a real error, not silently dropped.
- **Fidelity: full request + full response** every inner LLM call (§6). Content-
  addressed dedup is a later optimization, not a v1 constraint.
- **Retention: prune-below-checkpoint for closed sessions** (§9), with a boot
  scrub.
- **First slice: model I/O up front** — v1 and v2 (§11) merge, so the first
  deliverable captures tool trajectory *and* the exact `llm_request`/
  `llm_response`, the biggest gap.
- **Event sourcing is an integrated design goal, adopted incrementally** (§8a).
  The journal is built so internal state can be *projected* from it, starting
  with persisting the supervisor event bus and
  making one aggregate — goal lifecycle — projection-driven, before any broader
  CQRS. This constrains the design now (a two-tier durability model for events,
  §8a), even though full adoption is later and selective.

**Scope (resolved): the whole store.** The entire `memory.Store` — kv, files,
vectors, conversations, goals, notifications, reflections, workflows,
session_plans, human_requests, user_notifications — lives on one backend, with the
`session_events` journal as one more table on it. (That backend was Postgres when
this was written and is SQLite today: files are full-text indexed by FTS5 and
vectors are ranked by cosine similarity over stored blobs.) The backend swap was
scoped as its own effort (§11 phase 0) — the event log is deliberately *downstream*
of it.

---

## 2. What is persisted today (and what isn't)

| Execution detail | Emitted? | Persisted durably? | Where |
|---|---|---|---|
| Final conversation (user/assistant text) | — | **Yes** | `conversations.history` (overwritten each turn) |
| Post-turn scratchpad | — | Yes, but **always empty** post-turn | `conversations.scratchpad` |
| Intra-turn tool calls (the ReAct trajectory) | as events | **No** | cleared from scratchpad on success |
| Exact `llm.Request` sent (system, window, tool set) | no | **No** | rebuilt per inner call, never stored |
| Raw `llm.Response` (text, tool_calls, stop_reason) | no (only counts) | **No** | `slog.Debug` records counts only |
| Tool input | as `tool_start` event + `slog.Info` | **No** (log only) | `dispatchWithRetry` logs input |
| Tool output | as `tool_end` event | **No** (log records length only) | in-memory only |
| Tool latency / retries / error | partially logged | **No** | `slog` only |
| Context usage (tokens used/budget) | as `context_update` event | **No** | in-memory only |
| "Thinking" (inner LLM call N) | as `thinking` event | **No** | in-memory only |
| Streamed response chunks | as `response_chunk` event | **No** | in-memory only |
| Sub-agent start/end tree | as events | **No** | `SubAgents()` live only, gone on finish |
| notify_user / goal mutations | tool side effects | Indirectly (feed/goals rows) | not as timeline |

Key mechanics behind the table:

- **Checkpoint is a destructive snapshot.** `AgentWorker.processTurn` →
  `checkpoint()` → `loop.SaveState()` → `Store.ConversationSave` **overwrites**
  `history` and `scratchpad` every turn (`agent_worker.go`,
  `conversations.go`). There is no prior-state history; the row is the latest
  fold only.
- **Rich detail is emitted, not stored.** `processTurn` wires the loop's
  `onToolStart/onToolEnd/onContextUpdate/onChunk/onThinking` callbacks to
  `emitEvent`, which (a) forwards to a connected client's progress stream and
  (b) pushes a subset (`tool_start`, `tool_end`, `sub_agent_start`,
  `sub_agent_end` — **not** `context_update`, `thinking`, `response_chunk`) to a
  bounded 200-entry in-memory `replayBuffer` for reattach. Nothing reaches disk;
  the buffer dies with the process.
- **The model's I/O is the biggest hole.** The assembled `llm.Request` (system
  prompt with injected current-time, the budget-trimmed message window, the
  embedding-ranked tool subset, the self-model) is built fresh in `loop.Run`
  for every inner call and discarded. The raw `llm.Response` is only summarized
  in a `Debug` log (`n_tool_calls`, `has_text`). To answer "why did the model
  call that tool?" you need the exact request and reply — today unrecoverable.
- **Logs are not a substitute.** `slog` is unstructured, mostly `Debug`
  (off by default), interleaves all sessions, and deliberately omits large
  bodies. Useful for live grepping (aided by `X-Nine-Request-ID`), not for
  reconstruction or replay.

## 3. What "full replayability" requires

Two distinct capabilities, often conflated:

1. **Observational replay (deterministic, always works).** Reconstruct and
   inspect exactly what happened — step through a session turn by turn: the
   request sent, the model's reply, each tool call's I/O, what got trimmed and
   why. This is pure read-back of recorded facts; it is the **primary debugging
   value** and needs no re-execution.
2. **Re-execution replay (inherently nondeterministic live).** Actually re-run
   the loop from a point. Live re-execution will **not** reproduce a past run:
   the LLM is nondeterministic and drifts across model versions, and tools have
   side effects (`shell`, `http_post`, file writes) that must not be blindly
   replayed. Deterministic re-execution is only possible by **injecting the
   recorded responses** — a replay `Provider` that returns the logged
   `llm.Response` for each call and a replay dispatcher that returns the logged
   tool output. That is exactly what the test suite's `scriptedProvider` does by
   hand today; an event log makes it automatic (record a real session → replay
   it as a golden-transcript regression test).

The report treats (1) as the goal and (2) as a high-value derived capability for
tests — **not** as "press play and re-run against production services."

## 4. Overlap with checkpointing

The checkpoint (`ConversationState{History, Scratchpad}`) is a **fold** of the
session — the current materialized state. An event log is the **journal** of
what happened. This is the classic event-sourcing relationship: the checkpoint
is reconstructible by folding the relevant events (`turn_start` + final
`assistant` message per turn).

Two ways to reconcile them:

- **Subsume** — derive conversation state by folding the event log; drop the
  `conversations` snapshot. *Rejected:* folding on every resume is slower and
  couples resume correctness to full-log integrity; the snapshot is a cheap,
  proven fast path.
- **Complement (recommended)** — keep `conversations` as a periodic
  **snapshot** for O(1) resume, add `session_events` as the append-only
  journal, and record a `checkpoint` marker event carrying the snapshot's
  `seq`. This is the standard snapshot+journal pairing: resume from the snapshot,
  use the journal for audit/replay/analytics, and prune journal rows older than
  the latest snapshot for closed sessions.

Net: **the event log does not replace the checkpoint; it captures everything the
checkpoint deliberately throws away.**

## 5. Design options

### Option A — Append-only event log (recommended)
One `session_events` table, one row per event, monotonic `seq`. The system
**already emits a well-typed event stream** (`protocol.Msg` progress events +
supervisor `Event`s); this option simply persists that stream durably instead of
only streaming it to clients and a bounded ring. Minimal new concepts, natural
fit, directly enables §3(1) and §3(2).

### Option B — Graph / DAG store
Model execution as typed nodes (turn, llm_call, tool_call, sub_agent) and causal
edges. Genuinely nice for sub-agent trees and branching-what-if analysis. But a
separate graph store is heavier to build and operate, and **the graph is
derivable from a flat log** with `span_id`/`parent_span_id` columns: sub-agents
already form a parent→child tree (`parent_id`/`sub_id` in the existing events).
Recommendation: **don't build a graph store**; put span/parent IDs on flat events
and materialize the tree in the viewer.

### Option C — Hybrid (the recommendation)
Append-only `session_events` journal **+** retained `conversations` snapshot **+**
`span_id`/`parent_span_id` columns for tree reconstruction **+** correlation with
the existing `X-Nine-Request-ID` so daemon events and plugin logs share one ID.
Gets event-sourcing's replayability, keeps the fast snapshot resume, and answers
graph questions without a graph engine.

## 6. Proposed schema & event taxonomy

```sql
-- Phase 0 migrates the rest of the store; this table is the journal.
-- (As built on SQLite: seq is INTEGER PRIMARY KEY AUTOINCREMENT — AUTOINCREMENT
-- is required, since a bare rowid alias reuses ids after a delete and subscribers
-- persist absolute seq values as cursors.)
CREATE TABLE IF NOT EXISTS session_events (
    seq            BIGSERIAL PRIMARY KEY,             -- global causal order
    agent_id       TEXT NOT NULL,                     -- session (conversation/goal) id
    turn           INTEGER NOT NULL,                  -- 1-based turn within the session
    span_id        TEXT NOT NULL,                     -- this unit of work (turn/llm_call/tool_call/sub_agent)
    parent_span_id TEXT,                              -- causal parent; NULL at the turn root
    type           TEXT NOT NULL,                     -- see taxonomy below
    ts             TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload        JSONB NOT NULL DEFAULT '{}'        -- type-specific; full llm_request/response
);
CREATE INDEX IF NOT EXISTS session_events_agent ON session_events (agent_id, seq);
CREATE INDEX IF NOT EXISTS session_events_type  ON session_events (type);
-- JSONB enables indexed queries into payloads (e.g. tool name, stop_reason)
-- and cheap analytics; consider monthly RANGE partitioning on seq/ts for
-- retention (§9) once volume warrants it.
```

Event types (a superset of what is emitted today; **bold** = not currently
captured anywhere durable):

- `turn_start` — trigger (`user`|`idle`|`cron`|`reconcile`|`hitl`), input text, role
- **`llm_request`** — system, message window, advertised tool names, max_tokens,
  priority, `tokens_used`/`budget`, `llm_call_n`
- **`llm_response`** — text, tool_calls, stop_reason, and the provider's reported
  token usage (`input_tokens` / `output_tokens`, omitted when unreported). Paired with
  `llm_request`'s `tokens_used` under the same span, this makes the context builder's
  chars/4 estimate measurable against what the model actually charged.
- `tool_start` — name, input
- `tool_end` — name, **output**, **truncated**, **spill_path**, **output_chars**,
  **duration_ms**, **error**, **retries**. On an over-cap result `output` is the
  preview the model saw, while `spill_path`/`output_chars` say where the full text
  lives and how long it was — the journal stores the *pointer*, not the payload, since
  the bytes are already in the file store (see
  [tool-output-spill.md](tool-output-spill.md) §4)
- `context_update` — used, budget
- `sub_agent_start` / `sub_agent_end` — sub_id (as `span_id`), task, status, role, elapsed
- `notify_user` / `goal_mutated` — audit of human-facing / goal-steering effects
- `hitl_ask` / `hitl_answer` — question, options, answer
- `turn_end` — result, error, tool_count, duration_ms
- `checkpoint` — snapshot `seq` marker (ties journal ↔ snapshot, §4)
- lifecycle — `session_spawned`, `session_resumed`, `session_stopped`, `stall`

Payload-size note (§9): `llm_request` repeats a large, mostly-static system
prompt + tool set per inner call. Start by storing it whole (disk is cheap);
later, content-address the static parts (store `system_hash` + a one-time blob,
reference the trimmed window) to deduplicate.

## 7. Integration seams (precise)

The architecture already concentrates the event stream at one point, which makes
this tractable:

1. **`AgentWorker.emitEvent` (`agent_worker.go`) is the choke point.** Every
   `tool_start/tool_end/context_update/response_chunk/thinking/sub_agent_*`
   already flows here. Add one `EventSink.Append(...)` call beside the existing
   `replay.push` and client forward. This single seam captures most of the
   taxonomy with no changes to the loop.
2. **Two new loop hooks for the model's I/O (`loop.go` `Run`).** The
   currently-unpersisted `llm_request`/`llm_response` need capture points around
   `queue.Submit`: emit the assembled `req` (+ `tokens_used`, `llm_call_n`) and
   the raw `resp`. Mirror the existing `SetOnThinking`/`SetOnToolStart` callback
   style so the worker routes them to the sink.
3. **Enrich tool events.** `dispatchWithRetry` already has `duration`, `error`,
   retry count, and `Truncated`; the current `onToolEnd(name, dn, input,
   observation)` drops them. Widen that callback (or add `onToolResult`) so the
   sink records latency/error/retries/truncation.
4. **`EventSink` interface + SQL impl.** For **observability-tier** events
   `Append` is **non-blocking and best-effort** — a channel + dedicated writer
   goroutine + batched inserts, so event persistence never stalls a turn. In-memory
   impl for tests. **Authoritative (state-sourcing) events do not use this path**:
   they commit transactionally with the state change (§8a's two-tier model).
5. **Reuse `X-Nine-Request-ID` as the span id.** The daemon already threads a
   per-call trace ID to plugins (`plugin/trace.go`); use it as (or map it to)
   `span_id` so plugin-side logs and daemon-side events join on one key.
6. **Back the reattach `replayBuffer` with the sink.** Today reattach replay is a
   bounded in-memory ring lost on restart; sourcing it from `session_events`
   makes reattach-after-restart work and removes the 200-entry cap.

## 8. What this unlocks

1. Post-hoc debugging **after a restart** (today: gone).
2. **Exact LLM request/response** capture — the missing piece for "why did it do
   that?" (today: not stored at all).
3. Durable, unbounded **reattach replay** (today: bounded, in-memory).
4. **Deterministic golden-transcript tests** auto-generated from real sessions
   (record once, replay via a recorded `Provider`/dispatcher — automating
   today's hand-written `scriptedProvider`).
5. Analytics: token usage over time, tool latency/error rates, stall causes,
   context-trim frequency.
6. Sub-agent tree reconstruction after the fact (today: live-only `SubAgents()`).
7. Audit trail for self-modification, goal mutations, and `notify_user`.
8. Crash resilience: a turn that crashes mid-flight leaves a partial, inspectable
   trail (today: total loss).

## 8a. Event sourcing for internal state

Because the journal is append-only, ordered, and — thanks to full `llm_response`
and tool-output capture (§6) — **deterministically foldable**, it can be more
than an observability record. It can serve as the **event-sourcing substrate**
for Nine's internal processing: the authoritative stream that internal state is
*projected from*, rather than a side-channel describing state that is
authoritatively mutated elsewhere. This section makes that an explicit design
goal and sets the boundaries, because it constrains choices in v1.

### The distinction (and why it changes the design)

- **Observability framing (default, §4 "complement"):** the loop mutates state
  and writes a checkpoint; events *describe* what happened. Events may be lost
  without corrupting state.
- **System-of-record framing (event sourcing):** commands emit *events*; state
  (history, goal status, stage transitions) exists **only** as a projection that
  folds those events. The checkpoint becomes a *snapshot cache* of the fold, not
  an independent truth.

The two are not mutually exclusive — the resolution is **two tiers of events**,
which the sink must distinguish from day one:

| Tier | Examples | Durability | If lost |
|---|---|---|---|
| **Observability** | `context_update`, `response_chunk`, `thinking`, tool timings, `llm_request`/`llm_response` bodies | async, batched, best-effort | a monitoring gap |
| **Authoritative** | `GoalStatusChanged`, session/stage transitions, supervisor control-plane decisions | committed **transactionally with the command**, synchronously, before ack | **state corruption** |

This is the one hard v1 constraint the event-sourcing goal imposes: authoritative
events must **not** ride the best-effort async sink of §7.4 — they commit in the
same transaction as the state change (or *are* the state change, once an
aggregate is fully projection-driven). Postgres (§1a) makes this a normal
transactional insert.

### What adopting ES for an aggregate requires

1. **Command/event separation** — `goal_update_status` becomes a command that
   validates, then emits an immutable `GoalStatusChanged` fact; the goals table
   becomes a projection folding it (today it is mutated in place).
2. **Per-aggregate stream + optimistic concurrency** — a version per goal /
   conversation (`expected_version`) so concurrent sub-agents can't clobber.
3. **Projections / read models** — the payoff: goals, history, the self-model,
   the notification feed, and analytics all become materialized views rebuilt
   from one stream (a database notify channel or a projection worker; materialized
   views for read-heavy ones), with a guaranteed-consistent rebuild path.
4. **Schema evolution / upcasting** — events are immutable and long-lived; when
   the loop or tooling changes, old events must still fold. JSONB payloads help,
   but you own versioned event schemas and upcasters.
5. **Snapshot-as-cache** — the checkpoint stops being authoritative and becomes
   a periodic fold optimization (§4), so the fold must be canonical.

### Selective, not total

Do **not** event-source the whole store. The sweet spot is the **behavioral
core** — turn execution, goal lifecycle, session/stage transitions, and the
supervisor control-plane — where replay, audit, consistency, and multiple
projections matter. Plain-CRUD tables (`kv`, `files`, `vectors`) gain nothing;
leave them as ordinary state. Full CQRS across every table is usually
over-engineering for a single-node daemon; Postgres makes it *feasible*, not
*warranted everywhere*.

### Entry point: the supervisor bus

Nine has an internal event bus — `Supervisor` posts `Event{Kind, AgentID,
Payload}` (`agent_completes`, `goal_stalls`, `gap_reported`, `plugin_crashed`)
(`supervisor.go`). Persisting the bus into the journal is the lowest-risk first
step toward internal ES: it makes Nine's internal reactions durable, ordered,
and replayable with almost no new concepts, and it is already in v1's scope as
lifecycle events (§6).

### Recommended incremental path (see §11 v5)

1. **Persist the supervisor bus** into `session_events` (folds into v1).
2. **Pilot one aggregate:** make goal-lifecycle status a projection of
   `GoalStatusChanged` events (command/event split + a goals projection),
   proving the two-tier durability model and the rebuild path end-to-end.
3. **Generalize only if the pilot's wins justify it** — extend to session/stage
   transitions and additional projections; keep CRUD tables as state.

## 9. Costs & risks

- **Backend scope (phase 0).** The Postgres store is the largest, most
  cross-cutting piece — every `memory` method, `tsvector` full-text search, the
  `pgvector` vectors table, and open/resume/backup. *Approach:* the `memory.Store`
  method surface stays identical (only the SQL/driver behind it changes) so
  `runtime`/`agent` callers are untouched; a shared connection pool backs every
  table group; the `memory` tests run against a Postgres test container.
- **Postgres availability is now load-bearing.** Fail-fast means the daemon won't
  start without the DB (decided §1a). *Mitigate:* docker-compose `depends_on` +
  healthcheck (superseded by the s6 `pg_isready` gate once Postgres and the
  daemon moved into one container — see
  [Single-container Nine](single-container.md) §5); a clear startup error;
  connection pooling with retries/backoff on transient blips (distinct from
  start-time absence). Event `Append` stays async and batched for throughput,
  but a durable write failure surfaces as an error.
- **Growth / retention.** Events are unbounded. Add a scrub (cf.
  `WorkflowScrub` at boot): keep last *N* turns or *M* days per session, and
  prune journal rows below the latest `checkpoint` seq for closed sessions.
- **Payload size.** Full `llm_request` per inner call is large and repetitive;
  see §6's content-addressing path once volume matters.
- **Secrets.** Prompts and tool I/O can carry secrets (http headers, file
  contents). The journal shares the host DB's trust boundary with `memory`, so
  it is no worse — but note it, and consider a redaction hook keyed on the
  approval-gated tool set.
- **Concurrency ordering.** `run_agents` spawns sub-agents on parallel
  goroutines; wall-clock `ts` is insufficient for causal order. The
  autoincrement `seq` + `turn` + `parent_span_id` preserve causality.
- **Expectation-setting.** Be explicit that live re-execution ≠ reproduction
  (§3); "replay" defaults to observational.

## 10. Debug / UX surface

- `nine trace <agent-id> [--turn N] [--sub-agents]` — print the event timeline
  (or one turn); `--sub-agents` nests each delegated sub-agent's own journal
  inline beneath its `sub_agent_start` marker, recursing to any depth.
- `nine replay <agent-id> --turn N` — reconstruct a turn: request → response →
  each tool call I/O, with token usage and timings.
- `RecordedProvider` / `RecordedDispatcher` (test helpers) — feed a session's
  recorded `llm_response`/tool outputs back for deterministic re-execution;
  snapshot the transcript as a golden test.
- TUI: source the reattach replay from the journal so a reconnect after restart
  shows real history.

## 11. Phasing

Each phase is independently shippable. Phase 0 (the backend migration) is a hard
prerequisite for everything after it.

0. **Phase 0 — PostgreSQL persistence. ✅ Done (2026-07-07).**
   docker-compose Postgres service (fail-fast dependency); `memory.Store` sits
   behind its method surface via a `?`→`$N` rebinding `db` wrapper (so call sites
   are driver-agnostic), full-text search via a generated `tsvector` column + GIN
   index, vectors via `pgvector` (`<=>` cosine distance); `memory.Open` takes a DSN
   (`config.DatabaseURL`, env `NINE_DATABASE_URL`) and pings fail-fast; the
   `memory` suite runs against an isolated-schema-per-test Postgres via
   `internal/memory/memtest`. The `session_events` table also lands here (schema
   only; the sink is v1). *Gate met:* daemon boots end-to-end on Postgres, full
   suite green, starting without the DB exits 1 with no socket.
1. **v1 — journal + model I/O (merged, per §1a "model I/O first"). ✅ Done (2026-07-07).**
   `memory.SessionEvent` + batched `SessionEventsAppend`/`SessionEventsByAgent`
   (`internal/memory/events.go`); an async batched best-effort `EventSink`
   (`internal/runtime/eventsink.go`: buffered channel → single writer goroutine →
   coalesced multi-row INSERT, drop-on-full for the observability tier) wired into
   every session `AgentWorker` (`internal/runtime/journal.go`,
   `agent_worker.go`). Captured per turn: `turn_start`/`turn_end`, `context_update`,
   `thinking`, `tool_start`, enriched `tool_end` (output/truncated/duration/attempts/
   error), `sub_agent_start`/`sub_agent_end`, **plus** the new loop hooks
   `SetOnLLMRequest`/`SetOnLLMResponse` (`internal/agent/loop.go`) for the full
   `llm_request` (system, message window, advertised tool names, tokens/budget) and
   raw `llm_response` (text, tool_calls, stop_reason). Span ids are turn-derived
   (`t<turn>` → `t<turn>.llm<n>` → `t<turn>.tool<n>`) so the tree reconstructs from
   `parent_span_id`. The daemon builds one sink from the store and closes it on
   shutdown (`cmd/nine/daemon.go`). *Gate met:* a turn's exact request, raw reply,
   and full tool trajectory are reconstructable from `session_events` after the
   worker is gone (`TestJournalReconstructsTurn`). Sub-agent workers initially
   passed a nil sink; they now journal their own trajectory under the sub-agent's
   ID through the same shared sink (`AgentBuilder.SetEventSink` →
   `RunSubAgentSync`), while the parent still records the `sub_agent_start`/
   `sub_agent_end` tree pointers — so `nine trace --sub-agents` can nest a
   sub-agent's full trace beneath the parent.
2. **v2 — read surface. ✅ Done (2026-07-07).** `nine trace <agent-id> [--turn N]`
   (compact one-line-per-event timeline) and `nine replay <agent-id> --turn N`
   (one turn reconstructed: each inner LLM request → response → tool I/O with
   token usage, timings, attempts, errors), both reading `session_events`
   directly from the store so they work with the daemon down / after a restart
   (`internal/cli/trace.go`). The reattach path is journal-backed: `attach`
   reconstructs the **full multi-turn transcript** — every user prompt, tool/
   sub-agent trajectory, and assistant response, in order — from the journal
   (`Daemon.journalHistory`, capped at the last `historyMaxTurns` turns), so a
   reconnecting TUI shows the whole conversation as it was rather than a blank
   screen. A single-turn snapshot (`Daemon.journalReplay`) remains as the
   fallback when no store is configured. *Gate met:* `nine replay` reprints a
   real turn faithfully (verified live); `TestJournalHistoryReattach` covers the
   full-transcript reconstruction, `TestJournalReplayReattach` the single-turn
   snapshot, and `TestFormatTrace`/`TestFormatReplay` the rendering.
3. **v3 — deterministic replay for tests. ✅ Done (2026-07-07).** The `replay`
   package (`internal/replay`) reconstructs a session from its journal
   (`FromEvents`) and re-executes it on a real agent loop wired to a recorded
   `Provider` (returns the logged `llm_response`s in order) and a recorded
   dispatcher (returns the logged tool observations in order, one shared cursor
   so outputs are consumed in dispatch order), with no live LLM/tool calls.
   *Gate met:* `TestRecordThenReplay` records a genuine worker turn to Postgres
   via the sink, then rebuilds and re-executes it from the durable journal and
   reproduces the answer while asserting the live provider and real tool were
   never touched; `internal/replay` unit tests cover grouping, determinism
   (identical answers across two replays), and multi-turn ordering.
4. **v4 — retention & scale. ✅ Done (2026-07-07, retention slice).** A boot scrub
   bounds the journal: `Store.SessionEventsScrub(keepTurns, maxAge)` keeps the last
   *N* turns per agent and (independently) drops events older than *M* days —
   removing a row that violates either bound (`internal/memory/events.go`). Wired
   at startup beside `WorkflowScrub` (`cmd/nine/daemon.go`), configured via
   `[daemon] event_retention_turns` (0 → default 200, negative → keep all) and
   `event_retention_days` (0 → no age limit), surfaced by `Config.EventRetention`.
   Tested against Postgres (turn-window, age, disabled). **Deliberately deferred**
   (not warranted at single-node volume; retention now bounds growth):
   JSONB/expression indexes and monthly partitioning (no payload-query surface yet;
   trace/replay/scrub are all covered by the existing `(agent_id, seq)` index and a
   once-per-boot scrub needs no index), and content-addressed request dedup (would
   burden every read path — `nine trace`/`replay`, `replay.FromEvents` — for a
   space win the scrub largely obviates). Revisit either when a concrete
   analytics-query or payload-size pressure appears.
5. **v5 — internal event-sourcing pilot (§8a).** *Step 1 ✅ Done (2026-07-07):
   persist the supervisor bus.* Supervisor
   events are durably journaled on `Post` and consumed via a cursor-backed
   subscription that resumes on restart (built on the subscription primitive,
   reactive-events.md §8; `internal/runtime/supervisor.go`). This is the
   two-tier model's authoritative side: control-plane events commit synchronously,
   not through the best-effort async sink. *Remaining:* make goal-lifecycle status
   a projection of `GoalStatusChanged` events (command/event split + goals
   projection). *Gate:* the goals read model is rebuildable purely by folding
   events. Generalize to further aggregates only if the pilot's consistency/rebuild
   wins justify it.

## 12. Reference symbols

- `internal/agent/loop.go` — `Run` (inner ReAct loop; the LLM request/response
  capture points, §7.2), `SaveState`/`LoadState`/`ConversationState` (the
  checkpoint fold), `dispatchWithRetry` (latency/error/retry data, §7.3).
- `internal/runtime/agent_worker.go` — `processTurn`, `emitEvent`,
  `replayBuffer` (the choke point and the bounded in-memory replay, §7.1/§7.6).
- `internal/agent/dispatcher.go` — `Dispatch`, `AddHook` (post-call hook seam).
- `internal/context/builder.go` — `BuildWithUsage` (what the assembled request
  contains and how it is trimmed).
- `internal/memory/conversations.go` — `ConversationSave`/`Load` (the
  destructive snapshot, §2/§4).
- `internal/memory/db.go` — `initSchema` (where `session_events` lands; note
  `SetMaxOpenConns(1)`, §9).
- `internal/plugin/trace.go` — `X-Nine-Request-ID` (reuse as `span_id`, §7.5).
- `internal/runtime/supervisor.go` — `Event`/`EventKind` and `Post` (the
  journal-backed internal event bus; the event-sourcing entry point, §8a).
- `internal/memory/goals.go` — `GoalUpdateStatus`/`GoalAppendSubtree` (the
  in-place mutations the ES pilot turns into `GoalStatusChanged` events + a goals
  projection, §8a/§11 v5).
- `internal/protocol/protocol.go` — the emitted `Msg` progress event types (the
  stream to persist).
</content>
