# Concept consolidation — stages, self-reflection, and goal bookkeeping

- **Status:** **Proposed.** Four independent moves, each shippable alone. No
  behavior an operator or the model can observe is removed by Moves 1–3; Move 4a
  removes one agent tool.
- **Date:** 2026-08-18.
- **Depends on:** **F4** (the schema migration runner) from
  [`architecture-review.md`](architecture-review.md) — Moves 1, 3, and 4a all
  change the schema and none can land before it. Also `session-plans.md`,
  `roles.md`, `predefined-agents.md`, `workflows.md`.
- **Supersedes:** [`architecture-review.md`](architecture-review.md) §7.1's
  suggestion that `goal` and `workflow` merge into one structure with an
  `ordered` flag. **That was wrong**; §2 records why, and the review has been
  corrected.
- **Motivation:** F8 in the review named ~20 first-class nouns and pointed at two
  candidate collapses. Reading the code rather than the docs moves the finding: the
  goal/workflow pair is *fine* and mis-framed, while `session plan` + `stage` is a
  genuine over-generalization — a registry, a factory, and an interface built to
  hold an array that is **always length 1**, over an enum with **two** real values.

---

## 1. TL;DR — four moves

| # | Move | Deletes | Schema? |
|---|---|---|---|
| **1** | Collapse `session_plans.stages[]` into behavior columns on the session | `StageRegistry`, `StageFactory`, `initStages`, `activeStage`, `stageRole`, the stage loop in 4 functions | yes |
| **2** | Ship self-reflection as a standing agent, not a built-in stage | `stage_idle_reflection.go`, `BootstrapSelfReflection`, one enum value, one role case | no |
| **3** | Delete the `reflections` table; derive the view from the journal | a table, 2 store methods, 1 daemon interface method | yes |
| **4a** | Delete `goal.subtree` and `goal_append_subtree` | 1 column, 1 agent tool, 2 prompt clauses | yes |
| **4b** | Re-frame `workflow` as a delegation ledger, not a peer of `goal` | nothing — docs and spec only | no |

**Net: 8 nouns → 4.** Before: session plan, stage, goal, subtree, workflow, step,
self-reflection, standing agent. After: session (carrying a `behavior`), goal,
workflow/step, standing agent.

---

## 2. Correction — `goal` and `workflow` must not merge

The review proposed these were "one structure with an `ordered` flag". The code
says the distinguishing axis is not ordering but **autonomy**:

| | `goal` | `workflow` |
|---|---|---|
| owns a session | **yes**, 1:1 — `agentID == goalID` (`stage_pursue.go`) | **no** |
| wakes itself | yes, interval or cron | no |
| advanced by | its own pursue session, *between* turns | the calling model, *inside* a turn |
| lifecycle | open-ended | finite; auto-closes when every step is terminal |
| driver code | `pursueStage` + the idle scheduler | none — `workflow.Service` is pure record-keeping |

`internal/workflow` has no scheduler, no driver, and no session. A goal is a
**machine**; a workflow is a **record**. Merging them would put a scheduler behind
something that must not have one.

What is actually wrong is the framing, which §7 (Move 4b) fixes.

---

## 3. The finding — `stages[]` is a registry for a two-valued enum

Three constructors build a session plan. **Every one produces exactly one stage:**

| Constructor | Stages produced |
|---|---|
| `defaultProfile` (`session_plan.go:60`) | `["active"]` — 1 |
| `newIdleCapablePlan` (`:289`) | `[]SessionStage{{…}}` — 1 |
| `newStandingPursuePlan` (`:317`) | `[]SessionStage{{…}}` — 1 |

Three kinds are registered, and one is a no-op whose own comment states it exists
only to have something to seed (`session_plan.go:50`):

```go
// activeStage is the trivial stage every ordinary [active] conversation gets:
// it never has work of its own, but gives loadOrCreatePlan something to seed
func (activeStage) Init(...) error            { return nil }
func (activeStage) OnTurnEnd(...) error       { return nil }
func (activeStage) OnIdle(...) (string, bool) { return "", false }
```

So the machinery — a JSON array column, a global `StageRegistry`, a
`StageFactory` indirection, a three-method `StageHandler`, `initStages`, and
`sessionPlanState` — exists to express **one enum with two real values**.

**The tax is visible and quantifiable.** Four functions each loop the
always-length-1 array and re-parse its JSON config:

| Function | Location |
|---|---|
| `planOwnsGoal` | `session_plan.go:198` |
| `planDelegates` | `session_plan.go:213` |
| `planNeedsResume` | `session_plan.go:347` |
| `roleNameForPlan` | `roles.go:318` |

Each is written as though several stages could coexist and one of them wins —
with **no defined precedence** if that ever happened. `roleNameForPlan` simply
returns on the first `pursue` or `idle-reflection` it encounters.

**This generality was never planned.** `docs/session-plans.md` contains no
"sequential", "multi-stage", or "future stage" language anywhere; its "Built-in
stages" section lists exactly the three above. Four pilots have shipped without a
second stage.

---

## 4. Move 1 — behavior columns instead of a stage array

Replace `session_plans(id, status, stages TEXT)` with explicit columns:

```sql
behavior         TEXT    NOT NULL DEFAULT 'interactive'  -- 'interactive' | 'pursue'
wake_interval_s  INTEGER NOT NULL DEFAULT 0
wake_cron        TEXT    NOT NULL DEFAULT ''
role             TEXT    NOT NULL DEFAULT ''
delegates        INTEGER NOT NULL DEFAULT 0
```

`StageHandler` survives as `Behavior` with the same three methods (`Init`,
`OnTurnEnd`, `OnIdle`) — the interface is good and the two implementations stay
as they are. What goes is the *plurality*: one behavior per session, selected by
a `switch` rather than looked up in a mutable global registry.

**Deleted:** `activeStage`, `StageRegistry`, `StageFactory`, `initStages`,
`stageRole`, `stageConfig` JSON round-tripping, and the stage loop inside all
four functions in §3 — each becomes a column read.

**Preserved:** every observable behavior. Same resume rule (`wake_interval_s > 0
|| wake_cron != ''` replaces `stageScheduled`), same idle scheduling, same role
resolution, same eager-vs-lazy persistence.

**Note on the registry.** `StageRegistry` is a package-level mutable map written
from two places at boot (`assembly.go:176`, `cmd/nine/daemon.go:138`). Removing it
also removes an initialization-order dependency between the daemon binary and the
runtime package, which is a second, quieter win.

---

## 5. Move 2 — self-reflection becomes a standing agent

`docs/predefined-agents.md` §2 already names `BootstrapSelfReflection` as the
template that standing agents follow. **Invert that relationship:** ship
self-reflection as a default `[[agent]]` entry with `role = "reflection"`, the
configured interval, and the existing `ReflectionPrompt`.

The stage is thin enough that almost nothing is lost. Its entire `OnIdle` is:

```go
func (s *idleReflectionStage) OnIdle(context.Context, string) (string, bool) {
	return ReflectionPrompt, true
}
```

and its `OnTurnEnd` writes the `reflections` row that Move 3 removes.

**Deleted:** `stage_idle_reflection.go`, `BootstrapSelfReflection`, the
`idle-reflection` case in `roleNameForPlan` (`roles.go:329`), and one value from
Move 1's `behavior` enum — because a standing agent *is* a pursue shell with a
narrowed role, which is exactly what `newStandingPursuePlan` already builds.

**Effect:** self-reflection stops being a concept and becomes a config row. It
also makes the standing-agent mechanism load-bearing instead of a second path
that happens to resemble the first.

---

## 6. Move 3 — the `reflections` table is a journal projection

The table has three columns (`db.go:349`):

```sql
CREATE TABLE reflections (id TEXT PRIMARY KEY, ran_at TEXT, summary TEXT)
```

It is written in **exactly one place** (`idleReflectionStage.OnTurnEnd`) and read
in **exactly one** (`list_reflections` → `nine reflections`). Its content is "the
result text of a turn by a known agent, with a timestamp" — which
`session_events` already records as `(agent_id, turn, type, ts, payload)`, and
which I11 makes append-only and authoritative.

**Deleted:** the table, `ReflectionCreate` / `ReflectionList`, the `ReflectionList`
method on the daemon's store interface (`daemon.go:73`), and — with Move 2 — the
last reason `idleReflectionStage` exists.

**Generalizes for free.** `nine reflections` becomes `nine log <agent>`, a journal
query filtered by agent id. That works for *every* standing agent, not just the
reflection one — today a standing agent's output history has no equivalent view
at all.

### The trade-off, stated plainly

`SessionEventsScrub` (`events.go:63`) prunes the journal on two axes — keep the
last N turns per agent, and drop anything older than `maxAge`. **Nothing prunes
`reflections`**: it grows without bound today.

So the move trades unbounded-and-permanent for bounded-and-uniform. Two readings:

- **Recommended:** accept bounded history. An unbounded table of model-written
  summaries that only one CLI command reads is a leak, not a feature, and the
  current behavior is better described as an oversight than a guarantee.
- **Escape hatch:** if reflection history must be permanent, exempt the reflection
  agent in the scrub's `WHERE` clause. One predicate, no new table.

Deciding this is a prerequisite to landing the move, not a follow-up.

---

## 7. Move 4 — goal's double bookkeeping, and the workflow re-frame

### 7a. Delete `goal.subtree` and `goal_append_subtree`

`goals` carries **two parallel representations of the same parent/child
relation**:

- `parent_id` + `parent_type` — the authoritative back-edge, written by
  `goal_create`, enforced by the schema.
- `subtree` — a free-text, append-only JSON array of strings, described by its own
  tool as "typically a sub-goal or task ID".

**Nothing in the code reads `subtree`.** It reaches the model only by riding along
in `goal_get`'s JSON. And the orchestrator prompt (`prompts.go:14`) instructs the
model to maintain both by hand:

> "…using `goal_create` (with parent_id/parent_type set to the parent goal) …
> recording each one with `goal_append_subtree` as you spawn it"

That is asking a language model to keep a denormalized index of a relation the
database already stores — reliably wrong at some rate, unverifiable, and costing a
tool slot in every context that has goal tools.

**Replace with:** a `GoalListChildren(parentID)` query, with `goal_get` returning
derived children under the same `subtree` JSON key so the model-facing shape does
not change. Then drop the column, the tool, and both prompt clauses
(`prompts.go:14` and the `PursuePromptTemplate` at `:75`).

Goal tools go 5 → 4.

### 7b. Re-frame `workflow` as a delegation ledger

`spec/overview.md` §3.2 presents `goal` and `workflow` as siblings — "durable
structures imposed *over* sessions" — and that parallelism is the entire reason
they read as duplicates. §2 shows they are not siblings.

Since `Step.AgentID` means a workflow is literally *an ordered record of sub-agent
delegations*, move it out of §3.2 and into §3.1 beside the sub-agent:

- **goal** — a standing intention that owns a worker. Belongs with sessions and
  standing agents.
- **workflow** — a ledger the model keeps, inside a turn, of work it delegated.
  Belongs with sub-agents and `run_agent` / `run_agents`.

Docs and spec only; no code, no schema. Worth doing **first** if the clarity is
wanted before any of the code moves land.

---

## 8. Net effect

| | Before | After |
|---|---|---|
| Nouns | session plan, stage, goal, subtree, workflow, step, self-reflection, standing agent (**8**) | session (+`behavior`), goal, workflow/step, standing agent (**4**) |
| Tables touched | `session_plans`, `reflections`, `goals` | `session_plans` reshaped; `reflections` gone; `goals` −1 column |
| Registries | `StageRegistry` (mutable, written at boot from 2 packages) | none |
| Agent tools | 5 goal + 5 workflow | 4 goal + 5 workflow |
| Wire messages | includes `list_reflections` | `list_reflections` → generalized `nine log <agent>` |

---

## 9. Phases

Each phase is independently shippable. Moves 1, 3, and 4a change the schema and
are therefore **all blocked on F4**.

| # | Phase | Blocked on |
|---|---|---|
| 0 | **F4** — the migration step runner | — |
| 1 | **Move 4b** — the docs/spec re-frame | nothing; do it whenever |
| 2 | **Move 3** — reflections → journal, plus the retention decision (§6) | F4 |
| 3 | **Move 2** — reflection as a standing agent | Move 3 |
| 4 | **Move 1** — stages → behavior columns | Move 2 (which shrinks the enum first) |
| 5 | **Move 4a** — subtree removal | F4 |

Move 3 is deliberately first among the schema changes: it is the smallest, and it
exercises the new migration runner on something low-risk before Move 1 reshapes a
table the daemon reads at boot.

---

## 10. Decisions taken

- **`goal` and `workflow` do not merge.** The review's §7.1 suggestion is
  withdrawn; the distinguishing axis is autonomy, not ordering, and merging would
  put a scheduler behind a passive record.
- **`stage` is removed as a concept, not generalized further.** The array is
  always length 1, one of three kinds is a no-op, and no document has ever
  proposed a second stage.
- **`StageHandler` survives as `Behavior`.** The three-method shape is right; only
  the plurality and the registry go.
- **Self-reflection is an instance of standing agent, not a peer of it.**
  `docs/predefined-agents.md` already says so; this makes the code agree.
- **`reflections` is a projection, not a store.** Its data is already in the
  journal, which I11 makes authoritative.
- **Bounded reflection history is accepted** (§6), with a one-predicate scrub
  exemption as the escape hatch if that proves wrong.
- **`parent_id` is the only goal edge.** `subtree` is model-maintained
  denormalization of a relation the schema already enforces.
- **`goal_get`'s response shape is preserved** across 4a — children are derived
  and returned under the same key, so no prompt or eval changes on that account.
- **Move 4b may land first.** It is docs-only and unblocks nothing, but it is the
  cheapest clarity in the list.

---

## 11. Open questions

1. **Does anything want a genuinely multi-stage session** — one that moves through
   phases with a different tool surface per phase? Move 1 forecloses it. Roles plus
   sub-agents already cover the cases we know of, and nothing has used the array in
   four pilots, but this is a door closing deliberately and should be an explicit
   call rather than a side effect.
2. **May an operator delete the reflection agent?** Move 2 makes it a config row,
   so `[daemon] standing_agents_authoritative` (subtractive reconciliation) could
   remove it. Needs a shipped-enabled default and a decision on whether the
   reflection agent is exempt from subtractive reconciliation.
3. **Is permanent reflection history a requirement?** (§6.) If yes, the scrub
   exemption lands with Move 3; if no, nothing extra is needed.
