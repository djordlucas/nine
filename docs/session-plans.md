# Session Plans & Routines

Every running session (`AgentWorker`) — an ordinary conversation, the dedicated
self-reflection session, or a goal's background "pursue" session — has a persistent
**session plan**: a small state machine of **routines** that hook into the turn loop and
an idle scheduler. This is what powers autonomous behavior between user turns:
periodic self-reflection and background goal pursuit are both just routines.

---

## Data model

A session plan is one row in the `session_plans` table, keyed by the session's agent
ID:

| Field | Description |
|-------|-------------|
| `id` | Same ID as the session (the conversation/agent ID, or the goal ID for pursue sessions) |
| `status` | `active` \| `paused` \| `archived` |
| `routines` | JSON array of `SessionRoutine` |
| `created_at` / `updated_at` | Timestamps |

Each `SessionRoutine`:

| Field | Description |
|-------|-------------|
| `name` | Routine instance name (currently == `kind`) |
| `kind` | Registry key — `active`, `idle-reflection`, or `pursue` |
| `status` | `pending` \| `active` \| `paused` \| `done` \| `skipped` |
| `config` | Routine-specific JSON. The common field is `idle_interval_seconds`, which arms the idle scheduler for this routine. |
| `result` | Free-form, routine-specific result string |
| `updated_at` | Timestamp |

`internal/memory/session_plans.go` provides `SessionPlanGet`, `SessionPlanSave`, and
`SessionPlanListActive`.

---

## RoutineHandler

Routines are the extension point. Each `kind` maps to a `RoutineHandler` via
`RoutineRegistry` (`internal/runtime/session_plan.go`):

```go
type RoutineHandler interface {
    Init(ctx context.Context, agentID string, cfg json.RawMessage) error
    OnTurnEnd(ctx context.Context, agentID string, result string, err error) error
    OnIdle(ctx context.Context, agentID string) (turnText string, ok bool)
}
```

- **`Init`** — called once when the routine is loaded (fresh or restored from its
  `session_plans` row).
- **`OnTurnEnd`** — called after every turn completes, and also when the session's
  stall detector fires (`result == ""`, `err == ErrStall`). Routines sync their own
  state from domain data here and may write their `session_plans` row directly.
- **`OnIdle`** — called by the per-routine idle scheduler once `idle_interval_seconds`
  has elapsed since the routine's last idle check. If the routine has work to do, it
  returns `(turnText, true)` and `AgentWorker` runs `turnText` as the session's next
  turn — an autonomous turn, with no user input.

`RoutineRegistry["active"]` is registered statically; `"idle-reflection"` and
`"pursue"` are registered at daemon startup (`cmd/nine/daemon.go`), each bound to a
`*memory.Store`.

---

## Lifecycle

### Creating / loading a plan

`loadOrCreatePlan(ctx, store, agentID, profile, eager)`:

- If a `session_plans` row already exists for `agentID`, it's loaded and its routines
  re-instantiated via `Init`.
- Otherwise, a fresh plan is seeded from `profile` (an ordered list of routine kinds),
  with every routine starting `status: "active"`.
- **Lazy profiles** (ordinary conversations, profile `["active"]`) are *not* written
  to `session_plans` immediately — the worker persists the row on its first
  checkpoint, so short-lived conversations never write a row.
- **Eager profiles** (anything with an idle-capable routine — `["idle-reflection"]`,
  `["pursue"]`) are persisted immediately, since creating the row is itself the signal
  that the background session exists.

### Per-turn: `notifyStages`

After every turn (and on stall), `AgentWorker.notifyStages` calls `OnTurnEnd` on
every routine with `status == "active"`, then persists or refreshes the plan row. This
is how a routine's status changes (e.g. pursue syncing from its goal's status) become
visible to the idle scheduler.

### Idle scheduling

`armIdleTimer` computes the soonest remaining `idle_interval_seconds` across the
session's active, idle-capable routines and arms a single timer for it. When the timer
fires, `handleIdle`:

1. Finds the first active routine whose interval has elapsed.
2. Calls its `OnIdle`.
3. If `OnIdle` returns `ok == true`, runs the returned text as the session's next turn
   (going through the normal turn pipeline — context assembly, tool calls,
   checkpointing, `notifyStages`, re-arming the timer).
4. If `OnIdle` returns `ok == false` (nothing to do), the worker refreshes its
   cached plan from the store and re-arms for the next cycle. The refresh matters
   because a routine may retire *itself* from within `OnIdle` by writing its plan
   row directly (as `pursue` does when its goal is no longer active); picking that
   change up here is what stops the timer re-arming a since-retired routine and
   keeps it counting against the goal-session cap.

Routines with no `idle_interval_seconds` in their `config` (e.g. plain `active`) never
trigger idle turns.

### Stall interaction

When the stall detector fires (`StallConfig.Limit` consecutive no-tool turns),
`notifyStages` is called with `result == ""` and `err == ErrStall` instead of a real
turn result. Routines that care about stalls (currently only `pursue`) react to this —
see below.

### Resume on restart

On daemon startup, `Daemon.ResumeSessions` starts a `AgentWorker` for every
`session_plans` row with `status: "active"` that has at least one active,
idle-capable routine (`planNeedsResume`). Ordinary `[active]` conversations have no
idle-capable routine and stay attach-on-demand instead — only background sessions
(self-reflection, pursue) are proactively resumed.

---

## Built-in routines

### `active`

The trivial routine every ordinary conversation gets (`defaultProfile = ["active"]`).
All three `RoutineHandler` methods are no-ops; it exists so `loadOrCreatePlan` always has
something to seed, and as the slot future per-conversation routines can be added
alongside.

### `idle-reflection` — self-reflection session

A single, fixed session with agent ID `self-reflection`
(`SelfReflectionAgentID`) runs the `idle-reflection` routine. It's created once by
`ReconcileSelfReflection` on first daemon start, with `idle_interval_seconds` set to
2 minutes, and resumed on every subsequent restart via `ResumeSessions`.

- **`OnIdle`** always has work: it returns `ReflectionPrompt`, which asks the model to
  call `memory_set` to update:
  - `self/capabilities` — a concise description of what it can currently do
  - `self/learned` — a short, dated entry with key insights from recent activity
- Each successful reflection turn's result is recorded in the **event journal**
  under the session's own agent id, and read back with `nine reflections` (or
  `/reflections`).

This is how the agent's self-model stays current without user interaction. The
`internal/selfmodel.Assembler` reads `self/identity`, `self/capabilities`, and
`self/learned` from the KV store every turn and injects them as the `SystemSelf` block
of the context (see [Context Builder](context-builder.md) and
[Agent Loop](agent-loop.md)). `BootstrapSelfKV` seeds `self/identity` and
`self/capabilities` on first start (`self/learned` is created by the first reflection).

### `pursue` — background goal pursuit

See [Goals](architecture.md#goals--state-write-paths-status) for the LLM-facing `goal_*` tools. Each top-level
goal gets its own background session running the `pursue` routine, keyed 1:1 by
`agentID == goalID`.

- **On idle** — if the goal is still `active`, the session is prompted to fetch
  the goal and its sub-goals, take any useful action (including spawning
  sub-goals and sub-agents, which are recorded by creating them under the
  goal), and update the goal's status if it should change. If the goal is missing or no longer active, `OnIdle` returns `ok == false`
  **and** syncs the routine's `status` the same way `OnTurnEnd` does (a missing goal
  collapses to `done`). This matters when a goal is paused/finished/archived (e.g.
  via `goal_update_status`) while the session sits idle: `OnTurnEnd` only fires
  after a turn, so without this the routine would linger in `active`, re-arming the
  idle timer forever and holding a slot under the concurrent-session cap.
- **`OnTurnEnd`** — reads the goal back and syncs the routine's own `status` from
  `goals.status`: `active → active`, `paused → paused`, `done`/`archived → done`. On
  `ErrStall`, it also pauses the goal (`goal_update_status` → `paused`), which frees a
  slot under the concurrent-session cap below.

#### Spawning and resource bounds

`goal_create` spawns a pursue session for newly-created **top-level** goals
(`parent_type: "conversation"`) via `GoalSessionSpawnFn`
(`Daemon.SpawnGoalSession`):

- **Idempotent** — if a session for that goal ID is already running, it returns
  `(true, nil)` without creating another.
- **Capped** — `Daemon.maxGoalSessions` (config `daemon.max_goal_sessions`, default
  `DefaultMaxGoalSessions = 10`) bounds the number of concurrently-running sessions
  with an active `pursue` routine. At the cap, `SpawnGoalSession` returns `(false,
  nil)` — the goal itself is still recorded, just without a background session.
- `goal_create`'s response includes `pursue_session: "spawned"` or
  `"limit_reached"` accordingly.
- Sub-goals (`parent_type: "goal"`) do **not** get their own session — they're worked
  on inside their parent goal's pursue loop.

Each pursue session's idle interval (`PursueIdleInterval`) is 5 minutes.

---

## Source files

| File | Responsibility |
|------|---------------|
| `internal/runtime/session_plan.go` | `SessionPlan`/`SessionRoutine`-adjacent types, `RoutineHandler`, `RoutineRegistry`, `loadOrCreatePlan`, idle-interval helpers |
| `internal/runtime/session_worker.go` | Per-session loop: turns, stall detection, checkpointing, `notifyStages`, idle scheduler (`armIdleTimer`/`handleIdle`) |
| `internal/runtime/stage_idle_reflection.go` | `idle-reflection` routine |
| `internal/runtime/stage_pursue.go` | `pursue` routine |
| `internal/runtime/goal_session.go` | `SpawnGoalSession`, `MaxGoalSessions` cap |
| `internal/runtime/bootstrap.go` | `BootstrapSelfKV`, `ReconcileSelfReflection` |
| `internal/memory/session_plans.go` | `session_plans` table accessors |
| `internal/selfmodel/assembler.go` | Builds the `SystemSelf` context block from `self/*` KV keys |
