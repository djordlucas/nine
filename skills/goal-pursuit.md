---
name: goal-pursuit
description: Persistent open-ended goals — goal_create, goal_list, goal_update_status, and the background session that works on a goal between turns
tags: [goals, background, pursue, monitoring, persistence]
---

## Goals

Create a goal for work with **no defined end condition** — "monitor this repo
for security issues", "keep the dependency tree current". A top-level goal gets
a background session that wakes on a timer and works on it between your turns,
which is what makes a goal different from every other kind of tracked work.

Call `goal_list` at the start of any turn involving open-ended or persistent
work. It is how you find goals that already exist, and how you recover after a
restart.

| Tool | Use |
|---|---|
| `goal_list` | Start of a turn — every goal and its status |
| `goal_create` | Record an open-ended intention |
| `goal_get` | One goal with its sub-goal and task tree |
| `goal_update_status` | Move a goal between `active`, `paused`, `done`, `archived` |

### Creating a goal

```
goal_create({"description": "Watch this repository for new CVEs in its dependencies"})
# → {"goal_id": "...", "pursue_session": "spawned"}
```

The parent defaults to the current conversation. **Check `pursue_session` in
the response** — it is `spawned` when the background session started, and
`limit_reached` when the daemon is already at its cap for concurrent goal
sessions. At `limit_reached` the goal is still recorded but nothing works on it
between turns; either finish or pause an existing goal, or say so rather than
assuming it is being pursued.

A sub-goal names its parent and gets no session of its own:

```
goal_create({
    "description": "Audit the vendored crypto dependencies",
    "parent_id": "<goal id>",
    "parent_type": "goal"
})
```

The session already pursuing the parent works on it. That is what keeps a
branching goal from becoming a branching population of sessions.

### What a pursue turn does

An idle goal session wakes roughly every five minutes and is handed a turn
asking it to check the goal and its subtree, take any useful action, and update
the status if it should change. That turn runs the ordinary agent loop at
background priority — it is not a special mode.

When you are that session:

1. `goal_get` the goal to recall exactly what you are watching.
2. Compare what you find now against what you recorded before, in memory or in
   the goal's subtree.
3. Act, or delegate the acting — a pursue session can spawn sub-goals and
   sub-agents.
4. `notify_user` only when something genuinely warrants attention. Nobody is
   reading a transcript; that tool is the whole channel.
5. `goal_update_status` when the status has actually changed.

### Status is yours to set

```
goal_update_status({"goal_id": "...", "status": "done"})
```

| Status | Means |
|---|---|
| `active` | In progress; the session wakes on its timer |
| `paused` | Temporarily on hold; the routine pauses with it |
| `done` | Completed or resolved; the routine retires |
| `archived` | Retired without completing |

The session's schedule follows the goal, so setting `done` on a goal that is
not finished stops the work. Set `paused` when you are blocked and `archived`
when the goal stopped being worth pursuing.

### Goals, workflows, and delegation

| Use | When |
|---|---|
| **Goal** | Open-ended, no end condition, must continue between turns |
| **Workflow** | Known steps, known finish, advanced by you inside a turn |
| **`run_agent`** | One self-contained piece of work, right now |

A goal is not a to-do list. Work that ends when the steps are done is a
workflow — see the `workflow-tracking` skill.

## Limits

| Limit | Detail |
|---|---|
| Only top-level goals get a session | A sub-goal is worked on by its parent's session or not at all. |
| Concurrent sessions are capped | `goal_create` returns `pursue_session: "limit_reached"` at the cap, and the goal sits inert. |
| Spawning is idempotent | A goal that already has a session never gets a second one, whatever asks. |
| Role-gated | Only roles with `spawns_goals` create pursue sessions. A leaf sub-agent may have no goal tools at all. |
| No goal deletion | A goal ends by becoming `done` or `archived`; there is no delete. |
