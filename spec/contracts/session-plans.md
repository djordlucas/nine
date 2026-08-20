# Contract — Session Plans, Aspects, Self-Model & Reflection

**Status:** Built · **Depends on:** agent worker, memory store, embedder · **Used by:** every session; goals (pursue)

Every `AgentWorker` carries a **session plan**: a small state machine of **aspects**
persisted in `session_plans`. This is the single substrate for all between-turn autonomy
— periodic self-reflection and background goal pursuit are both just aspects.

---

## R-PLAN.1 — Data model

A session plan is one row keyed by the session's agent ID:

```schema
SessionPlan { id, status ("active"|"paused"|"archived"), aspects []SessionAspect, created_at, updated_at }
SessionAspect {
  name    string   // instance name (currently == kind)
  kind    string   // registry key: "active" | "idle-reflection" | "pursue"
  status  string   // "pending"|"active"|"paused"|"done"|"skipped"
  config  json     // aspect-specific; common field: idle_interval_seconds (arms the idle scheduler)
  result  string   // free-form
  updated_at string
}
```

Store methods: `SessionPlanGet`, `SessionPlanSave`, `SessionPlanListActive`.

---

## R-PLAN.2 — AspectHandler interface

```interface
AspectHandler {
  Init(ctx, agentID, cfg json) error
  OnTurnEnd(ctx, agentID, result string, err error) error  // after every turn, and on stall (result="", err=ErrStall)
  OnIdle(ctx, agentID) (turnText string, ok bool)          // when this aspect's idle interval elapses
}
```

- `Init` runs once when the aspect is loaded (fresh or restored).
- `OnTurnEnd` lets a aspect sync its state from domain data and write its plan row.
- `OnIdle` returning `(text, true)` causes the worker to run `text` as the session's next
  turn — an autonomous turn with no user input. Returning `ok=false` re-arms the timer
  with no turn.

A `AspectRegistry` maps `kind → handler`. `"active"` is registered statically;
`"idle-reflection"` and `"pursue"` are registered at daemon startup, each bound to the
store.

---

## R-PLAN.3 — Lifecycle: lazy vs eager persistence (I7)

`loadOrCreatePlan(store, agentID, profile, eager)`:

- If a row exists, load it and re-`Init` its aspects.
- Else seed a fresh plan from `profile` (an ordered list of aspect kinds), all aspects
  `active`.
- **Lazy profiles** (ordinary conversation, profile `["active"]`) are **not** written
  immediately — the worker persists on its first checkpoint, so short-lived
  conversations never write a row.
- **Eager profiles** (anything with an idle-capable aspect — `["idle-reflection"]`,
  `["pursue"]`) are persisted immediately, because the row's existence *is* the signal
  that the background session exists.

---

## R-PLAN.4 — Idle scheduling

```text
armIdleTimer():  next = min remaining idle_interval_seconds across active, idle-capable aspects
                 (no idle-capable aspect → no timer; plan paused → no timer)
on fire (handleIdle):
   1. find the first active aspect whose interval has elapsed
   2. call its OnIdle
   3. if ok: run the returned text as the next turn (full pipeline: context, tools,
      checkpoint, notifyStages, re-arm)
   4. if not ok: refresh the cached plan from the store, then re-arm for the next cycle
```

The refresh in step 4 is load-bearing: a aspect may retire itself from within `OnIdle`
by writing its plan row directly (as `pursue` does when its goal is no longer active).
Without the refresh the cached plan would keep that aspect `active`, so the timer would
re-arm indefinitely and the session would keep counting against the goal-session cap.

Aspects with no `idle_interval_seconds` (e.g. plain `active`) never trigger idle turns.

---

## R-PLAN.5 — Resume on restart (I7)

On daemon startup, `ResumeSessions` starts a worker for every `session_plans` row with
`status: active` that has at least one active, idle-capable aspect (`planNeedsResume`).
Ordinary `[active]` conversations have no idle-capable aspect and are **not** auto-resumed
— they return on demand via `attach`. Only background sessions (self-reflection, pursue)
are proactively resumed, so background autonomy survives reboots.

---

## R-PLAN.6 — Built-in aspect: `active`

The trivial aspect every ordinary conversation gets (`defaultProfile = ["active"]`). All
three methods are no-ops. It exists so `loadOrCreatePlan` always has something to seed and
as the slot where future per-conversation aspects can be added.

---

## R-PLAN.7 — Built-in aspect: `idle-reflection` (self-reflection)

A single fixed session, agent ID `"self-reflection"`, profile `["idle-reflection"]`,
idle interval **2 minutes**. Created once by `ReconcileSelfReflection` on first start;
resumed every restart.

- `OnIdle` **always** has work: it returns the reflection prompt, asking the model to
  `memory_set`:
  - `self/capabilities` — a concise description of what it can currently do
  - `self/learned` — a short, dated entry of key insights from recent activity
- `OnTurnEnd` does nothing: the turn is recorded by the journal under the session's own
  `agent_id`, and `nine reflections [agent-id]` reads it back from there.

---

## R-PLAN.8 — Self-model (`SystemSelf`)

A self-model assembler reads `self/identity`, `self/capabilities`, and `self/learned`
from K/V **every turn** and injects them as the P2.5 `SystemSelf` block (cap ~600 tokens;
see [`context-builder.md`](context-builder.md)).

- `BootstrapSelfKV` seeds `self/identity` and `self/capabilities` on first start;
  `self/learned` is created by the first reflection.
- This is how the agent's introspection stays accurate without hallucinated capabilities
  — the self-model is *data the reflection loop maintains*, not a static prompt.

---

## R-PLAN.9 — Built-in aspect: `pursue` (background goal pursuit)

See [`orchestration.md`](orchestration.md) for the `goal_*` tools and goal data model.
Each top-level goal gets a `pursue` session keyed 1:1 by `agentID == goalID`, profile
`["pursue"]`, idle interval **5 minutes**.

- `OnIdle` — if the goal is still `active`, return a prompt asking the session to
  `goal_get` the goal and its children, take useful action (including spawning sub-goals/
  sub-agents), and call `goal_update_status`
  if the status should change. If the goal is missing/inactive, sync the aspect status
  the same way `OnTurnEnd` does (missing goal → `done`) and return `ok=false` — this
  retires the aspect on the idle path when a goal is paused/finished/archived while the
  session is idle and no turn (hence no `OnTurnEnd`) ever fires.
- `OnTurnEnd` — read the goal back and sync the aspect status from `goals.status`
  (`active→active`, `paused→paused`, `done`/`archived→done`). On `ErrStall`, also pause
  the goal (`goal_update_status → paused`), freeing a slot under the session cap.

Spawning, idempotency, and the `max_goal_sessions` cap are specified in
[`orchestration.md`](orchestration.md) R-ORCH.* (Goals).

---

## Reference symbols

`internal/runtime/session_plan.go` (`AspectHandler`, `AspectRegistry`, `loadOrCreatePlan`),
`internal/runtime/session_worker.go` / `agent_worker.go` (idle scheduler),
`internal/runtime/stage_idle_reflection.go`, `internal/runtime/stage_pursue.go`,
`internal/selfmodel/` (`Assembler`, `BootstrapSelfKV`).
