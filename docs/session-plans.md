# Session Plans & Stages

Every running session (`AgentWorker`) — an ordinary conversation, the dedicated
self-reflection session, or a goal's background "pursue" session — has a persistent
**session plan**: a small state machine of **stages** that hook into the turn loop and
an idle scheduler. This is what powers autonomous behavior between user turns:
periodic self-reflection and background goal pursuit are both just stages.

---

## Data model

A session plan is one row in the `session_plans` table, keyed by the session's agent
ID:

| Field | Description |
|-------|-------------|
| `id` | Same ID as the session (the conversation/agent ID, or the goal ID for pursue sessions) |
| `status` | `active` \| `paused` \| `archived` |
| `stages` | JSON array of `SessionStage` |
| `created_at` / `updated_at` | Timestamps |

Each `SessionStage`:

| Field | Description |
|-------|-------------|
| `name` | Stage instance name (currently == `kind`) |
| `kind` | Registry key — `active`, `idle-reflection`, or `pursue` |
| `status` | `pending` \| `active` \| `paused` \| `done` \| `skipped` |
| `config` | Stage-specific JSON. The common field is `idle_interval_seconds`, which arms the idle scheduler for this stage. |
| `result` | Free-form, stage-specific result string |
| `updated_at` | Timestamp |

`internal/memory/session_plans.go` provides `SessionPlanGet`, `SessionPlanSave`, and
`SessionPlanListActive`.

---

## StageHandler

Stages are the extension point. Each `kind` maps to a `StageHandler` via
`StageRegistry` (`internal/runtime/session_plan.go`):

```go
type StageHandler interface {
    Init(ctx context.Context, agentID string, cfg json.RawMessage) error
    OnTurnEnd(ctx context.Context, agentID string, result string, err error) error
    OnIdle(ctx context.Context, agentID string) (turnText string, ok bool)
}
```

- **`Init`** — called once when the stage is loaded (fresh or restored from its
  `session_plans` row).
- **`OnTurnEnd`** — called after every turn completes, and also when the session's
  stall detector fires (`result == ""`, `err == ErrStall`). Stages sync their own
  state from domain data here and may write their `session_plans` row directly.
- **`OnIdle`** — called by the per-stage idle scheduler once `idle_interval_seconds`
  has elapsed since the stage's last idle check. If the stage has work to do, it
  returns `(turnText, true)` and `AgentWorker` runs `turnText` as the session's next
  turn — an autonomous turn, with no user input.

`StageRegistry["active"]` is registered statically; `"idle-reflection"` and
`"pursue"` are registered at daemon startup (`cmd/nine/daemon.go`), each bound to a
`*memory.Store`.

---

## Lifecycle

### Creating / loading a plan

`loadOrCreatePlan(ctx, store, agentID, profile, eager)`:

- If a `session_plans` row already exists for `agentID`, it's loaded and its stages
  re-instantiated via `Init`.
- Otherwise, a fresh plan is seeded from `profile` (an ordered list of stage kinds),
  with every stage starting `status: "active"`.
- **Lazy profiles** (ordinary conversations, profile `["active"]`) are *not* written
  to `session_plans` immediately — the worker persists the row on its first
  checkpoint, so short-lived conversations never write a row.
- **Eager profiles** (anything with an idle-capable stage — `["idle-reflection"]`,
  `["pursue"]`) are persisted immediately, since creating the row is itself the signal
  that the background session exists.

### Per-turn: `notifyStages`

After every turn (and on stall), `AgentWorker.notifyStages` calls `OnTurnEnd` on
every stage with `status == "active"`, then persists or refreshes the plan row. This
is how a stage's status changes (e.g. pursue syncing from its goal's status) become
visible to the idle scheduler.

### Idle scheduling

`armIdleTimer` computes the soonest remaining `idle_interval_seconds` across the
session's active, idle-capable stages and arms a single timer for it. When the timer
fires, `handleIdle`:

1. Finds the first active stage whose interval has elapsed.
2. Calls its `OnIdle`.
3. If `OnIdle` returns `ok == true`, runs the returned text as the session's next turn
   (going through the normal turn pipeline — context assembly, tool calls,
   checkpointing, `notifyStages`, re-arming the timer).
4. If `OnIdle` returns `ok == false` (nothing to do), the scheduler is simply
   re-armed for the next cycle.

Stages with no `idle_interval_seconds` in their `config` (e.g. plain `active`) never
trigger idle turns.

### Stall interaction

When the stall detector fires (`StallConfig.Limit` consecutive no-tool turns),
`notifyStages` is called with `result == ""` and `err == ErrStall` instead of a real
turn result. Stages that care about stalls (currently only `pursue`) react to this —
see below.

### Resume on restart

On daemon startup, `Daemon.ResumeSessions` starts a `AgentWorker` for every
`session_plans` row with `status: "active"` that has at least one active,
idle-capable stage (`planNeedsResume`). Ordinary `[active]` conversations have no
idle-capable stage and stay attach-on-demand instead — only background sessions
(self-reflection, pursue) are proactively resumed.

---

## Built-in stages

### `active`

The trivial stage every ordinary conversation gets (`defaultProfile = ["active"]`).
All three `StageHandler` methods are no-ops; it exists so `loadOrCreatePlan` always has
something to seed, and as the slot future per-conversation stages can be added
alongside.

### `idle-reflection` — self-reflection session

A single, fixed session with agent ID `self-reflection`
(`SelfReflectionAgentID`) runs the `idle-reflection` stage. It's created once by
`BootstrapSelfReflection` on first daemon start, with `idle_interval_seconds` set to
2 minutes, and resumed on every subsequent restart via `ResumeSessions`.

- **`OnIdle`** always has work: it returns `ReflectionPrompt`, which asks the model to
  call `memory_set` to update:
  - `self/capabilities` — a concise description of what it can currently do
  - `self/learned` — a short, dated entry with key insights from recent activity
- **`OnTurnEnd`** records each successful reflection turn's result text as a new row
  in the `reflections` table (`nine reflections` / `/reflections`).

This is how the agent's self-model stays current without user interaction. The
`internal/selfmodel.Assembler` reads `self/identity`, `self/capabilities`, and
`self/learned` from the KV store every turn and injects them as the `SystemSelf` block
of the context (see [Context Builder](context-builder.md) and
[Agent Loop](agent-loop.md)). `BootstrapSelfKV` seeds `self/identity` and
`self/capabilities` on first start (`self/learned` is created by the first reflection).

### `pursue` — background goal pursuit

See [Goals](architecture.md#goals) for the LLM-facing `goal_*` tools. Each top-level
goal gets its own background session running the `pursue` stage, keyed 1:1 by
`agentID == goalID`.

- **`OnIdle`** — if the goal is still `active`, returns a prompt
  (`PursuePromptTemplate`) asking the session to `goal_get` the goal and its subtree,
  take any useful action (including spawning sub-goals/sub-agents and recording them
  via `goal_append_subtree`), and call `goal_update_status` if its status should
  change. If the goal is missing or no longer active, `OnIdle` returns `ok == false`.
- **`OnTurnEnd`** — reads the goal back and syncs the stage's own `status` from
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
  with an active `pursue` stage. At the cap, `SpawnGoalSession` returns `(false,
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
| `internal/runtime/session_plan.go` | `SessionPlan`/`SessionStage`-adjacent types, `StageHandler`, `StageRegistry`, `loadOrCreatePlan`, idle-interval helpers |
| `internal/runtime/session_worker.go` | Per-session loop: turns, stall detection, checkpointing, `notifyStages`, idle scheduler (`armIdleTimer`/`handleIdle`) |
| `internal/runtime/stage_idle_reflection.go` | `idle-reflection` stage |
| `internal/runtime/stage_pursue.go` | `pursue` stage |
| `internal/runtime/goal_session.go` | `SpawnGoalSession`, `MaxGoalSessions` cap |
| `internal/runtime/bootstrap.go` | `BootstrapSelfKV`, `BootstrapSelfReflection` |
| `internal/memory/session_plans.go` | `session_plans` table accessors |
| `internal/selfmodel/assembler.go` | Builds the `SystemSelf` context block from `self/*` KV keys |
