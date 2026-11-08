---
name: process-management
description: Working with background processes from a conversation — find, inspect, message, start, stop, and remove them, and what each refusal means
tags: [processes, process_list, process_show, process_send, process_start, process_stop, background, budget]
---

## Background processes

A **process** is something that runs between conversations: a goal's session,
self-reflection, a watcher, or a standing tool. Each has an id, a state (`running`,
`stopped`, `failing`), a clock or other trigger, and a daily budget. Processes are
declared by the operator in nine.toml's `[[process]]` blocks (docs/processes.md).

### Finding and inspecting

- `process_list` — every process: id, state, who stopped it, trigger, budget used.
  Start here; ids are not guessable.
- `process_show <id>` — one in detail, with its recent activity and last error.

A goal's process is `goal:<goal id>`.

### Stop or remove — not the same thing

| You want | Use |
|---|---|
| It to stop for now, able to run again | `process_stop` |
| It gone for good | you cannot remove a process; tell the person to remove its `[[process]]` block from nine.toml — it is deleted at the next reconcile |

When the person says *remove*, *delete* or *get rid of* a process, the operator's
file is the only place it is removed from. Stopping it leaves it in the roster.

### Starting a stopped process

`process_start` succeeds for a process a model stopped, one that stopped itself, or
one that is failing. It is refused, with the reason, when:

| Stopped by | What to do |
|---|---|
| The operator | Tell the person: only the operator can start it. Do not try another way. |
| Its goal | Reactivate the goal with `goal_update_status` (status `active`); the process runs again on its own. |
| Its budget | Tell the person when it resumes; it runs again by itself when its day is over. |
| The `max_running` cap | Tell the person. Do not stop another process to make room unless they asked. |

A refusal is final for this turn. Report it plainly; retrying with different
arguments will not change it.

### Messaging a process

`process_send <id> <text>` hands a running live process a message as its next
trigger. It does not wait for an answer: the process answers through what it
writes — a file, memory, the notification feed. It is refused when the process is
stopped, busy, or a slice process; say so, or try again later if it was busy.

