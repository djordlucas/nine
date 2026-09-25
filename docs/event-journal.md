# The event journal

Nine records what actually happened during a session — every model exchange and
every tool call — in an **append-only, ordered, typed journal** held in the
store. Entries are never edited or reordered; correcting the record means
appending to it.

This is separate from logs. Logs are for an operator reading output; the
journal is a structured behavioral record meant to be queried, replayed, and
reacted to.

## What it holds

Each inner model call is recorded with its **full request and full response**,
alongside the tool calls made and what they returned. A summary would record
that a tool was called; the journal records what the model was looking at when
it decided to call it.

| Entry | Records |
|-------|---------|
| `turn_start` / `turn_end` | the boundaries of one turn |
| `llm_request` / `llm_response` | one inner model call, whole in both directions |
| `thinking` | reasoning the model streamed before answering |
| `tool_start` / `tool_end` | one tool call, its arguments, and its result |
| `tool_http` | one outbound request a sandboxed tool made: host, URL, status, bytes, duration — **including a refused one, with its reason** |
| `context_update` | a change to what the next model call will be shown |

Entries carry a span and a parent span, so the record is a tree rather than a
flat list: an HTTP request nests under the tool call that made it, which nests
under the model call that chose it, which nests under the turn. `nine trace`
renders that tree for one session.

Checkpoints and the journal answer different questions and both are kept. A
checkpoint is a **state** — enough to resume a session. The journal is the
**history** — how that state was arrived at.

## Retention

A scrub runs at startup and keeps the most recent **200 turns per agent**
(`[memory] event_retention_turns`; a negative value keeps every turn). An age cap
can be added with `event_retention_days`, which is off by default, leaving the
turn window as the only bound.

Recording every model call whole is affordable because the window is bounded, not
because the entries are small. What falls outside it is gone: the journal is the
history of how a session got here, and a session resumes from its checkpoint
rather than from the record.

## Subscribing to journal events

The journal is an ordered stream, so components can **subscribe** to it and
react as entries arrive. Reactions maintain derived material — tags, links,
indices, notifications — that makes later turns better informed.

Four rules bound what a subscriber may do:

**Reactions are out-of-band.** A reaction never blocks a turn, never writes
into conversation history, and never steers the discussion. It writes only to
derived stores.

**Pull, not push.** Reactions build a substrate; the value reaches you only
when a later turn you initiated *pulls* it — for instance, the context builder
surfacing a related prior session under its existing relevance ranking and
token budget. Nine never interrupts you on its own initiative.

**No generative reactions.** Subscribers do not make generative model calls.
This bounds cost, keeps replay deterministic by keeping a nondeterministic step
out of the record, and prevents a reaction from triggering an event that
triggers another reaction.

**Embedding is allowed; generation is not.** Subscribers may compute embeddings
and use vector retrieval, which is what lets them relate sessions to each other
without writing anything new.

> The investigation behind the journal, and the delivery semantics worked out
> for subscriptions — [../adr/event-log.md](../adr/event-log.md) and
> [../adr/reactive-events.md](../adr/reactive-events.md).

## Limits

| Limit | Detail |
|-------|--------|
| A bounded turn window | The scrub keeps the last 200 turns per agent and runs at startup only, so a long-running daemon's journal grows until the next restart. Anything outside the window is deleted, not archived. |
| One subscriber ships | Session linking is the only subscriber built. The subscription mechanism is general; nothing else uses it yet. |
| No generative subscribers, by design | Subscribers may compute embeddings but may not make generative model calls. This bounds cost, keeps replay deterministic, and prevents a reaction from triggering another reaction. It is a deliberate constraint, not a gap. |
| Pull only, by design | A reaction never blocks a turn, writes to conversation history, or interrupts. Derived material reaches you only when a later turn you initiated pulls it. |
| Written off the critical path | An async batched sink writes the journal, so entries for an in-flight turn may not be readable the instant the turn ends. |
