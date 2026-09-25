---
name: turn-mechanics
description: What the loop does silently around you — history trimmed oldest-first, tool results discarded at the end of a turn, and failed tools already retried three times
tags: [context, history, scratchpad, retries, persistence, turn]
---

## What the loop does without telling you

Two mechanisms change what you should do, and neither announces itself:

1. **Tool results do not survive the turn.** Your scratchpad is cleared the
   moment you produce an answer. Only the conversation — user messages and your
   answers — carries forward.
2. **The oldest history is dropped when the budget is tight**, silently, with
   no marker. The conversation simply appears to start later than it did.

Both point the same way: **write down anything you will need later.** Your
context is not a record of the session, and treating it as one is how you end
up confidently wrong about what you already established.

### What survives a turn, and what does not

| | Survives | Notes |
|---|---|---|
| Your final answer | ✓ | Appended to history as an assistant message |
| The user's messages | ✓ | Appended as they arrive |
| Tool calls and their results | ✗ | The scratchpad is cleared on the answer, and again at the start of the next turn |
| Your reasoning between tool calls | ✗ | Same scratchpad |
| `memory_set` values | ✓ | Stored; read them back with `memory_get` |
| Files you wrote | ✓ | On the workspace filesystem |
| A goal or workflow | ✓ | Persisted; `goal_list` and `workflow_list` recover them |

So when a tool call produces something that matters beyond this turn — a
finding, a path, a version number, a decision — do one of three things before
you answer:

- Put it **in your answer**. That is the cheapest durable record, and the user
  wanted it anyway.
- `memory_set` it under a namespaced key, for a fact you will want in a later
  session.
- `write_file` it, for anything long.

Reporting "I've analysed the tree" without saying what you found means the
analysis is gone.

### Your context is trimmed from the front

When the assembled turn does not fit the budget, the builder drops entries from
the **front** of history, oldest first, until the rest fits. Nothing is
inserted to mark the gap.

The budget is spent in a fixed order, so what goes first is predictable:

| Order | Part | When the budget is tight |
|---|---|---|
| 1 | System core | Never trimmed |
| 2 | Tool definitions | The ranked set is cut to what fits |
| 3 | Self-model | Capped at 600 tokens, and skipped entirely if it will not fit |
| 4 | History | Oldest messages dropped |
| 5 | Scratchpad | Oldest tool results dropped, within the turn |
| 6 | Extras | Dropped first when anything is tight |

The self-model going first matters: it carries your `self/` keys and the three
ranked skill names. On a long, crowded turn you may have neither — which is
exactly when `skill_search` and `memory_get` earn their round-trip.

A long turn can also lose the *earliest* tool results while still running. If
you are working through many tool calls and an early result matters, restate it
in a later step rather than assuming you can still see it.

### A failed tool was already retried

A tool call that fails is retried automatically — **three attempts in total**
— before the error ever reaches you. Re-issuing the identical call with
identical arguments spends a turn repeating work the loop already did.

When you see a tool error:

| The message says | Do |
|---|---|
| "failed after 3 attempt(s) … try a different approach" | Change the approach: different arguments, a different tool, or a different route to the goal |
| "cannot succeed on retry; change the arguments or use a different tool" | The tool itself declared this unfixable by repetition. Do not retry it in any form. |

Read the error text before reacting. A path that does not exist, an argument
the schema rejects, and a service that is down all read differently and call
for different responses — and none of them is fixed by sending the same call
again.

## Limits

| Limit | Detail |
|---|---|
| You are not told when history is trimmed | The 90% warning goes to the human's client, not into your context. You cannot detect the gap; assume it may have happened on any long session. |
| No compaction or summary | Trimmed history is dropped, not summarized. What falls out is gone from the turn entirely. |
| The scratchpad is per-turn by design | It is cleared on the answer and at the start of the next `Run`. This is not a bug to work around; it is why durable notes exist. |
| Retry count is fixed | Three attempts, not configurable from a tool call. A tool needing more is a tool that should report non-retryable. |
| Tool definitions are charged before history | A turn with many ranked tools has less room for conversation than one with few. |
