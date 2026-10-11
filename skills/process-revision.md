---
name: process-revision
description: How a reviser rewrites a process or tool Nine wrote — tool_read, one rewrite, probation and rollback, and what it may not do
tags: [revision, reviser, tool_read, rollback, probation]
---

## Revising what you wrote

Every tool and process you write can be rewritten: `tool_write` again with the same
name. Nothing is lost — the previous version is kept (the last ten), and the operator
can restore any of them.

### Find what to change

1. `tool_read` with no name lists the tools you wrote: version, process state, last
   error.
2. `tool_read` with a name shows one in full: its source, its declared capabilities,
   and its process — trigger, role, state, failures, last error.
3. Look at what it produces: the files it writes, its memories, its rows in the
   database (`sql_query`). A process with no errors can still be doing the wrong thing.

### Make the change

- **One change per review.** Pick the one that matters most.
- **Rewrite the whole thing**: the complete source, the same `process` block, the same
  capabilities unless the change needs another. `tool_write` replaces; it does not patch.
- **Say why** in the description if it changes what the tool does.

### What happens next

| When | Then |
|---|---|
| You rewrite a process | It restarts on the new version, **on probation** |
| The new version handles its first trigger | Probation ends; it is the process now |
| It fails `rollback_after` times (default 3) before that | The previous version is restored, the person is told, and you get a message saying why |
| It runs but does worse | Nothing automatic: notice it in your next review and fix or restore it |

### What you may not do

| Refused | Why |
|---|---|
| Rewriting a process the operator stopped | Only the operator changes what they stopped |
| Writing or rewriting a process from a process's turn, without `[processes] allow_revision` | Revision is the operator's to allow |
| Shipped and operator-installed tools | They are not yours |
| A capability beyond the operator's ceiling | Ask with `capability_request`, from a conversation |
