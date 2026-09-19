# Contract — session event journal

**Status:** Built · **Depends on:** memory store, agent worker · **Used by:** trace/replay, retention, subscriptions

Every session's execution trajectory is recorded to an append-only journal (the
`session_events` table). The journal is a **record** (replay, audit, debug) and the
**substrate** the subscription layer reacts to ([`subscriptions.md`](subscriptions.md)).
It is written off the turn's critical path and is distinct from the in-memory progress
stream and reattach ring (see [`agent-worker.md`](agent-worker.md) R-WORK.5/6/8). Full
design: `adr/event-log.md`.

---

## R-EVT.1 — append-only, ordered, typed

`session_events` rows are **append-only** and **never mutated** in place. Each row is
`{seq INTEGER PRIMARY KEY AUTOINCREMENT, agent_id, turn, span_id, parent_span_id, type,
ts, payload TEXT}`. `seq`
is DB-assigned and monotonic; reads for one `agent_id` are returned in `seq` order. A
conforming implementation **MUST NOT** expose journal mutation to agents (it is
daemon-private, R-MEM.4).

---

## R-EVT.2 — producer: the async batched sink

Events are produced by `runtime.NewSQLEventSink`, an **async, batched** writer wired into
each `AgentWorker`. Writing **MUST** stay off the turn's critical path (a slow or failed
journal write must not stall a user turn). Per-turn loop hooks
(`AgentWorker.turnHooks`, attached via `Loop.SetHooks` and detached with
`ClearHooks`) emit at least these types:

```text
turn_start      {input}
llm_request     {system, messages, tool_names, max_tokens, tokens_used, budget, llm_call_n}
llm_response    {text, tool_calls, stop_reason, llm_call_n, input_tokens?, output_tokens?}
tool_start      {name, input}
tool_end        {name, input, output, truncated, duration_ms, attempts, err}
context_update  {tokens_used, budget}
turn_end        {result}
supervisor      {kind, agent_id, payload}   // control-plane events (see supervisor.md)
```

`span_id`/`parent_span_id` form a per-turn tree: the turn root, each LLM call, and each
tool call get spans.

**Token reconciliation.** `llm_request.tokens_used` is the context builder's pre-send
estimate and `llm_response.input_tokens` is what the provider actually charged; the two
events share one LLM-call `span_id`, so estimate-vs-actual is a **join on span** and
**MUST NOT** be duplicated onto a single event. The usage keys are omitted when the
provider reports none (R-LLM.8), so a reader **MUST** treat an absent key as unmeasured
rather than as zero tokens.

A delegated sub-agent runs as its own `AgentWorker` and **MUST** be given the same sink
(`AgentBuilder.SetEventSink` → `RunSubAgentSync`), so it journals its full trajectory
under **its own** `agent_id` (the sub-agent ID). The parent separately records
`sub_agent_start`/`sub_agent_end` events carrying that `sub_id`, which is how a reader
links a parent to its children across the flat, per-`agent_id` log.

---

## R-EVT.3 — read surface: trace & replay

- **`nine trace <agent-id> [--turn N] [--sub-agents]`** renders a session's journal
  directly from the table — it **MUST** work with the daemon down (it opens the store
  read-only). With `--sub-agents`, each `sub_agent_start` event expands into the spawned
  sub-agent's own journal (fetched by `sub_id`), nested and indented beneath the marker,
  recursing to any delegation depth.
- **`nine replay <agent-id> --turn N`** renders one recorded turn in full detail — each
  inner LLM request/response and every tool's I/O with timing, attempts and errors. Like
  `trace` it reads the journal and **MUST** work with the daemon down. It is
  **observational**: rendering the record, making no LLM or tool call.
- **Deterministic re-execution** is a separate, programmatic surface, not a CLI command:
  `internal/replay` (`FromEvents` → `Recorded` → `Session`) rebuilds the loop on a
  **recorded** provider and dispatcher, so a recorded session re-runs with **no live LLM
  or tool calls** and reproduces the recorded answers. It backs the record-then-replay
  gate (`TestRecordThenReplay`) and the eval suite's replay track.
- **Journal-backed reattach**: a revived session's real history is reconstructed from the
  journal, so `nine attach` shows what actually happened.

---

## R-EVT.4 — retention (bounded growth)

A boot-time scrub bounds journal size: `SessionEventsScrub(keepTurns, maxAge)` prunes,
per agent, events outside the most recent `keepTurns` turns and (independently) events
older than `maxAge`. Config: `[daemon] event_retention_turns` (0 → default, <0 → keep
all) and `event_retention_days` (0 → no age limit). Retention **MUST** leave the journal a
valid, replayable prefix per agent.

---

## R-EVT.5 — read methods

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
`internal/cli/` (`trace`, `replay`). Design: `adr/event-log.md`.
