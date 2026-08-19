# Concept consolidation — stages, reflection, and goal bookkeeping

- **Status:** **Proposed.** Seven changes (`C1`–`C7`), each independently shippable.
  Five need no schema change. Nothing an operator or the model can observe is
  removed except one redundant agent tool (`C6`).
- **Date:** 2026-08-18.
- **Depends on:** **F4** (the schema migration runner) from
  [`architecture-review.md`](architecture-review.md) — required by `C5` and `C6`
  only. Also `session-plans.md`, `roles.md`, `predefined-agents.md`,
  `workflows.md`.
- **Context:** F8 in the review counted ~20 first-class nouns in the worker
  vocabulary. This note is the resolution for the session-plan / stage / goal /
  workflow / reflection cluster. Two directional decisions shaped it: **multi-stage
  sessions are wanted**, and **reflection history is bounded**. §4 records what was
  considered and rejected, so those options do not get re-proposed.

---

## 1. The changes

| # | Change | Why | Schema? |
|---|---|---|---|
| **C1** | `handleIdle` picks the **longest-overdue** stage, not the first in array order | A real bug: a short-interval stage listed first starves a long-interval one | no |
| **C2** | **Exactly one stage** may carry `role` / `delegates` / goal-ownership, validated at load | Three functions resolve these by first-match, so with two stages behavior depends on JSON array order | no |
| **C3** | Add a **two-stage profile**; let `[[agent]]` declare a stage list | `loadOrCreatePlan` already supports N stages and nothing passes more than one — the capability has no caller | no |
| **C4** | Make **`reflect` an aspect any session can carry** | Reflection is special-cased as a session *kind* in three places; as an aspect, any pursue session or standing agent can reflect on its own progress | no |
| **C5** | Delete the **`reflections` table**; `nine reflections` → `nine log <agent>` | No `agent_id` column, so it breaks under `C4`; its content is already in the journal; the durable output of a reflection turn is a KV write | **yes** |
| **C6** | Delete **`goal.subtree`** and **`goal_append_subtree`** | `parent_id` is the authoritative edge; `subtree` is a free-text copy nothing reads, which the prompt asks the model to maintain by hand | **yes** |
| **C7** | Move **`workflow`** beside the sub-agent in `spec/overview.md` | §3.2 presents goal and workflow as siblings; they are not, and that false parallelism is why they read as duplicates | no (docs) |

**Optional, and only on its own:** rename `stage` → `aspect`. The code implements
concurrent aspects that retire independently; the word implies a sequence. Real
clarity, pure churn — land it alone, last, or not at all.

---

## 2. Detail

### C1 — Fix idle starvation

`handleIdle` (`agent_worker.go:440`) iterates stages in array order and `return`s
after the first that yields work, updating `w.idleSince` for **only that stage**.
Given a 60-second stage ahead of a 3600-second stage, the short one comes due
again before the long one is ever reached, and the long one can starve
indefinitely.

Invisible today because no plan has two stages; a guaranteed defect the moment
`C3` lands. Fix: among stages whose wake has elapsed, choose the one **longest
overdue** (largest negative remaining) rather than the first. One comparison, and
it makes fairness independent of JSON ordering.

### C2 — Define stage precedence

`roleNameForPlan` (`roles.go:318`), `planOwnsGoal` (`session_plan.go:198`) and
`planDelegates` (`:213`) each loop the stage array and return on first match. With
two stages, the resolved role and goal-ownership depend on array order, silently
and unrecorded.

Require that **at most one stage** carries `role`, `delegates`, or goal ownership,
and reject a plan violating it at construction and at load. This turns undefined
behavior into a validated invariant rather than leaving a rule to be inferred from
iteration order.

### C3 — Make multi-stage reachable

The scheduler is already multi-stage and correct:

| Mechanism | Location |
|---|---|
| Per-stage last-fire tracking | `w.idleSince[st.Name]` |
| Per-stage wake, interval **or** cron | `stageNextWake` (`session_plan.go:235`) |
| Timer armed to the **earliest** wake across active stages | `armIdleTimer:418` |
| One turn at a time when several are due (I1-safe) | `handleIdle:446` |
| `OnTurnEnd` fanned out to **all** active stages | `notifyStages:355` |
| Independent per-stage `Status` and retirement | `SessionStage.Status` |

`loadOrCreatePlan` already ranges over `profile []string` creating one stage per
entry, so N works today. What is missing is a caller: `defaultProfile = ["active"]`
(`:60`), `newIdleCapablePlan` (`:289`) and `newStandingPursuePlan` (`:317`) all
pass exactly one.

Add a two-stage profile — `["pursue", "reflect"]` from `C4` is the natural first —
and let an `[[agent]]` entry declare a stage list instead of a single implied
stage.

Also drop `activeStage` from any profile that has a real stage. It exists only to
give `loadOrCreatePlan` something to seed, and once profiles carry real stages it
should stop occupying a slot that means nothing. Keep it as the lone stage of an
ordinary conversation.

### C4 — `reflect` becomes an attachable aspect

Today reflection is a session *kind*: a dedicated session id, a
`BootstrapSelfReflection` boot path, and a special case in `roleNameForPlan`
(`roles.go:329`). The handler itself is trivial and already reusable — its entire
`OnIdle` is `return ReflectionPrompt, true`.

Make it an aspect. The current dedicated reflection session becomes
`profile = ["reflect"]` — same behavior, expressed in the general mechanism — and
any pursue session, standing agent, or the orchestrator can add `reflect` as a
second aspect to reflect on its own recent activity on its own cadence.

**Deletes:** `BootstrapSelfReflection` (`reconcileStandingAgents` plus a default
profile covers it) and the `idle-reflection` branch in `roleNameForPlan` — the role
now resolves through `C2`'s precedence rule like any other stage.
**Keeps:** `stage_idle_reflection.go`, now reusable rather than single-purpose.

### C5 — Delete `reflections`; derive the view from the journal

```sql
CREATE TABLE reflections (id TEXT PRIMARY KEY, ran_at TEXT, summary TEXT)  -- db.go:349
```

Written in exactly one place (`idleReflectionStage.OnTurnEnd`), read in exactly one
(`list_reflections` → `nine reflections`). Three reasons to remove it:

1. **`C4` makes it unrepresentable.** There is no `agent_id` column. Survivable
   while one session reflects; broken the moment several do.
2. **Its content is already in the journal.** `session_events` records
   `(agent_id, turn, type, ts, payload)`, and I11 makes it append-only and
   authoritative.
3. **It is not where the state lives.** The reflection prompt (`prompts.go:66`)
   instructs `memory_set` on `self/capabilities` and `self/learned`; that KV write
   is the durable product, read back by `selfmodel.Assembler` every turn. The row
   is a transcript of the turn that performed the write.

**Deletes:** the table, `ReflectionCreate` / `ReflectionList`, and the
`ReflectionList` method on the daemon store interface (`daemon.go:73`).
**Generalizes:** `nine reflections` becomes `nine log <agent>`, a journal query by
agent id that works for **every** aspect-bearing session — today a standing
agent's output history has no equivalent view at all.

**Retention.** `SessionEventsScrub` (`events.go:63`) bounds the journal; nothing
bounds `reflections`, which grows forever today. Moving to journal-backed history
makes reflection transcripts bounded like every other agent's. That is the
intended outcome, and **no scrub exemption should be built** — reason 3 above is
why: the self-model state was never at risk.

### C6 — Delete `goal.subtree` and `goal_append_subtree`

`goals` carries two representations of one relation:

- `parent_id` + `parent_type` — authoritative, written by `goal_create`.
- `subtree` — a free-text append-only JSON array, described by its own tool as
  "typically a sub-goal or task ID".

**Nothing in the code reads `subtree`.** It reaches the model only by riding along
in `goal_get`'s JSON, and the orchestrator prompt (`prompts.go:14`) tells the model
to maintain both:

> "…using `goal_create` (with parent_id/parent_type set to the parent goal) …
> recording each one with `goal_append_subtree` as you spawn it"

That asks a language model to keep a denormalized index of a relation the schema
already enforces: wrong at some rate, unverifiable, and costing a tool slot in
every context carrying goal tools.

Replace with a `GoalListChildren(parentID)` query, with `goal_get` returning
derived children **under the same `subtree` JSON key** so the model-facing shape
does not change and no prompt or eval moves on that account. Then drop the column,
the tool, and both prompt clauses (`prompts.go:14`, `:75`). Goal tools 5 → 4.

### C7 — Re-frame `workflow` as a delegation ledger

`spec/overview.md` §3.2 presents `goal` and `workflow` as siblings — "durable
structures imposed *over* sessions". They are not siblings:

| | `goal` | `workflow` |
|---|---|---|
| owns a session | **yes**, 1:1 — `agentID == goalID` | **no** |
| wakes itself | yes, interval or cron | no |
| advanced by | its own pursue session, *between* turns | the calling model, *inside* a turn |
| driver code | `pursueStage` + the idle scheduler | none — `workflow.Service` is record-keeping |

A goal is a **machine**; a workflow is a **record**. Since `Step.AgentID` makes a
workflow literally an ordered record of sub-agent delegations, move it out of §3.2
into §3.1 beside the sub-agent:

- **goal** — a standing intention that owns a worker. Sits with sessions and
  standing agents.
- **workflow** — a ledger the model keeps, inside a turn, of work it delegated.
  Sits with sub-agents and `run_agent` / `run_agents`.

Docs and spec only. Cheapest clarity in the list; worth doing first.

---

## 3. Order and net effect

**Unblocked now:** `C7` → `C1` → `C2` → `C3` → `C4`
**After F4:** `C5` → `C6`
**Last or never, alone:** the `stage` → `aspect` rename.

`C1` and `C2` should precede `C3`: both are latent defects that only manifest once
a profile has two stages, so fixing them first means multi-stage never ships
broken. `C4` needs `C2`'s precedence rule. `C5` needs `C4` (and F4).

Net: **−1 table, −1 column, −1 agent tool, −1 wire message, −1 session kind, −1
bootstrap function, −1 latent bug**, and one capability that exists but cannot
currently be used becomes reachable.

Noun count for this cluster drops by three — `stage` survives because it becomes
load-bearing, while self-reflection folds into it as an aspect, `subtree` folds
into `parent_id`, and `reflections` folds into the journal.

---

## 4. Considered and rejected

Recorded so these do not get re-proposed.

- **Merging `goal` and `workflow` into one structure with an `ordered` flag.**
  Proposed in the first review pass and withdrawn. The distinguishing axis is
  autonomy, not ordering (see `C7`'s table): a goal owns a session and wakes
  itself, a workflow has no scheduler at all. Merging would put a scheduler behind
  a passive record. The fix is framing, which is `C7`.
- **Collapsing `session_plans.stages[]` into a single `behavior` column.** The
  array is length 1 in every plan today, and one of the three registered kinds is a
  no-op, so this looked like speculative generality to delete. It is not: the
  scheduler underneath genuinely handles plurality (`C3`'s table). Multi-stage
  sessions were adopted as a direction, so the work is to finish the capability —
  `C1`–`C3` — not remove it.
- **A scrub exemption preserving reflection history permanently.** Unnecessary. The
  self-model lives in `self/*` KV, which nothing prunes; only transcripts become
  bounded (`C5`, *Retention*).
- **Adding `agent_id` to `reflections` instead of deleting the table.** Every one of
  its columns is already in `session_events`; widening it would entrench a
  projection as a store.

---

## 5. Open questions

1. **What is the second aspect, concretely?** `["pursue", "reflect"]` is the
   obvious first profile, but `C3` only earns its keep if a real deployment wants
   it. Other candidates: a cron `report` aspect emitting a digest via
   `notify_user`, or a `compaction` aspect that periodically summarizes its own
   history.
2. **Is the `stage` → `aspect` rename worth the churn?** It removes a genuinely
   misleading name, at the cost of a column rename and every doc that mentions
   stages.
3. **May an operator delete the reflection aspect from the default profile?** `C4`
   makes it config-shaped, so `[daemon] standing_agents_authoritative` (subtractive
   reconciliation) could remove it. Needs a shipped-enabled default and a decision
   on whether it is exempt.
