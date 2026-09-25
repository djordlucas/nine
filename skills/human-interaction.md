---
name: human-interaction
description: Reaching a human — ask_human in an interactive session, notify_user from a background one, and the queued-message buffer
tags: [hitl, ask_human, notify_user, queued-messages, interaction]
---

## Reaching a human

Which tool you have depends on what kind of session you are, and you cannot
choose: a tool you do not see is not available to you.

| Session | Tool | What it does |
|---|---|---|
| Interactive (a person in the TUI) | `ask_human` | Pauses the turn and waits for an answer |
| Background (goal, pursue, standing agent) | `notify_user` | Posts to a feed the human reads later; does not wait |
| Sub-agent, `nine query` | neither | Make a reasonable assumption and proceed |

### `ask_human` — when a person is there

```
ask_human({
    "question": "Deploy to staging or production?",
    "options": ["staging", "production"]
})
```

Ask when a decision is genuinely the human's — an irreversible action, a
choice between paths with different consequences, or a requirement you cannot
infer. Do not ask to confirm something you can check yourself, and do not ask
for information the workspace already holds.

Pass `options` whenever the answer is one of a known few. A free-text question
gets a free-text answer you then have to interpret.

The same mechanism carries **approval gates**: a tool the operator marked as
requiring approval is intercepted before it runs, and the human permits or
refuses it. Gates follow the session that owns the work, so a sub-agent
reaching for a gated tool still puts the question to the person who started the
conversation — delegating a risky call is not a way around approval.

### `notify_user` — when nobody is watching

```
notify_user({"text": "Three dependencies picked up CVEs overnight; details in the goal subtree."})
```

A background session has no conversation to speak into, so this is the entire
channel to a human. Use it only when something genuinely warrants attention —
a feed of routine "still watching" posts is one the human stops reading. One or
two sentences, specific, with the finding in them rather than a pointer to go
look.

### Queued messages

A message the user sends while you are mid-turn goes into a per-session queue
rather than into the current turn's context. You are told it exists; you are
not given it.

```
queued_messages_unconsumed_count({})   # is there anything new?
queued_messages_get({})                # read them, with consumption status
queued_message_mark_consumed({"index": 0})
queued_messages_mark_all_consumed({})
```

Marking a message consumed moves it into conversation history, where it appears
in your context on later turns. Read the queue when you reach a natural
checkpoint and the new message could change what you do next — a correction or
a cancellation is exactly what people send mid-turn.

Nothing is lost by leaving a message in the queue. After the turn ends the
worker drains one unconsumed message and starts a fresh turn with it. Finishing
the current turn first is a legitimate choice; the queue exists so a message in
flight does not derail reasoning already underway.

## Limits

| Limit | Detail |
|---|---|
| `ask_human` is interactive-only | Sub-agents, goal and pursue sessions, and `nine query` never get it, at any delegation depth. |
| `notify_user` needs a goal-owning role | An ordinary sub-agent has neither tool and must decide for itself. |
| `notify_user` does not wait | It posts and returns. There is no reply channel; the human reads it with `nine notifications`. |
| Answers are not guaranteed | An `ask_human` call waits on a person who may be away; the turn's own budget still applies. |
| Indices span the whole queue | `queued_message_mark_consumed` takes a 0-based index over every queued message, consumed and unconsumed alike. A consumed message keeps its slot, so indices from `queued_messages_get` stay valid. |
