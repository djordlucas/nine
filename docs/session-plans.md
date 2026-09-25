# Session plans and routines

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

`plans` provides `SessionPlanGet`, `SessionPlanSave`, and
`SessionPlanListActive`.

---

## RoutineHandler

Routines are the extension point. Each `kind` maps to a `RoutineHandler` via
`RoutineRegistry`:

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
`"pursue"` are registered at daemon startup, each bound to a
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

### Per-turn: routine notification

After every turn (and on stall), the session worker notifies each active routine, calling `OnTurnEnd` on
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
   checkpointing, notifying routines, re-arming the timer).
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
routines are notified with an empty result and a stall error instead of a real
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

A single session with agent ID `self-reflection` (`SelfReflectionAgentID`) runs
the `idle-reflection` routine. It is created once by `ReconcileSelfReflection` on
first daemon start and resumed on every subsequent restart via `ResumeSessions`.

**The cadence is `[daemon] self_reflection`**, a Go duration, defaulting to
2 minutes when unset — reflection is how Nine maintains its own self-model, so it
ships on. `"off"`, `"none"`, `"0"` or any non-positive duration removes it, and
removal is subtractive rather than merely skipped: an existing session is
deactivated so the resume pass stops reviving it. An unparseable value is logged
and falls back to the default, because a typo should not silently switch a
background behavior off.

- **`OnIdle`** always has work: it returns `ReflectionPrompt`, which asks the model to
  call `memory_set` to update:
  - `self/capabilities` — a concise description of what it can currently do
  - `self/learned` — a short, dated entry with key insights from recent activity
- Each successful reflection turn's result is recorded in the **event journal**
  under the session's own agent id, and read back with `nine reflections` (or
  `/reflections`).

This is how the agent's self-model stays current without user interaction. The
`Assembler` reads `self/identity`, `self/persona`, `self/capabilities` and
`self/learned` from the KV store every turn and injects them as the `SystemSelf` block
of the context (see [Context Builder](context-builder.md) and
[Agent Loop](agent-loop.md)). On first start those keys are seeded either from a
packaged self-model file, when the operator configured one
([personalities.md](personalities.md)), or from Nine's generic defaults —
`self/learned` is created by the first reflection either way, and `self/persona`
only exists if a file set it.

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

## Limits

| Limit | Detail |
|-------|--------|
| Fixed pursue interval | `PursueIdleInterval` is 5 minutes and is not configurable per goal — unlike the reflection cadence, which `[daemon] self_reflection` sets. |
| Goal-session cap | `daemon.max_goal_sessions`, default 10. Past the cap a goal is recorded without a background session, and `goal_create` reports `pursue_session: "limit_reached"`. |
| Sub-goals get no session | A goal with `parent_type: "goal"` is worked inside its parent's pursue loop. |
| No backfill across restarts | A routine's `lastFire` resets to the worker's start time on restart, so an occurrence missed while the daemon was down is not replayed. |
| One clock trigger per routine | `idle_interval_seconds` and `schedule` are mutually exclusive. |
