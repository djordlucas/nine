# Concept consolidation — multi-stage sessions, reflection, and goal bookkeeping

- **Status:** **Proposed (rev 2).** Rev 1 proposed collapsing `session_plans.stages[]`
  into a single behavior column. **That move is withdrawn**: multi-stage sessions
  are wanted (decision, §2), and reading the scheduler shows they are already
  most of the way built. Rev 2 replaces it with the work to *finish* them.
- **Date:** 2026-08-18.
- **Depends on:** **F4** (the schema migration runner) from
  [`architecture-review.md`](architecture-review.md) — Moves 3 and 4a change the
  schema. Move 1 no longer does. Also `session-plans.md`, `roles.md`,
  `predefined-agents.md`, `workflows.md`.
- **Supersedes:** [`architecture-review.md`](architecture-review.md) §7.1's
  `goal`+`workflow` merge (withdrawn, §3), and rev 1 of this note's Move 1.
- **Motivation:** F8 in the review counted ~20 first-class nouns. Rev 1 read that
  as "delete the generality". Two decisions and a closer reading of
  `agent_worker.go` invert it: the generality is **wanted and already works** —
  what is missing is precedence, reachability, and a name that matches the
  semantics.

---

## 1. TL;DR — four moves

| # | Move | Shape | Schema? |
|---|---|---|---|
| **1** | **Finish multi-stage sessions** — define precedence, fix starvation, make multi-stage plans reachable, rename `stage` to match its actual semantics | additive | no |
| **2** | Make `reflect` an **aspect attachable to any session**, not a dedicated session kind | additive | no |
| **3** | Delete the `reflections` table; derive the view from the journal | deletion | yes |
| **4a** | Delete `goal.subtree` and `goal_append_subtree` | deletion | yes |
| **4b** | Re-frame `workflow` as a delegation ledger, not a peer of `goal` | docs only | no |

Moves 1 and 2 compose: once `reflect` is an aspect, the *first real use* of
multi-stage is a standing agent that both pursues its goal and periodically
reflects on how it is doing — on one session, which is exactly what the existing
scheduler supports.

---

## 2. Decisions taken as input to this rev

Two calls were made on rev 1's open questions, and both changed the design:

1. **Multi-stage sessions are wanted.** Rev 1's Move 1 (collapse `stages[]` to a
   column) is withdrawn. §4 is its replacement.
2. **Reflection history is bounded**, sharing the journal's retention. §6 records
   why this costs nothing — the answer turned out to be stronger than "acceptable".

---

## 3. Correction carried forward — `goal` and `workflow` do not merge

The review proposed these were "one structure with an `ordered` flag". Withdrawn.
The distinguishing axis is **autonomy**, not ordering:

| | `goal` | `workflow` |
|---|---|---|
| owns a session | **yes**, 1:1 — `agentID == goalID` (`stage_pursue.go`) | **no** |
| wakes itself | yes, interval or cron | no |
| advanced by | its own pursue session, *between* turns | the calling model, *inside* a turn |
| lifecycle | open-ended | finite; auto-closes when every step is terminal |
| driver code | `pursueStage` + the idle scheduler | none — `workflow.Service` is record-keeping |

A goal is a **machine**; a workflow is a **record**. Merging them puts a scheduler
behind something that must not have one. The framing fix is Move 4b.

---

## 4. Move 1 — finish multi-stage sessions

### 4.1 Correction: the machinery is already multi-stage

Rev 1 of this note described the four stage-reading functions as "written as
though several stages could coexist", implying the plurality was fiction. **That
undersold what is built.** `agent_worker.go` handles plurality properly:

| Mechanism | Location | Multi-stage today? |
|---|---|---|
| Per-stage last-fire tracking | `w.idleSince[st.Name]` map | **yes** |
| Per-stage wake computation (interval *or* cron) | `stageNextWake` (`session_plan.go:235`) | **yes** |
| Timer armed to the **earliest** wake across active stages | `armIdleTimer:418–430` | **yes** |
| One turn at a time when several are due (I1-safe) | `handleIdle:446–465` | **yes** |
| `OnTurnEnd` fanned out to **all** active stages | `notifyStages:355–367` | **yes** |
| Independent per-stage `Status` and retirement | `SessionStage.Status` | **yes** |

This is not speculative generality. It is a working per-stage scheduler that
nothing currently exercises.

### 4.2 What is actually missing

Four gaps, only one of which is a real bug:

**(a) No multi-stage plan is reachable.** `loadOrCreatePlan` already ranges over
`profile []string` creating one stage per entry (`session_plan.go:112`) — it
supports N today. But all three constructors pass exactly one:
`defaultProfile = ["active"]` (`:60`), `newIdleCapablePlan` (`:289`),
`newStandingPursuePlan` (`:317`). **The capability has no caller.**

**(b) Precedence is undefined.** `roleNameForPlan` (`roles.go:318`),
`planOwnsGoal` (`:198`) and `planDelegates` (`:213`) each loop the array and
**return on first match**. With two stages the answer depends on array order,
silently. A session with `pursue` + `reflect` gets a role decided by JSON
ordering.

**(c) Starvation — the one real bug.** `handleIdle` iterates in array order and
`return`s after the first stage that yields work, updating `idleSince` for **only
that stage**. Given a 60-second stage ahead of a 3600-second stage, the short one
is due again before the long one is ever reached, and the long one may never fire.
Invisible today because no plan has two stages; a guaranteed defect the moment one
does.

**(d) The name is wrong.** "Stage" implies sequence — stage 1, then stage 2. The
implementation is **concurrent aspects** that retire independently. `CLAUDE.md`
already warns that Nine's worker vocabulary is overloaded; this is one of the
offenders.

### 4.3 The work

1. **Define precedence explicitly.** Exactly one stage may carry `role`,
   `delegates`, and goal ownership. Enforce at plan-construction and at load:
   a second role-bearing stage is a load error, not a silent first-match. This
   turns (b) from undefined behavior into a validated invariant.
2. **Fix starvation.** In `handleIdle`, among stages whose wake has elapsed, pick
   the one **longest overdue** (largest negative remaining) rather than the first
   in array order. One comparison; makes fairness independent of JSON ordering.
3. **Make it reachable.** Add a profile with two stages — the natural first one is
   `["pursue", "reflect"]` from Move 2 — and let `[[agent]]` declare a stage list
   rather than a single implied one.
4. **Retire `activeStage` where it is redundant.** It exists only to give
   `loadOrCreatePlan` something to seed. Keep it as the lone stage of an ordinary
   conversation; drop it from any profile that has a real stage, so it stops being
   a slot that means nothing.
5. **Rename `stage` → `aspect`** (recommended, separable). `SessionStage` →
   `SessionAspect`, `StageHandler` → `AspectHandler`, `stages` column →
   `aspects`. Costs a column rename and a glossary entry; buys a name that stops
   implying a sequence that does not exist. **Do this last and alone** — it is
   pure churn mixed into anything else.

Items 1–3 are the substance; 4 is tidying; 5 is optional and independent.

---

## 5. Move 2 — `reflect` becomes an attachable aspect

Rev 1 proposed making self-reflection a *standing agent* (its own session).
Multi-stage offers something strictly better: make `reflect` an **aspect any
session can carry**.

The stage is thin enough to be reusable as-is — its entire `OnIdle` is:

```go
func (s *idleReflectionStage) OnIdle(context.Context, string) (string, bool) {
	return ReflectionPrompt, true
}
```

**What changes:** the dedicated reflection session becomes just
`profile = ["reflect"]` — the current behavior, unchanged, expressed in the general
mechanism. What is *new* is that a pursue session, a standing agent, or the
orchestrator can add `reflect` as a second aspect and reflect on its own recent
activity on its own cadence.

**What this deletes:** `BootstrapSelfReflection` (`reconcileStandingAgents` plus a
default profile covers it) and the special-cased `idle-reflection` branch in
`roleNameForPlan` (`roles.go:329`) — reflection stops being a session *kind* and
becomes an aspect with a role, resolved by Move 1's precedence rule.

**What this keeps:** `stage_idle_reflection.go`, now reusable rather than
single-purpose. Rev 1 deleted it; rev 2 does not, and that is the better outcome.

---

## 6. Move 3 — delete `reflections`; bounded retention costs nothing

### 6.1 The table

```sql
CREATE TABLE reflections (id TEXT PRIMARY KEY, ran_at TEXT, summary TEXT)  -- db.go:349
```

Written in exactly one place (`idleReflectionStage.OnTurnEnd`), read in exactly
one (`list_reflections` → `nine reflections`).

### 6.2 Move 2 makes it unrepresentable

**The table has no `agent_id` column.** That is survivable while exactly one
session ever reflects. The moment `reflect` is an attachable aspect (Move 2), N
sessions reflect and the table cannot say which one produced a row.

So Move 3 is not merely a tidy-up any more — **Move 2 requires it**. The
alternative is adding `agent_id` to a table whose every column is already in
`session_events`.

### 6.3 The retention question, answered

> *Is reflection history still required once reflection is an aspect?*

**No — and the reason is stronger than "bounded is acceptable".** Look at what a
reflection turn actually produces (`prompts.go:66`):

```
You have been idle. Reflect on your recent sessions. Use memory_set to update:
- `self/capabilities`: a concise description of what you can currently do
- `self/learned`: append a short dated entry with key insights from recent activity
```

The **durable product of a reflection turn is a KV write** to `self/capabilities`
and `self/learned` — which `selfmodel.Assembler.Build` reads back into the system
prompt every turn, and which **nothing scrubs**. The `reflections` row is a
transcript of the turn that performed the write, not the state the write produced.

So the state survives regardless of retention. What becomes bounded is the
*transcript*, which is exactly what the journal is for and exactly what
`SessionEventsScrub` (`events.go:63`) already bounds for every other agent.

**Decision: accept bounded retention; no scrub exemption.** The escape hatch
proposed in rev 1 (a `WHERE` clause exempting the reflection agent) is **not
needed and should not be built** — it would preserve transcripts while the actual
self-model state was never at risk.

Worth noting: `reflections` grows without bound today, since nothing prunes it.
Bounded retention is a fix, not a regression.

### 6.4 What lands

**Deleted:** the table, `ReflectionCreate` / `ReflectionList`, the `ReflectionList`
method on the daemon store interface (`daemon.go:73`).
**Generalized:** `nine reflections` becomes `nine log <agent>` — a journal query
filtered by agent id, which works for **every** aspect-bearing session. Today a
standing agent's output history has no equivalent view at all.

---

## 7. Move 4 — goal bookkeeping and the workflow re-frame

### 7a. Delete `goal.subtree` and `goal_append_subtree`

`goals` carries **two parallel representations of one relation**:

- `parent_id` + `parent_type` — authoritative, written by `goal_create`.
- `subtree` — a free-text append-only JSON array, described by its own tool as
  "typically a sub-goal or task ID".

**Nothing in the code reads `subtree`.** It reaches the model only by riding along
in `goal_get`'s JSON, and the orchestrator prompt (`prompts.go:14`) instructs the
model to maintain both by hand:

> "…using `goal_create` (with parent_id/parent_type set to the parent goal) …
> recording each one with `goal_append_subtree` as you spawn it"

That asks a language model to keep a denormalized index of a relation the schema
already enforces — wrong at some rate, unverifiable, and costing a tool slot in
every context carrying goal tools.

**Replace with** a `GoalListChildren(parentID)` query, with `goal_get` returning
derived children under the same `subtree` JSON key so the model-facing shape is
unchanged. Then drop the column, the tool, and both prompt clauses
(`prompts.go:14`, `:75`). Goal tools 5 → 4.

### 7b. Re-frame `workflow` as a delegation ledger

`spec/overview.md` §3.2 presents `goal` and `workflow` as siblings — "durable
structures imposed *over* sessions" — and that false parallelism is the entire
reason they read as duplicates (§3 shows they are not siblings).

Since `Step.AgentID` makes a workflow literally *an ordered record of sub-agent
delegations*, move it out of §3.2 into §3.1 beside the sub-agent:

- **goal** — a standing intention that owns a worker. Sits with sessions and
  standing agents.
- **workflow** — a ledger the model keeps, inside a turn, of work it delegated.
  Sits with sub-agents and `run_agent` / `run_agents`.

Docs and spec only. Worth doing **first** if the clarity is wanted before code
moves.

---

## 8. Net effect

Rev 1 claimed 8 nouns → 4 by deletion. Rev 2 keeps multi-stage, so the count moves
differently — and more honestly:

| | Before | After |
|---|---|---|
| session plan + stage | two nouns, one unreachable capability | one noun (`aspect`), reachable and fair |
| self-reflection | a session *kind*, special-cased in 3 places | an *aspect* any session may carry |
| reflections | a table with no `agent_id` | a journal query, per agent |
| goal edges | `parent_id` **and** a model-maintained `subtree` | `parent_id` only |
| workflow | a false sibling of `goal` | a delegation ledger beside sub-agents |

Net: **−1 table, −1 column, −1 agent tool, −1 wire message, −1 session kind, −1
bootstrap function**, and one previously-unreachable capability made real. The
noun count drops by 3 (stage/self-reflection/subtree fold away) rather than 4 —
`aspect` survives because it is now load-bearing.

---

## 9. Phases

| # | Phase | Blocked on |
|---|---|---|
| 1 | **Move 4b** — docs/spec re-frame | nothing |
| 2 | **Move 1 items 1–3** — precedence, starvation fix, a reachable two-aspect profile | nothing (no schema change) |
| 3 | **Move 2** — `reflect` as an attachable aspect | Move 1 (needs the precedence rule) |
| 4 | **F4** — the migration step runner | — |
| 5 | **Move 3** — reflections → journal | F4, Move 2 |
| 6 | **Move 4a** — subtree removal | F4 |
| 7 | **Move 1 item 5** — the `stage` → `aspect` rename | everything above; land alone |

Note the change from rev 1: **Moves 1 and 2 no longer touch the schema**, so they
are not blocked on F4 and can start immediately. Only Moves 3 and 4a wait.

---

## 10. Decisions taken

- **Multi-stage sessions are kept and completed**, not collapsed. Rev 1's Move 1
  is withdrawn.
- **The existing scheduler is multi-stage-correct** (per-stage `idleSince`,
  earliest-wake arming, one turn at a time, `OnTurnEnd` fan-out). Rev 1 undersold
  this; it is the reason Move 1 is now cheap.
- **Exactly one stage may carry role, delegation, and goal ownership**, enforced
  at load. First-match-wins becomes a validated invariant instead of undefined
  behavior.
- **Idle dispatch picks the longest-overdue stage**, not the first in array order —
  fairness must not depend on JSON ordering.
- **`reflect` becomes an attachable aspect, not a session kind.** Strictly more
  useful than rev 1's "make it a standing agent", and it is the first real
  consumer of multi-stage.
- **`stage_idle_reflection.go` survives** (rev 1 deleted it) — as a reusable aspect
  rather than a single-purpose one.
- **Reflection history is bounded, with no scrub exemption.** The durable product
  of a reflection turn is the `self/*` KV write, which nothing scrubs; the
  transcript is journal data like any other.
- **Move 2 requires Move 3.** `reflections` has no `agent_id` and cannot represent
  more than one reflecting session.
- **`goal` and `workflow` do not merge**; the axis is autonomy, and the fix is
  framing.
- **`parent_id` is the only goal edge.** `subtree` is model-maintained
  denormalization of a relation the schema already enforces.
- **`goal_get`'s response shape is preserved** across 4a — children are derived and
  returned under the same key, so no prompt or eval changes on that account.
- **The `stage` → `aspect` rename lands alone, last**, or not at all. It is pure
  churn if mixed into a behavioral change.

---

## 11. Open questions

1. **What is the second aspect, concretely?** `["pursue", "reflect"]` is the
   obvious first profile, but Move 1 item 3 is only worth landing if a real
   deployment wants it. Candidates beyond reflect: a cron `report` aspect emitting
   a digest via `notify_user`, or a `compaction` aspect that periodically summarizes
   its own history.
2. **Is the `stage` → `aspect` rename worth the churn?** It removes a genuinely
   misleading name (the code implements concurrent aspects, the word implies a
   sequence), at the cost of a column rename and every doc mentioning stages.
3. **May an operator delete the reflection aspect from the default profile?**
   Carried over from rev 1 and unchanged by these decisions: `[daemon]
   standing_agents_authoritative` could remove it. Needs a shipped-enabled default
   and a decision on exemption from subtractive reconciliation.
