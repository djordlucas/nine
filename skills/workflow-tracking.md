---
name: workflow-tracking
description: Track multi-step delegated work with workflow_create, workflow_update, workflow_list and workflow_retry_step
tags: [workflow, steps, delegation, tracking, recovery]
---

## Workflow tracking

Create a workflow before delegating multi-step work, and call `workflow_list`
at the start of every turn. A workflow is a persistent ledger of a plan you
made: it survives a daemon restart, so it is how you find out what you were
doing when a turn picks up after one.

A workflow is passive. It has no session, no scheduler and no driver — it
advances only when you call a `workflow_*` tool inside a turn. Nothing updates
it on your behalf.

| Tool | Use |
|---|---|
| `workflow_list` | Start of every turn — active workflows plus the 10 most recent completions |
| `workflow_create` | Before delegating work that takes more than one step |
| `workflow_update` | After each step finishes, succeeds or fails |
| `workflow_get` | The full plan and current step statuses for one workflow |
| `workflow_retry_step` | Reset a failed step to pending and re-run it with a new sub-agent |

### Creating a workflow

```
workflow_create({
    "name": "Add retries to the HTTP plugin",
    "steps": [
        "Read the current request path",
        "Add a bounded retry loop",
        "Write a test for the retry path",
        "Run go build and go test"
    ]
})
# → {"workflow_id": "...", "steps": [{"step_id": "...", "label": "..."}, ...]}
```

Steps are ordered labels, not executable instructions — write each one as the
outcome a step produces, so a later turn can tell from the label alone whether
the work is done.

Keep the plan to the steps you actually intend to delegate. A workflow of one
step is bookkeeping with no reader.

### Updating a step

```
workflow_update({"workflow_id": "...", "step_id": "...", "status": "running"})

workflow_update({
    "workflow_id": "...",
    "step_id": "...",
    "status": "done",
    "result": "Retry loop added in client.go; 3 attempts, exponential backoff."
})
```

Statuses are `running`, `done`, `failed`, and `skipped`. Pass `result` on
success and `failure_reason` on failure — the next turn reads those, not your
recollection. The workflow closes automatically once every step reaches a
terminal status.

### Recovering a failed step

```
workflow_retry_step({"workflow_id": "...", "step_id": "..."})
```

This resets the step to pending and immediately re-runs it with a new
sub-agent. Retry when the failure was transient. When the failure was the plan,
mark the step `skipped` and create a workflow for the corrected approach
instead — a step that fails the same way three times is a plan problem.

### Workflows, goals, and delegation

| Concept | What it is | What drives it |
|---|---|---|
| **Workflow** | A ledger of a multi-step plan you are delegating | You, inside a turn |
| **Goal** | A persistent, open-ended intention with no end condition | Its own background session, on a timer |
| **Sub-agent** | One worker executing one step | A `run_agent` call |

Use a workflow when the work has a known end and a known sequence. Use a goal
when it does not — see the `goal-pursuit` skill. A workflow step is typically
one `run_agent` call; see the `delegation` skill for choosing the worker.

## Limits

| Limit | Detail |
|---|---|
| Nothing advances a workflow but you | A step left `running` stays `running` across restarts until a turn updates it. |
| Steps are labels, not a program | Creating a workflow schedules no work; it records a plan you still have to execute. |
| No step insertion | The step list is fixed at creation. A changed plan needs a new workflow. |
| Depth-capped | `workflow_*` is gated like `run_agent`: a sub-agent deep in a delegation chain may not have it. |
