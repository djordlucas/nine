# Contract — Session Event Journal

**Status:** Built · **Depends on:** memory store (Postgres), agent worker · **Used by:** trace/replay, retention, subscriptions

Every session's execution trajectory is recorded to an append-only journal (the
`session_events` table). The journal is a **record** (replay, audit, debug) and the
**substrate** the subscription layer reacts to ([`subscriptions.md`](subscriptions.md)).
It is written off the turn's critical path and is distinct from the in-memory progress
stream and reattach ring (see [`agent-worker.md`](agent-worker.md) R-WORK.5/6/8). Full
design: `docs/event-log.md`.

---

## R-EVT.1 — Append-only, ordered, typed

`session_events` rows are **append-only** and **never mutated** in place. Each row is
`{seq BIGSERIAL, agent_id, turn, span_id, parent_span_id, type, ts, payload JSONB}`. `seq`
is DB-assigned and monotonic; reads for one `agent_id` are returned in `seq` order. A
conforming implementation **MUST NOT** expose journal mutation to agents (it is
daemon-private, R-MEM.4).

---

## R-EVT.2 — Producer: the async batched sink

Events are produced by `runtime.NewSQLEventSink`, an **async, batched** writer wired into
each `AgentWorker`. Writing **MUST** stay off the turn's critical path (a slow or failed
journal write must not stall a user turn). Per-turn loop hooks
(`wireJournalHooks`/`clearJournalHooks`) emit at least these types:

```text
turn_start      {input}
llm_request     {system, messages, tool_names, max_tokens, tokens_used, budget, llm_call_n}
llm_response    {text, tool_calls, stop_reason, llm_call_n}
tool_start      {name, input}
tool_end        {name, input, output, truncated, duration_ms, attempts, err}
context_update  {tokens_used, budget}
turn_end        {result}
supervisor      {kind, agent_id, payload}   // control-plane events (see supervisor.md)
```

`span_id`/`parent_span_id` form a per-turn tree: the turn root, each LLM call, and each
tool call get spans.

A delegated sub-agent runs as its own `AgentWorker` and **MUST** be given the same sink
(`AgentBuilder.SetEventSink` → `RunSubAgentSync`), so it journals its full trajectory
under **its own** `agent_id` (the sub-agent ID). The parent separately records
`sub_agent_start`/`sub_agent_end` events carrying that `sub_id`, which is how a reader
links a parent to its children across the flat, per-`agent_id` log.

---

## R-EVT.3 — Read surface: trace & replay

- **`nine trace <agent-id> [--turn N] [--sub-agents]`** renders a session's journal
  directly from the table — it **MUST** work with the daemon down (it opens the store
  read-only). With `--sub-agents`, each `sub_agent_start` event expands into the spawned
  sub-agent's own journal (fetched by `sub_id`), nested and indented beneath the marker,
  recursing to any delegation depth.
- **`nine replay <agent-id> --turn N`** deterministically re-executes a recorded session:
  `internal/replay` (`FromEvents` → `Recorded`) rebuilds the loop on a **recorded**
  provider and dispatcher, so replay makes **no live LLM or tool calls** and reproduces
  the recorded answer.
- **Journal-backed reattach**: a revived session's real history is reconstructed from the
  journal, so `nine attach` shows what actually happened.

---

## R-EVT.4 — Retention (bounded growth)

A boot-time scrub bounds journal size: `SessionEventsScrub(keepTurns, maxAge)` prunes,
per agent, events outside the most recent `keepTurns` turns and (independently) events
older than `maxAge`. Config: `[daemon] event_retention_turns` (0 → default, <0 → keep
all) and `event_retention_days` (0 → no age limit). Retention **MUST** leave the journal a
valid, replayable prefix per agent.

---

## R-EVT.5 — Read methods

| Method | Purpose |
|--------|---------|
| `SessionEventsAppend([]SessionEvent)` | batch insert (seq/ts DB-assigned) |
| `SessionEventsByAgent(agentID)` | full trajectory in `seq` order (trace/replay) |
| `SessionEventsAfter(afterSeq, limit)` | forward scan for subscribers (see [`subscriptions.md`](subscriptions.md)) |
| `SessionEventsScrub(keepTurns, maxAge)` | retention |
| `LatestTurnResult(agentID)` | the most recent `turn_end` answer (used by enrichment surfacing) |

---

## Reference symbols

`internal/memory/events.go` (`SessionEvent`, append/read/scrub, `LatestTurnResult`),
`internal/memory/cursors.go` (`SessionEventsAfter`), `internal/runtime/eventsink.go`
(`NewSQLEventSink`), `internal/runtime/journal.go` (hooks + payloads),
`internal/replay/replay.go` (`FromEvents`, `Recorded`, `Session`),
`internal/cli/` (`trace`, `replay`). Design: `docs/event-log.md`.
