# Contract — supervisor (oversight)

**Status:** Built · **Depends on:** plugin manager (rebuild), agent worker (events) · **Used by:** daemon

The supervisor is a single serial event-handling activity that monitors all sessions and
reacts to stalls, capability gaps, and plugin crashes. It is the grounding point for
self-awareness and self-correction.

---

## R-SUP.1 — durable, journal-backed control-plane bus

The supervisor's bus is **durable** (see [`subscriptions.md`](subscriptions.md)).
`Post(event)` **synchronously appends** the event to the session-event journal (the
`supervisor` event type) — it is control-plane state, so it commits before returning and is
**never dropped** on a full buffer or lost on restart. The supervisor consumes its own
journaled events by
implementing `subscribe.Handler` (`ID`/`Types`/`Handle`); `Attach(store)` wires a
cursor-backed `subscribe.Subscription`, and `Run` drives it, resuming from the durable
cursor on boot. Delivery is at-least-once and in `seq` order; reactions (`handle`) must be
idempotent.

---

## R-SUP.2 — events

```text
EventAgentCompletes  — posted by a worker via onComplete after every turn; resets the idle timer
EventGoalStalls      — posted by stall detection (Limit=5 no-tool turns) via OnStall
EventGapReported     — posted by the gap_report core tool
EventPluginCrashed   — posted by the plugin manager on unexpected subprocess exit
```

Each event carries at least the `AgentID` (or plugin name) it concerns.

---

## R-SUP.3 — reactions

| Event | Action |
|-------|--------|
| `EventAgentCompletes` | reset the supervisor idle timer (liveness bookkeeping) |
| `EventGoalStalls` / `EventGapReported` | log/diagnose; surface to the user if unresolved. The supervisor does **not** generate plugins or rewrite config (N1–N3) |
| `EventPluginCrashed` | call the manager's restart-from-binary (`PluginRebuildFn`) — recompilation is never involved (I9) |

Capability gaps that Nine genuinely lacks are **surfaced to the user**, not
self-generated. This is the boundary that distinguishes the current design from older
"self-modifying" designs.

---

## R-SUP.4 — priority

The supervisor submits any LLM calls at `PrioritySupervisor` (value 1) — above active
conversations and background work (see [`llm-provider.md`](llm-provider.md) R-LLM.4). It
preempts background reflection. (Per R-SUP.1 `Post` performs a synchronous journal
append.)

---

## R-SUP.5 — `gap_report` tool

`gap_report` is a core-intercepted tool (always available, all depths) an agent calls
when no available tool fits its task. It posts `EventGapReported`. The system prompt
**SHOULD** steer an agent to first try `skill_search` for a documented approach, and only
`gap_report` a genuine dead-end. (This is the explicit, supervisor-driven half of
capability-gap handling; the implicit half is the agent resolving gaps within its own
loop.)

---

## Reference symbols

`internal/runtime/supervisor.go` (`Supervisor`, `Run`, `Post`, `Attach`, `Handle`/`ID`/
`Types` as a `subscribe.Handler`, `Event`, the `Event*` kinds, `PluginRebuildFn`),
`internal/subscribe/` (the cursor-backed subscription), `internal/agent/register_*.go`
(`gap_report` wiring).
