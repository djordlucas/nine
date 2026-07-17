---
name: task-management
description: How to create, track, update, and complete background tasks
tags: [tasks, background, async, goals]
---

## Task Management

Tasks are units of work tracked in the memory store. Use them to break large requests into steps, track progress, and report completion.

### Creating a task

```
internal.task.create({
    "id": "<uuid>",
    "description": "Refactor the HTTP plugin to support retries",
    "status": "pending"
})
```

Generate a UUID with `shell({"command": "uuidgen"})` or use a short descriptive slug.

### Updating progress

```
internal.task.update({
    "id": "<id>",
    "status": "in_progress",
    "progress": ["Analysed existing code", "Wrote retry loop"]
})
```

Add one progress entry per meaningful step — keep them short (under 80 chars).

### Completing a task

```
internal.task.update({
    "id": "<id>",
    "status": "done",
    "progress": ["All steps complete", "Tests pass"]
})
```

### Listing tasks

```
internal.task.list({})
# → JSON array of all tasks with status and progress
```

### Best practices

- Create a task at the start of any multi-step job
- Update status to `in_progress` before starting work
- Add a progress entry after each significant step
- Mark `done` only when fully complete and verified
- Use `failed` status if the task cannot be completed; include the reason in progress

### Goals vs tasks

Goals are higher-level objectives that may span multiple conversations; tasks are the concrete steps to achieve them. A goal might generate several tasks.
