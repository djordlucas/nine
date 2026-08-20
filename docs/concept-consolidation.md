# Concept consolidation — stages, reflection, and goal bookkeeping

- **Status:** **Complete** (2026-08-19). All seven changes (`C1`–`C7`) shipped.
  This note is now a record, not a plan: it is frozen and will not be edited
  further, per the rule adopted in [`architecture-review.md`](architecture-review.md)
  F11 — a note is frozen by its programme finishing, not by its directory.

  Six of the seven needed correction on contact with the code: `C1`'s fix was not
  expressible as written (the clamp made "largest negative remaining" always
  zero), `C2`'s claim was overstated (only one of the three functions was
  order-dependent), `C3` could not ship without part of `C4`, `C4` could not use
  the standing-agent path it named, `C5` became urgent rather than optional once
  `C3` landed, and `C7` was docs-only as predicted. Only `C6` landed exactly as
  specified. The reasoning held up well throughout; the specific prescriptions
  mostly did not.
- **Date:** 2026-08-18.
- **Depends on:** ~~**F4** (the schema migration runner)~~ **landed** as R-MEM.10 —
  it gated `C5` and `C6` only, so both are now unblocked. Also `session-plans.md`,
  `roles.md`, `predefined-agents.md`, `workflows.md`.
- **Context:** F8 in the review counted ~20 first-class nouns in the worker
  vocabulary. This note is the resolution for the session-plan / stage / goal /
  workflow / reflection cluster. Two directional decisions shaped it: **multi-stage
  sessions are wanted**, and **reflection history is bounded**. §4 records what was
  considered and rejected, so those options do not get re-proposed.

---

## 1. The changes

| # | Change | Why | Schema? |
|---|---|---|---|
| **C1** | ~~`handleIdle` picks the **longest-overdue** stage, not the first in array order~~ **done** | A real bug: a short-interval stage listed first starves a long-interval one | no |
| **C2** | ~~**Exactly one stage** may carry `role` / `delegates` / goal-ownership, validated at load~~ **done** | Three functions resolve these by first-match, so with two stages behavior depends on JSON array order | no |
| **C3** | ~~Add a **two-stage profile**; let `[[agent]]` declare a stage list~~ **done** (`[[agent.aspect]]`) | `loadOrCreatePlan` already supports N stages and nothing passes more than one — the capability has no caller | no |
| **C4** | ~~Make **`reflect` an aspect any session can carry**~~ **done** | Reflection is special-cased as a session *kind* in three places; as an aspect, any pursue session or standing agent can reflect on its own progress | no |
| **C5** | ~~Delete the **`reflections` table**; `nine reflections` → `nine log <agent>`~~ **done** (kept the verb, repointed at the journal) | No `agent_id` column, so it breaks under `C4`; its content is already in the journal; the durable output of a reflection turn is a KV write | **yes** |
| **C6** | ~~Delete **`goal.subtree`** and **`goal_append_subtree`**~~ **done** (R-ORCH.13) | `parent_id` is the authoritative edge; `subtree` is a free-text copy nothing reads, which the prompt asks the model to maintain by hand | **yes** |
| **C7** | ~~Move **`workflow`** beside the sub-agent in `spec/overview.md`~~ **done** | §3.2 presents goal and workflow as siblings; they are not, and that false parallelism is why they read as duplicates | no (docs) |

**Optional, and only on its own:** rename `stage` → `aspect`. The code implements
concurrent aspects that retire independently; the word implies a sequence. Real
clarity, pure churn — land it alone, last, or not at all.

**Done, alone and last** — and it was not pure churn. The stronger reason only
became visible from inside the code: `stage` named **two unrelated things**. A
session's concurrent, independently-retiring behaviors were stages, and so were
the phases *within a single turn* that make a user wait (`StageContext` =
"building context", `StageModel` = "waiting for the model", the `stage` wire
event). Nothing distinguished them but context.

Only the first is renamed. `stage` now means a turn phase and nothing else;
`aspect` means a session behavior and nothing else. `C3` had already made the
inconsistency operator-visible by shipping `[[agent.aspect]]` against a schema
column named `stages`.

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

**Done — but not as one comparison.** `stageNextWake` ends in `max(…, 0)`, so
`remaining` is **never negative**: every overdue stage reports exactly `0` and
"largest negative remaining" is not a quantity the code can produce. The clamp is
documented behavior and `armIdleTimer` depends on it (a negative duration would
arm a timer in the past), so it stays. The unclamped reading was factored out as
`stageWakeDelta`, with `stageNextWake` (clamped, for arming) and `stageOverdueBy`
(for choosing) as the two views of it.

Two further details the sketch did not cover. Selection is a **stable sort**, not
a single pick, so a stage that is due but whose `OnIdle` declines yields to the
next-most-overdue rather than costing the whole tick — and equal overdueness
falls back to array order, so single-stage plans behave exactly as before. And
every stage *considered* is marked as fired, not only the one that runs:
otherwise a declining stage stays maximally overdue, wins the sort forever, and
starves the others in a new way.

### C2 — Define stage precedence

`roleNameForPlan` (`roles.go:318`), `planOwnsGoal` (`session_plan.go:198`) and
`planDelegates` (`:213`) each loop the stage array and return on first match. With
two stages, the resolved role and goal-ownership depend on array order, silently
and unrecorded.

Require that **at most one stage** carries `role`, `delegates`, or goal ownership,
and reject a plan violating it at construction and at load. This turns undefined
behavior into a validated invariant rather than leaving a rule to be inferred from
iteration order.

**Done, with the claim narrowed.** Only `roleNameForPlan` is actually
order-dependent — it returns the role of the *first* role-bearing stage.
`planOwnsGoal` and `planDelegates` are existence checks (`any stage has kind
pursue`), so their result does not depend on order at all. The invariant is still
the right one, for two different reasons: at most one **role-bearing** stage,
because a session has exactly one role and it must not be decided by
serialization order; and at most one **pursue** stage, because a pursue session
owns its goal 1:1 (`agentID == goalID`) and two would describe something the
identity relation cannot express. `validateStages` enforces both, at construction
and at load.

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

**Done as `[[agent.aspect]]`**: a nested table array, so the existing single-stage
shape is untouched and each aspect carries its own `interval`/`schedule`.
`ValidateAspects` rejects the declarations that would otherwise fail silently — an
unregistered kind (scheduled, wakes, finds no handler, rearms forever), a missing
cadence (never wakes at all), a duplicate kind (two stages sharing the name
`idleSince` keys on), and `pursue` (which duplicates the shell).

**C3 turned out to require part of C4.** The only registered second aspect,
`idle-reflection`, was *role-bearing by kind*, so `["pursue", "idle-reflection"]`
was rejected by `C2`'s own rule — C3's mechanism would have shipped with no aspect
it could legally combine. The fix is C4's underlying idea, landed here: **the role
is data, not an implication of the kind.** `roleBearingKinds` keeps only `pursue`;
`roleNameForPlan` resolves a role declared in any stage's config, falling back to
the pursue default. The dedicated reflection session now declares
`role = "reflection"` in its stage config (`BootstrapSelfReflection`), so it is
unchanged, while the same kind rides role-free beside a pursue shell.

That is what makes a kind mean different things in different plans: a reflection
stage is the session's whole purpose when it stands alone, and a passenger when it
does not. Encoding the role in the kind made those indistinguishable, so a
reflecting pursue session was simply unrepresentable.

**Still outstanding from `C4`:** deleting `BootstrapSelfReflection` and moving the
dedicated reflection session onto a default profile. That is a boot-path change
touching `reconcileStandingAgents` and resume, and folding it in here would have
made both halves harder to review.

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

**Done in two parts.** The `roleNameForPlan` branch and the role-as-data change
landed with `C3`, which could not ship without them. This is the boot-path half.

`BootstrapSelfReflection` is replaced by `ReconcileSelfReflection`, not by
`reconcileStandingAgents`. Routing it through the standing-agent path would have
required reflection to be a **goal** — standing agents are goals — and a session
that reflects on itself is not an intention anyone holds. It would also have
collided with `C2`'s one-pursue-stage rule.

**Open question 3 is answered: an operator may remove reflection.**
`[daemon].self_reflection` takes a cadence or `"off"`, defaulting to on, since
reflection is how the self-model is maintained (G3).

The part worth care is that removal had to be **subtractive**. A plan row outlives
the boot that created it, so a bootstrap that merely stopped creating one would
leave every machine that had ever run reflection still running it, and the setting
would appear to do nothing. Turning it off now deactivates an existing session —
plan and stages out of `active`, so `planNeedsResume` is false thereafter. The row
is kept rather than deleted, so `nine reflections` still reads its history.

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

**Done, with one deviation: the verb stays `nine reflections`.** `nine log <agent>`
would have duplicated `nine trace <agent-id>`, which is already a journal query by
agent id and is strictly more capable (turn filtering, sub-agent trees). What was
actually missing is not a second raw-journal view but the *digest* one — so
`nine reflections [agent-id]` keeps the verb, takes an optional agent, and reads
the journal instead of the table. Operator muscle memory survives, and the new
capability (any agent's reflection history) arrives without a redundant command.

Like `nine trace`, it reads the store read-only and needs no running daemon — which
is why the `list_reflections` **wire message and its handler are deleted** rather
than repointed, matching the note's own "−1 wire message".

**C5 became urgent rather than optional once `C3` landed.** `OnTurnEnd` received the
agent id and discarded it (`ReflectionCreate(newUUID(), result)`), so the moment an
operator could attach a reflect aspect to a standing agent — which `C3` shipped —
several sessions could write into a table that cannot say which produced what.

**The drop is destructive**, and is the first migration step that is. Rows written
before the journal existed have no equivalent elsewhere and are lost. That is the
accepted cost recorded in §4 ("adding `agent_id` … would entrench a projection as a
store"), not an oversight.

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

**Done as written**, which is worth saying because it is the only change in this
note that needed no correction on contact with the code. Verified first that
nothing in the daemon reads `subtree`: it is written by one tool, and reaches the
model only by riding along in `goal_get`.

Two details worth recording. `GoalGet` derives the children, `GoalList` does not —
listing would otherwise be a query per goal for a field the list view never shows.
And the drop is guarded by a column check rather than issued blind, so the step is
safe against a database that never had the column, matching every other step's
"correct for its own from-version" obligation.

The migration test asserts the point of the change directly: a fixture whose stored
`subtree` said `["stale-entry"]` while its real child was `child` comes back from
`goal_get` as `[child]`. The stored copy could disagree with the schema; the derived
one cannot.

Normative as **R-ORCH.13**.

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
- **Requiring reflection to stay enabled.** Open question 3 resolved the other
  way: an operator may remove it. The obligation that came with that answer is
  that removal must be subtractive, or the setting is decorative.
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
