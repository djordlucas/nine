# Contract — Journal Subscriptions & Reactive Enrichment

**Status:** Built · **Depends on:** event journal, memory store, embedder · **Used by:** supervisor, related-session indexer, context builder

The event journal ([`event-journal.md`](event-journal.md)) is **subscribable**: durable
per-subscriber cursors let programmatic handlers react to events as they land. The scope
is deliberately narrow — reactions are **out-of-band** and **enrich derived stores** that
later user-initiated turns *pull* from. Full design: `docs/reactive-events.md`.

---

## R-SUB.1 — The subscription primitive

A subscriber is a `subscribe.Handler`:

```text
Handler { ID() string            // stable identity = durable cursor key
          Types() []string       // event types to receive; empty = all
          Handle(ctx, event) error }
```

A `subscribe.Subscription` (`subscribe.New(store, handler)`, driven by `Run(ctx)`) reads
forward from the handler's persisted cursor (`SessionEventsAfter` + `EventCursorGet/Set`),
delivers matching events in `seq` order, and advances the cursor per batch. Delivery is
**at-least-once** — handlers **MUST** be idempotent. A handler error on one event is
**logged and skipped**, never wedging the cursor (poison-event tolerance). The runner is a
plain goroutine, **off the turn path**.

---

## R-SUB.2 — Wake + catch-up

`Notify()` gives an in-process, low-latency wake (the event sink's flush calls
`Daemon.NotifySubscribers`); a poll tick is the fallback. The durable **cursor is the
source of truth**, the wake is only a hint — a subscriber that was down catches up from
its cursor on restart. (Postgres `LISTEN/NOTIFY` is the deferred multi-process
generalization; single-node uses the in-process `Notify`.)

---

## R-SUB.3 — Out-of-band discipline (the guarantees)

A subscriber **MUST NOT**:

- make a **generative** LLM completion (embeddings are allowed — a bounded, single-vector
  call);
- write conversation history or otherwise mutate an active session;
- block or enqueue a turn.

It **MAY** write only *derived* stores (cross-session links, indices, metrics, the
notification feed). This is what bounds cost, preserves replay determinism, and eliminates
reaction→event→reaction feedback loops. **Enrich, don't interject.**

---

## R-SUB.4 — The related-session indexer

`subscribers.RelatedIndexer` (config-gated, on by default when an embedder is configured:
`[daemon] related_sessions_index`) subscribes to `turn_end`. On each completed turn it:

1. embeds the answer;
2. vector-searches prior sessions (`session-index` namespace) and records links to
   topically-similar ones in `related_sessions` (upsert; threshold-gated ≥ 0.75,
   deduped per agent);
3. adds this turn's vector to the index.

It is idempotent (per-event vector id + upsert) and makes **no generative call**. Without
an embedder it is a no-op.

---

## R-SUB.5 — Pull surfacing (how value reaches the user)

Derived state is inert until a **user-initiated** turn pulls it. The context builder may
surface one recorded `related_sessions` link as `SystemEnrichment` (priority 2.6, capped,
dropped first under budget — see [`context-builder.md`](context-builder.md)), gated so it
appears **only when relevant to the current query** (the current query must itself be
≥ 0.6 similar to the linked session's index vector) and **never proactively**. The
surfaced payload is a short gist from the linked session's latest `turn_end`
(`LatestTurnResult`). Surfacing threshold (0.6) is deliberately below the linking
threshold (0.75): a question scores structurally lower against a prior answer than two
answers do against each other.

---

## R-SUB.6 — The supervisor as a subscriber

The supervisor's control-plane bus is folded onto the journal: it `Post`s events as the
`supervisor` type and consumes them via a cursor-backed subscription, surviving restart
(see [`supervisor.md`](supervisor.md) R-SUP.1). This is the same primitive as R-SUB.1
applied to lifecycle events.

---

## R-SUB.7 — Deferred by decision (not forgotten)

Generative-LLM reactions and autonomous session injection ("free will") are **out of
scope**: reactions stay programmatic and out-of-band. `LISTEN/NOTIFY`, JSONB
indexes/partitioning, and request dedup are deferred as premature at single-node volume.

---

## Reference symbols

`internal/subscribe/` (`Handler`, `Subscription`, `New`, `Run`, `Notify`, `Store`),
`internal/subscribers/related.go` (`RelatedIndexer`), `internal/memory/cursors.go`
(`SessionEventsAfter`, `EventCursorGet/Set`), `internal/memory/related.go`
(`RelatedSessionAdd`, `RelatedSessions`), `internal/runtime/related_surface.go`
(`relatedEnrichmentFn` — the pull-surfacing seam), `internal/runtime/supervisor.go`
(supervisor-as-subscriber). Design: `docs/reactive-events.md`.
