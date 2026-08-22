# Pre-defined agents — long-running goals seeded from config

An operator can declare **pre-defined, long-running agents** in `nine.toml`.
They come up on daemon boot, without a human first opening a session.

A pre-defined agent is **not a new primitive**. It is a **goal seeded from
configuration**, running the **pursue structural shell** — persistence,
scheduler, non-interactive — with a **narrowed role** supplying only its tools
and persona.

---

## 1. Motivation

Today a long-running background worker is born inside a conversation: the user
talks to the orchestrator, the orchestrator calls `goal_create`, and the daemon
spawns a `[pursue]` session for that goal (see
[`session-plans.md`](../docs/session-plans.md), [`roles.md`](../docs/roles.md) — the `pursue`
role). This is exactly the behavior we want for a standing agent like "monitor
this repo for security issues" or "keep dependencies up to date" — but it
requires a human to kick off a session first.

We want the same standing workers to exist **declaratively**: list them in
`nine.toml`, and they run from boot, survive restarts, pursue themselves on a
schedule, are **narrowly tool-scoped** for safety/focus, and can **surface
findings to a human** — no human turn required to bring them to life.

## 2. The key insight — the pursue machinery already exists

Three pieces already in the tree compose into the *lifecycle* of this feature
with almost no new code:

1. **`BootstrapSelfReflection`** (`internal/runtime/bootstrap.go`) is this exact
   pattern in miniature: at boot it ensures a `session_plans` row exists
   (idempotent — a no-op if already present), seeded with a profile
   (`idle-reflection`) and an idle interval. It is a long-running,
   non-interactive background worker that **no human session created**. That is
   the template a pre-defined agent follows.

2. **`Daemon.SpawnGoalSession(goalID)`** (`internal/runtime/goal_session.go`)
   builds the pursue worker for a goal: it creates the `[pursue]` plan, wires
   the idle scheduler, and registers the session. Idempotent — it early-returns
   if a session for that ID is already running.

3. **`Daemon.ResumeSessions`** (`internal/runtime/daemon.go`) walks
   `SessionPlanListActive()` at boot and resurrects every background session
   whose plan needs resume. So once a pursue session's plan row exists, **every
   subsequent reboot revives it automatically**.

What these *don't* give us — and what this feature has to build — is:
**declarative reconciliation** (config edits take effect without a
create/delete/re-create loop), **tool narrowing per agent**, **cron scheduling**
(the scheduler is interval-only today), and a **human-facing output surface** (a
background session has no conversation a human attaches to). Those four are §5's
work breakdown.

## 3. Design — a pre-defined agent is a config-seeded goal

Do **not** introduce a parallel "agent" primitive alongside goals. A pre-defined
agent **is** a long-running goal with a good description; Nine's own system
prompt already frames standing work this way. One mental model, and every
existing goal tool and the pursue idle scheduler apply.

### 3.1 The structural shell vs. the role — a deliberate split

A running standing agent is two layers, resolved separately:

- **The structural shell = pursue.** The session *persists* (checkpointed),
  runs on the scheduler, is *non-interactive* (R-HITL.1: no `ask_human`), and
  owns a goal. This comes from the `[pursue]` session plan, exactly as today.
- **The role = narrowed.** The loop's *tools* and *persona* come from the named
  role (default `monitor`, §6), **not** from the pursue role. The role narrows
  only the **work toolset**; it does **not** supply the structural flags
  (`Persists`/`Profile`/`Interactive`) — those belong to the shell. This is
  consistent with `roles.go`'s note that `Persists`/`Profile` are "realized by
  the daemon's session machinery, not the loop builder."

Two consequences of this split, both important:

- **Goal self-management tools are part of the shell, always-on.** A standing
  agent's whole job is to steer its own goal: `goal_get`, `goal_list` and
  `goal_update_status` (how it pauses or finishes itself — §4). These are
  granted to any goal-owning shell independently of delegation, so a narrowed
  role keeps them. Findings are recorded as sub-goals under the agent's own
  goal, and in memory.
- **Delegation is separate and opt-in.** `run_agent`/`run_agents` stay gated;
  a standing agent gets them only when its `[[agent]].delegates` flag is true
  (default false — §4).

### 3.2 Config schema

A repeated `[[agent]]` block in `nine.toml`:

```toml
[[agent]]
id          = "sec-watch"                          # stable, operator-chosen — NOT a UUID
description = "Monitor this repo for security issues; triage new CVEs affecting our deps."
role        = "monitor"                            # optional; default "monitor" (read-only). Narrows WORK tools only.
delegates   = false                                # optional; default false. Opt-in to sub-agent fan-out.
schedule    = "0 9 * * 1-5"                         # cron — weekdays 9am
#                                                   # …XOR…
# interval  = "24h"                                 # Go duration; default PursueIdleInterval (5m) if neither set

[[agent]]
id          = "dep-monitor"
description = "Keep Go dependencies current: check weekly, summarize, propose bumps."
role        = "sysadmin"                            # opt into a wider role when the task needs it
interval    = "168h"
```

Mapped to config (`internal/config/config.go`):

```go
type Config struct {
    // …existing fields…
    Agents []AgentConfig `toml:"agent"`
}

// AgentConfig declares a pre-defined long-running agent — a goal seeded at boot
// and run under the pursue shell with a narrowed role (predefined-agents.md).
type AgentConfig struct {
    ID          string `toml:"id"`          // stable goal ID; reconciliation keys on it
    Description string `toml:"description"`  // the standing intention the agent pursues
    Role        string `toml:"role"`         // work-tool/persona role; default "monitor"
    Delegates   bool   `toml:"delegates"`    // may spawn sub-agents; default false
    Interval    string `toml:"interval"`     // idle cadence (Go duration) — XOR Schedule
    Schedule    string `toml:"schedule"`     // cron expression — XOR Interval
}
```

### 3.3 Boot reconciliation

The reconcile loop goes in `cmd/nine/daemon.go`, **after**
`BootstrapSelfReflection` and **before** `daemon.ResumeSessions` — the slot the
self-reflection bootstrap already occupies. Unlike a naive
seed-if-absent, it makes config the **desired state for the definition** while
leaving **run-state to the agent** (§4):

```text
for each [[agent]] a in config:
    validate a (skip-and-log on empty id/description, or interval+schedule both set)
    goal, exists = store.GoalGet(a.ID)
    if !exists:
        store.GoalCreate(a.ID, a.Description, "", "config")   // parent_type="config" = origin marker
        daemon.SpawnStandingSession(ctx, a)                   // pursue shell + a.Role + a.trigger
    else if goal is config-owned (parent_type=="config"):
        if goal.Description != a.Description:
            store.GoalUpdateDescription(a.ID, a.Description)   // NEW store method
        if goal.Status == "active":
            daemon.SpawnStandingSession(ctx, a)               // idempotent; ensures running w/ current role/trigger
        // if paused/done/archived: update definition only, do NOT resurrect (§4)
```

- **Origin marker.** Reuse the existing `goal_create` `parent_type` field with
  the sentinel `"config"` (empty `parent_id`) — no schema migration. It lets
  reconciliation touch *only* config-managed goals and never a
  conversation-created one.
- **Idempotent.** Safe to run every boot; on later boots `ResumeSessions` (which
  runs right after) revives the pursue plans, and `SpawnStandingSession`
  early-returns for anything already live.
- **`SpawnStandingSession`** is `SpawnGoalSession` extended to accept the role
  name, delegates flag, and trigger (interval or cron), threading them into the
  seeded plan (§5, pieces 1 and 5).

## 4. Reconciliation authority — config owns the definition, the agent owns run-state

The operator wanted to **edit agents without a create/delete/re-create loop**,
and separately, standing agents should be able to decide they are *stuck* or
*done*. These two combine into a split authority:

| Field | Owner | Behavior on boot |
|---|---|---|
| `description`, `role`, `delegates`, trigger | **config** | Reconciled in place every boot. Edit the file, restart, the agent picks it up. |
| goal **status** (`active`/`paused`/`done`/`archived`) | **the agent** | Never overridden by config. If the agent paused or finished its goal, boot updates the definition but **does not resurrect** the session. |

So a `done` agent stays done even while it's still listed in `nine.toml` — the
list is the set of agents Nine *knows about and will maintain the definition
of*, not a command to force them all active. Removing an entry is **additive-
subtractive-safe**: it simply stops reconciling that goal (the goal/session are
left as-is for the operator to `goal_update_status` archive). Full "remove from
config ⇒ tear down" is deferred to a later phase (§7, v3) because it needs
careful handling to never touch conversation-created goals.

**Known v1 limitations** (documented, not bugs):

- Config is read **at boot**. If a human re-activates a paused config agent
  mid-run (`goal_update_status active`), its pursue session is not re-spawned
  with the config role/trigger until the next boot. Acceptable for v1.
- `role`/`delegates`/trigger are **not persisted on the goal** — they are
  re-read from config each boot and passed into `SpawnStandingSession`. The
  durable state is the goal (description + status) and the session plan.

## 5. Work breakdown — the genuinely new pieces

Reuse gets us the lifecycle; these six are the actual build. The first two are
self-contained enough to ship (and spec) on their own.

1. **Cron scheduler** *(largest; independently shippable)*. The session-plan
   idle scheduler is interval-only. Add a trigger that computes next-wake from a
   cron expression. Each agent is `interval` **XOR** `schedule`. Likely a small
   cron-parsing dependency plus a scheduler variant on the plan. Deserves its
   own note if it grows — `scheduling.md`.
2. **Daemon-level notification feed** *(independently shippable)*. A background
   agent has no conversation, and today's `NotifAdd(agentID, text)` posts to a
   *session's own* next turn (`agent_worker.go` `prependNotifications`), not a
   human. Build: a global feed store, a `notify_user(text)` core tool the pursue
   shell can call, and a `nine notifications` CLI command / TUI badge to read
   it. This is the "human sees findings" surface.
3. **`monitor` role** — new `skills/roles/monitor.md` (§6). Read-only work tools,
   no shell/write/delegation. The safe default.
4. **Goal-tool decoupling** — `goal_get`/`goal_list`/`goal_update_status` are
   granted to every goal-owning shell regardless of role (§3.1), while
   `run_agent`/`run_agents`/`workflow_*`/`goal_create` stay on the delegation
   gate.
5. **Per-plan role override** — `roleNameForPlan` (`internal/runtime/roles.go`)
   hardcodes `pursue → PursueRole`. The seeded plan must carry the configured
   role name so `build()` resolves *that* role's tools/persona while keeping the
   pursue shell; `SpawnStandingSession` sets it.
6. **Reconcile loop + origin marker + `GoalUpdateDescription`** — the §3.3 boot
   loop, the `parent_type="config"` sentinel, and a new
   `Store.GoalUpdateDescription` (alongside `GoalUpdateStatus`) so the definition
   can be reconciled in place.

## 6. The `monitor` role

A new built-in role, seeded like the others from `skills/roles/monitor.md`
(embedded, immutable). It is the safe default for standing agents: read and
research, never mutate the host.

```markdown
---
name: monitor
description: Read-only standing-agent worker — web, HTTP GET, file reads, and memory; no shell, no writes.
tags: [role, monitoring]
role:
  tools: [web_search, web_page_read, http_get, read_file, file_list,
          file_search_text, file_fetch, memory_get, memory_set, memory_list, skill_read]
  delegates: false
  spawns_goals: false
  persists: false      # ignored — the pursue shell owns persistence (§3.1)
  interactive: false
  profile: []
---

# Monitor role
You are Nine operating as a standing monitoring agent. You watch a specific,
open-ended concern over time. Each time you wake: read your goal (goal_get),
gather current information from the web and stored files, compare it against
what you recorded before (memory_get, and the sub-goals under your goal), and
record what changed (memory_set, and a sub-goal for a new finding). If something warrants human
attention, call notify_user with a concise summary. You cannot run shell
commands or write to the host filesystem. Be terse; do not repeat findings you
have already reported.
```

Note the role carries `delegates:false` and no `run_agent`; delegation for a
standing agent is turned on by the **`[[agent]].delegates` flag** at the shell
level (§3.1, §5 piece 4), not by the role. Wider built-in roles
(`sysadmin`, `software-dev`, `report-writer`) remain selectable via
`[[agent]].role` when a task genuinely needs shell or writes; agent-authored
restrictive roles (R-ROLE.7) are a later extension.

## 7. Phasing

Each phase is independently shippable with its own gate.

1. **v1a — reconcile loop, role scoping, goal-tool decoupling** (pieces 3–6).
   `[[agent]]` seeds a goal, runs it under a narrowed role on the *interval*
   scheduler, reconciles definition-in-place, and respects agent-owned status.
   *Gate:* a `monitor` agent declared in `nine.toml` is running after a cold
   boot with no human input; it can call `goal_update_status` but the `shell`
   tool is not in its reach and returns `unknown tool`
   (R-ROLE.4); editing its description in config and restarting updates it in
   place; a goal the agent marked `done` is not resurrected.
2. **v1b — cron scheduling** (piece 1). `schedule` triggers per cron. *Gate:*
   an agent with `schedule="0 9 * * 1-5"` wakes on that cron, not on an
   interval.
3. **v1c — human output surface** (piece 2). `notify_user` + `nine
   notifications`. *Gate:* a finding posted by a background agent appears in the
   daemon feed and is visible with no session attached.
4. **v2 — delegation & wider roles.** Exercise `[[agent]].delegates=true` and
   non-`monitor` roles end-to-end; consider agent-authored restrictive roles.
5. **v3 (optional) — subtractive reconciliation** *(shipped)*. Config as full
   desired state, behind the opt-in `[daemon] standing_agents_authoritative`
   flag (default false). When enabled, a config-origin goal (`parent_type ==
   "config"`) no longer listed in `nine.toml` has its session stopped and its
   plan deactivated (`Daemon.TeardownStandingSession`) and — if it was still
   live — its goal archived. Conversation-created goals are never touched (the
   selection filters on the `config` origin marker; `removedConfigGoals`).
   *Gate:* with the flag on, deleting an `[[agent]]` entry and restarting
   archives that agent's goal and stops its session, while a goal created in a
   conversation is left untouched.

   **Limitation.** Teardown archives (does not delete) the goal, preserving it
   and its sub-goals. Because goal status is agent/authority-owned (§4), re-adding
   a removed agent does **not** auto-resurrect it — its goal stays `archived`;
   reactivate it manually (`goal_update_status <id> active`) to bring it back.

## 8. Reference symbols

- `internal/runtime/bootstrap.go` — `BootstrapSelfReflection` (the lifecycle
  template); the reconcile loop is a sibling.
- `internal/runtime/goal_session.go` — `SpawnGoalSession` → extend to
  `SpawnStandingSession` (role + delegates + trigger); `PursueIdleInterval`,
  `DefaultMaxGoalSessions`, `newIdleCapablePlan` (interval/cron carrier).
- `internal/runtime/daemon.go` — `ResumeSessions` (auto-revive on reboot).
- `internal/runtime/roles.go` — `roleNameForPlan` (per-plan role override, piece
  5), `PursueRole`, and the built-in registry that will load `monitor`.
- `internal/runtime/builder.go` — `subAgentToolNames` split (piece 4): goal
  self-management always-on vs. delegation gated.
- `internal/runtime/agent_worker.go` — `prependNotifications` (why per-session
  notif won't reach a human; contrast with the new daemon feed, piece 2).
- `internal/memory/goals.go` — `GoalCreate` (origin marker via `parent_type`),
  `GoalGet`, `GoalUpdateStatus`, and a new `GoalUpdateDescription` (piece 6).
- `internal/config/config.go` — new `AgentConfig` / `Config.Agents`.
- `skills/roles/monitor.md` — the new read-only role (§6).
- `cmd/nine/daemon.go` — the boot reconcile loop, between
  `BootstrapSelfReflection` and `ResumeSessions`.
- New: `notify_user` core tool + daemon notification feed + `nine
  notifications` command (piece 2); cron scheduler (piece 1).
</content>
