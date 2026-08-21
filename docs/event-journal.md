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
alongside the tool calls made and what they returned. Recording exchanges in
full is what makes the difference between knowing that a session went wrong and
being able to see why: a summary tells you a tool was called, the journal tells
you what the model was looking at when it decided to call it.

Checkpoints and the journal answer different questions and both are kept. A
checkpoint is a **state** — enough to resume a session. The journal is the
**history** — how that state was arrived at.

## Retention

Sessions that are closed have their journal pruned below the most recent
checkpoint, and a scrub runs at startup. Full-fidelity recording is only
affordable if it is bounded, and history below a checkpoint is history you can
no longer resume into.

## Subscribing to journal events

Because the journal is an ordered stream and not just a record, components can
**subscribe** to it and react as entries arrive. Reactions maintain derived
material — tags, links, indices, notifications — that makes later turns better
informed.

Four rules bound this, and they are what keep it from becoming an agent that
talks to you:

**Reactions are out-of-band.** A reaction never blocks a turn, never writes
into conversation history, and never steers the discussion. It writes only to
derived stores. Every other guarantee follows from this one.

**Pull, not push.** Reactions build a substrate; the value reaches you only
when a later turn you initiated *pulls* it — for instance, the context builder
surfacing a related prior session under its existing relevance ranking and
token budget. Nine never interrupts you on its own initiative.

**No generative reactions.** Subscribers do not make generative model calls.
This bounds cost, keeps replay deterministic by keeping a nondeterministic step
out of the record, and — most importantly — removes the possibility of a
reaction triggering an event that triggers another reaction. A programmatic
reaction produces bounded, structured output that cannot spiral.

**Embedding is allowed; generation is not.** Subscribers may compute embeddings
and use vector retrieval, which is what lets them relate sessions to each other
without writing anything new.

> The investigation behind the journal, and the delivery semantics worked out
> for subscriptions — [../adr/event-log.md](../adr/event-log.md) and
> [../adr/reactive-events.md](../adr/reactive-events.md).
