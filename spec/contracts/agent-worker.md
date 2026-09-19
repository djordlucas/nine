# Contract — agent worker (session lifecycle)

**Status:** Built · **Depends on:** agent loop, checkpoint store, notification store, session plans · **Used by:** daemon, sub-agents

An `AgentWorker` wraps one `agent.Loop` and is the unit of session liveness. It owns a
single **serial worker** that processes turns one at a time. It is used identically for
interactive conversations, the self-reflection session, and goal pursue sessions.

---

## R-WORK.1 — one session, one serial worker, serialized turns (I1)

The worker's `inbox` is a **single-slot** hand-off (capacity 1). A caller submitting a new
turn waits until the current one finishes, making turns strictly sequential per session.
The wrapped `agent.Loop` is never touched concurrently. Idle-triggered turns go through the
**same** `processTurn`, so an idle reflection and a user turn can never run at once on one
session.

```text
worker loop — wait for whichever happens first:
    a turn arrives on the inbox  → processTurn(req)   // real or notification-prepended turn
    the idle timer fires         → handleIdle()       // autonomous turn (session plans)
    stop is requested            → drain current turn & exit
```

---

## R-WORK.2 — turn pipeline (`processTurn`)

```text
turnReq on inbox
  → turnN++ ; replay.clearResponse()
  → prependNotifications(text)         // pull pending notifications, splice into the message
  → wire loop callbacks → emitEvent (OnContextUpdate/OnToolStart/OnToolEnd/OnChunk/OnThinkingChunk/OnThinking)
  → wire journal hooks  → EventSink.Append (turn_start/llm_request/llm_response/tool_*/turn_end) (R-WORK.8)
  → result, err = loop.Run(ctx, text) // streams progress as it goes
  → unwire callbacks + journal hooks
  → notifyStages(result, err)          // RoutineHandler.OnTurnEnd for each active routine, then persist plan
  → checkStall(ctx)                    // if LastRunToolCount()==0 → stallN++
  → checkpoint()                       // loop.SaveState() → ckpt.Save(id, data)   (I5)
  → armIdleTimer()                     // recompute next idle wake-up
  → deliver {result, err} to the waiting caller   // unblocks the connection handler
  → onComplete(id)                     // Supervisor EventAgentCompletes
```

The order is normative: routines are notified, **then** stall is checked, **then** the
checkpoint is written, **then** the idle timer is re-armed, **then** the response is
returned.

---

## R-WORK.3 — notifications (push delivery)

Before running the loop, the worker fetches pending notifications for this agent and
prepends them to the message text. This is the **push** path: a completed/errored
background unit writes a `notifications` row; the next active turn in any conversation
surfaces it with no polling. (The **pull** path is `nine goals`/`nine workflows`.)

---

## R-WORK.4 — stall detection

```text
turn completes:
   tools used > 0  → stallN = 0
   tools used == 0 → stallN++
        stallN >= Limit → OnStall(agentID) ; stallN = 0
```

The production `Limit` is **5** (configured by the daemon when wiring the supervisor).
`OnStall` posts `EventGoalStalls` to the supervisor. Stall detection is **disabled** when
`Limit == 0` or `OnStall == nil`. (Tests use smaller limits; 5 is the runtime default.)

On stall, routines are also notified via `OnTurnEnd(result="", err=ErrStall)` so a routine
can react (e.g. the pursue routine pauses its goal).

---

## R-WORK.5 — progress streaming

During a turn the daemon registers a `progressFn`; `emitEvent` pushes events onto a
**bounded buffer** (cap **256**) that the connection handler forwards to the client.
Registration/clearing of `progressFn` is **guarded** so the daemon can register or clear it
while the worker emits, without a data race. When no client is attached, events still flow
to the replay buffer (R-WORK.6).

---

## R-WORK.6 — replay buffer (reattach support)

Every emitted `tool_*`/`sub_agent_*` event is also pushed into a bounded ring buffer
(`replayBufCap = 200`) and the last completed response is recorded. On `attach`, the
daemon returns a snapshot of up to `replayMaxSend = 50` recent events plus the last
response (`replay_events` + `pending_response` in the `ok` message), so a reconnecting
client redraws recent activity and any answer it missed while disconnected.

```text
loop event → emitEvent ─┬→ progressFn (live, if a client is attached)
                        └→ replay.push (always, for later reattach)
```

---

## R-WORK.7 — clean shutdown

`stop()` signals the inbox closed and waits for the worker to acknowledge, ensuring it
drains the current turn and exits before the worker is discarded. Sub-agent workers are created with
empty `StallConfig{}` (no stall detection) since they are short-lived and synchronous.

---

## R-WORK.8 — durable event journal (distinct from progress/replay)

Separately from the **in-memory** progress stream (R-WORK.5) and reattach ring
(R-WORK.6), each worker records its full trajectory to the **durable** `session_events`
journal when an `EventSink` is configured. Per-turn journal hooks
(`turnHooks`, attached via `Loop.SetHooks` and detached with `ClearHooks`) emit, off
the turn's critical path via an async
batched sink: `turn_start`, `llm_request` (with the exact assembled system prompt and
messages), `llm_response`, `tool_start`/`tool_end`, `context_update`, and `turn_end`
(carrying the answer). Events are keyed by `agent_id` and ordered by a DB-assigned `seq`,
grouped into a span tree (`span_id`/`parent_span_id`). This journal — not the in-memory
buffers — backs `nine trace`/`nine replay`, journal-backed reattach, retention, and
subscriptions (see [`event-journal.md`](event-journal.md) and
[`subscriptions.md`](subscriptions.md)). The sink's flush also wakes journal subscribers.

---

## Reference symbols

`internal/runtime/agent_worker.go` (`AgentWorker`, `processTurn`, `checkStall`,
`replayBuffer`, `armIdleTimer`/`handleIdle`, `prependNotifications`),
`internal/runtime/journal.go` (the event payloads),
`internal/runtime/agent_worker.go` (`turnHooks`),
`internal/runtime/eventsink.go` (`NewSQLEventSink`),
`internal/runtime/subagent.go` (sub-agent worker construction).
