# Contract — orchestration: sub-agents, workflows, goals

**Status:** Built · **Depends on:** agent loop, dispatcher, memory store, session plans · **Used by:** any delegating-role turn

Three related delegation mechanisms. All three are exposed as **core-intercepted tools**
gated to **delegating roles** with `depthGuard` as the recursion backstop (invariant I6, [`roles.md`](roles.md)), and all three reach daemon-private
state only through mediated handlers (invariant I4).

---

## Sub-agents

### R-ORCH.1 — tools

| Tool | Effect |
|------|--------|
| `run_agent` | spawn one child agent to execute a single self-contained task; return its result |
| `run_agents` | spawn several children in parallel and wait for all results |

`run_agent` input is `{task, context?, role?}`; `run_agents` input is a list of
`{task, context?, role?}` plus optional `timeout_seconds`. `role` optionally names the
child's worker role; omitted or unknown names resolve to the default leaf role
(`executor`) — see [`roles.md`](roles.md) R-ROLE.8/9. The default group timeout is the
daemon's `TaskTimeoutSeconds` = **1800s / 30 min**. Agents still running when the
timeout fires are **cancelled and marked `timed_out`**. (Only lower the default for
known-quick tasks.)

### R-ORCH.2 — execution model

A child runs a **fresh `agent.Loop` in its leaf role at depthGuard-1, synchronously**
(`RunSubAgentSync`).
`run_agents` runs its tasks **concurrently and joins** (waits for all to finish), then
returns the collected results to the parent as a single tool observation. Child lifecycle streams to
the parent (and through to the client) as `sub_agent_start` / `sub_agent_end{status}`
events (R-PROTO.3), each carrying the child's resolved leaf `role` so a client can
show which kind of agent is running.

### R-ORCH.3 — delegation termination (I6)

Termination is primarily structural: coarse leaf roles are non-delegating
(`delegates: false`, [`roles.md`](roles.md)). The `depthGuard` backstop
(`roles.max_delegation_depth`, default 2, decremented per spawn) hard-stops delegating
roles: with the default, a conversation can delegate, its `executor` child can delegate
once more, and the grandchild has no delegation tools at all (absent from its tool
list). This makes runaway recursion structurally impossible (R-ROLE.6).

---

## Workflows

### R-ORCH.4 — what a workflow is

A named, persistent, multi-step plan the LLM creates before delegating multi-step work to
sub-agents. Stored in `workflows` with steps as a **JSON array on the row** (no join for
the common "read the whole plan" case).

```schema
Workflow { id, agent_id, name, status ("active"|"done"|"failed"|"cancelled"), steps []Step }
Step     { id, label, status ("pending"|"running"|"done"|"failed"|"skipped"), result, failure_reason }
```

### R-ORCH.5 — package boundary (I3)

Workflow domain logic lives in a `workflow.Service` that depends only on a narrow
`Repository` interface (`Insert/Load/Save/ListActive/ListRecent/Notify`) — it holds no
database handle. The memory store implements the repository and exposes `Workflow*`
methods as thin delegations. This keeps the single-gateway invariant while moving domain
rules out of persistence.

### R-ORCH.6 — tools (delegating roles)

`workflow_create`, `workflow_update`, `workflow_get`, `workflow_list`,
`workflow_retry_step`. When a workflow is active, the system prompt **SHOULD** steer the
agent to `workflow_list` at the start of a turn and `workflow_get` before deciding what
to do next.

### R-ORCH.7 — auto-close

When `workflow_update` marks a step `done`/`failed`, it checks whether all steps are
terminal; if so it auto-closes the workflow as `done` (all succeeded) or `failed` (any
failed). The LLM needs no explicit close call.

### R-ORCH.8 — operator commands

- `workflow_stop <id>` (daemon up): mark workflow `cancelled`, pending steps `skipped`,
  running steps `failed (stopped)`. In-flight sub-agents finish but their results are
  discarded (their execution is not forcibly terminated).
- `workflow_fail <id>` / `--all`: post-mortem; mark workflow(s) `failed`. **Works with
  the daemon down** by running the store as a one-shot process.

### R-ORCH.9 — startup scrub

On every daemon start, mark all `running` steps `failed (interrupted)` and auto-close
workflows now fully terminal. Workflows with remaining `pending` steps stay `active` so
the LLM resumes them. This recovers from ungraceful shutdowns.

---

## Goals

### R-ORCH.10 — what a goal is

A persistent, open-ended intention with no defined end condition (e.g. "monitor this repo
for security issues"). Stored in `goals`:

```schema
Goal { id, description, status ("active"|"paused"|"done"|"archived"),
       parent_id, parent_type ("conversation"|"goal") }
       // `subtree` is returned by goal_get, derived from children's parent_id — not stored
```

The LLM creates and decomposes goals **autonomously** — no user approval to spawn
sub-goals or tasks.

### R-ORCH.11 — tools (delegating roles)

`goal_create`, `goal_get`, `goal_list`, `goal_update_status`.
`goal_create` defaults `parent_id`/`parent_type` to the owning conversation when no parent
is given. `goal_list` is also reachable as a read-only daemon proxy (`nine goals`) without
an agent loop.

### R-ORCH.12 — pursue session spawning (goal-spawning roles only)

`goal_create` for a **top-level** goal (`parent_type: "conversation"`) spawns a `pursue`
session via `SpawnGoalSession` (see [`session-plans.md`](session-plans.md) R-PLAN.9):

- **Idempotent** — if a session for that goal ID already runs, it's a no-op.
- **Capped** — `max_goal_sessions` (default **10**) bounds concurrently-running pursue
  sessions. At the cap, the goal is still recorded but no session spawns.
- `goal_create`'s response includes `pursue_session: "spawned" | "limit_reached"`.
- **Sub-goals** (`parent_type: "goal"`) do **not** get their own session — they are
  worked on inside the parent goal's pursue loop.

Only loops whose role has `spawns_goals` (the orchestrator) are given the spawn
function, so only top-level goals get background sessions (R-ROLE.1).

---

## Reference symbols

`internal/agent/register_subagents.go` (`run_agent`/`run_agents`),
`internal/runtime/subagent.go` (`RunSubAgentSync`), `internal/workflow/` (`Service`,
`Repository`), `internal/memory/workflows.go`, `internal/agent/register_*.go` (`goal_*`,
`workflow_*` wiring), `internal/runtime/goal_session.go` (`SpawnGoalSession`,
`DefaultMaxGoalSessions`, `PursueIdleInterval`).

---

## R-ORCH.13 — the goal tree has exactly one representation

A goal's parent is `parent_id` (with `parent_type`), written when the child is created.
That edge is the **only** stored form of the relation: an implementation **MUST NOT**
keep a second, parent-side copy of its children.

`goal_get` still returns a `subtree` array — the child ids — but it is **derived** from
`parent_id` at read time, and there is no tool for writing it.

The rule exists because the alternative was tried. A stored `subtree` column was
append-only free text, written solely by a `goal_append_subtree` tool, read by nothing in
the daemon, and reaching the model only by riding along in `goal_get`. Keeping it in step
with `parent_id` was therefore delegated to the model, by an orchestrator prompt that
asked it to record every spawn twice. That is a denormalized index of a relation the
schema already enforces: **wrong at some rate, unverifiable, and costing a tool slot in
every context that carries goal tools.**

Deriving it instead makes the two impossible to disagree, and leaves the model-facing
shape of `goal_get` unchanged.
