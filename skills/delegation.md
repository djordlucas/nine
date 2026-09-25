---
name: delegation
description: Delegating work with run_agent and run_agents — when it pays, how to write the task, and picking the narrowest role
tags: [delegation, sub-agents, roles, run_agent, parallel]
---

## Delegation

Delegate when a piece of work is self-contained and would otherwise flood your
context — reading a large tree, running a long build, researching several
sources. A sub-agent runs in its own context and returns only its final answer,
so what it read never reaches you. Do the work yourself when it is a single
tool call, when you need the intermediate output, or when the task needs
context you would have to explain at length.

| Tool | Use |
|---|---|
| `run_agent` | One self-contained task; returns its final answer |
| `run_agents` | Several independent tasks at once; returns when all finish or the timeout fires |

### Picking a role

Pick the **narrowest role that fits**. A role is a tool boundary: a worker that
cannot run a shell cannot accidentally run one. Omitting `role` gets
`executor`, which carries everything.

| Role | Can | Use it for |
|---|---|---|
| `software-dev` | Shell, file read/write/edit/search, memory | Implement or modify code, run builds and tests |
| `sysadmin` | Shell, file read/write, `http_get`, `http_post`, memory | Inspect and operate the system, check service state |
| `report-writer` | Web search, page reads, `http_get`, full file tools, memory | Research and write; no shell |
| `monitor` | Web search, page reads, `http_get`, file **reads**, memory | Check on something without changing anything |
| `executor` | Everything, and may sub-delegate | Anything none of the above covers |

An operator or a prior turn may have added roles beyond these five. The live
list, with each role's description, is in the `role` field of the `run_agent`
schema — read it there rather than assuming these are all of them. An unknown
role name falls back to `executor` rather than failing.

### Writing the task

```
run_agent({
    "role": "software-dev",
    "task": "Add a bounded retry loop to the HTTP client in internal/builtins. Three attempts, exponential backoff. Run go build ./... and go test ./internal/builtins/ before reporting.",
    "context": "The existing client has no retry. Tests live beside the source."
})
```

The sub-agent sees `task` and `context` and nothing else — not your
conversation, not what you already read, not what the user said. A task that
assumes shared context produces a worker that guesses.

- **State the finished condition.** "Run the tests and report the failures" is
  a task; "look at the tests" is not.
- **Name the paths.** The worker has to find the file; you already know where
  it is.
- **Keep `context` short.** Background the worker genuinely needs, not a
  transcript.
- **Do not ask questions in a task.** Sub-agents are non-interactive and cannot
  raise them; they make an assumption and proceed.

### Running several at once

```
run_agents({
    "tasks": [
        {"role": "report-writer", "task": "Summarize docs/roles.md in 5 bullets"},
        {"role": "report-writer", "task": "Summarize docs/workflows.md in 5 bullets"},
        {"role": "monitor", "task": "Report the current size of dist/"}
    ],
    "timeout_seconds": 600
})
```

Use `run_agents` only for tasks that are genuinely independent. Two workers
editing the same file will conflict, and nothing coordinates them. When step
two needs step one's answer, that is two `run_agent` calls, not one
`run_agents`.

`timeout_seconds` defaults to the daemon's task timeout (typically 30 minutes).
Lower it only when you know the tasks are quick — agents still running when it
fires are cancelled and marked `timed_out`.

### Delegation and workflows

Delegating more than two steps means keeping a `workflow` alongside, so a turn
after a restart can tell what finished. Create the workflow first, mark each
step `running` before its `run_agent` call, and `done` or `failed` with the
worker's result after. See the `workflow-tracking` skill.

## Limits

| Limit | Detail |
|---|---|
| Depth is capped | `[roles] max_delegation_depth` (default 2) decrements on every spawn. At zero the delegation tools are not registered, so a deep worker cannot delegate at all. |
| Leaf roles do not delegate | `software-dev`, `sysadmin`, `report-writer` and `monitor` have no `run_agent`. Only `executor` sub-delegates. |
| A role can only narrow | A `tools` list never grants a tool the daemon lacks. Naming a role cannot expand what is reachable. |
| One-shot, no conversation | A sub-agent returns a final answer and ends. You cannot ask it a follow-up; you spawn a new one. |
| Sub-agents are non-interactive | `ask_human` is unavailable to them at any depth. Approval gates still apply, inherited from the owning session. |
