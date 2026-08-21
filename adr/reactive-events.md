# Event subscriptions — reacting to the session journal

**Status:** Proposed (design note) · **Depends on:** the session event journal
(event-log.md v1–v3), the supervisor bus, the vector store + the embedder (phase 0),
the `llm.Queue` priority tiers, the context builder, the notification feed ·
**Downstream of:** event-log.md (this builds on the durable journal; it does
not replace it)

This note proposes making the session event journal **subscribable**, so Nine
(and platform plugins) can *react* to interesting events as they happen —
turning the log from a past-facing **record** (replay, audit, debug) into a
present-facing **stream**. It also fixes the scope deliberately narrow: reactions
are **programmatic and out-of-band**, never generative LLM calls injected into a
live session. The narrow scope is the point — it's what keeps Nine useful rather
than invasive.

---

## 1. TL;DR — recommendation

The journal (event-log.md) is already append-only, ordered, and typed. Add a
**subscription layer** on top of it: durable per-subscriber cursors over `seq`,
live wake via an in-process notify, catch-up-after-restart from the last
cursor. Subscribers are **programmatic** handlers that run **off the turn path**
and **enrich derived stores** — topic tags, cross-session links, indices,
metrics, the notification feed — which *later* turns or the user **pull** from.

The turn from reactive to proactive agent is the prize here (it's the mechanism
the roadmap's "IO plugins / free will" items lack). But the same mechanism, used
carelessly, makes Nine invasive and hard to reason about. So the recommendation
draws a hard line: **enrich, don't interject.**

---

## 1a. Decisions taken (2026-07-07)

- **Reactions are out-of-band and never mutate the active session.** A reaction
  must not block a turn, write into conversation history, or otherwise steer the
  discussion. It runs on its own goroutine and writes only to *derived* stores
  (tags, links, indices, metrics, the notification feed). This is the primary
  guarantee protecting the user experience; everything else follows from it.
- **Pull, not push.** Reactions build up a knowledge substrate; the value reaches
  the user only when a later, user-initiated turn *pulls* it — e.g. the context
  builder optionally surfacing a subscriber-maintained "related prior session"
  under its existing relevance ranking and token budget. Nine never proactively
  interrupts.
- **No generative LLM reactions.** Subscribers do not make generative LLM
  completions. This bounds cost, preserves replay determinism (no nondeterministic
  generative step enters the behavioral record), and — critically — **eliminates
  the reaction→event→reaction feedback loop** at the source: a programmatic
  reaction produces bounded, structured output that cannot spiral. The
  generation/depth-limiting machinery a generative tier would require is therefore
  unnecessary in this scope.
- **Embedding / vector retrieval is allowed; generative completion is not.**
  Embeddings are a bounded, cheap, single-vector model call (not a generative
  turn), and the vector store + the embedder already back it. Allowing them keeps
  semantic reactions ("is this related to an earlier discussion?") cheap and
  passive; keyword/regex/structural matching remains available for subscribers
  that want zero model calls.
- **Subscribability is decoupled from event-sourcing of state.** This feature
  needs a durable, typed, ordered, *subscribable* stream — not state modeled as a
  projection of events. It can ship without converting any aggregate to CQRS
  (event-log.md §8a v5 remains a separate, optional track).

### Explicitly rejected / deferred (and why)

- **Generative LLM reactions** (e.g. an LLM subscriber that reads an event and
  decides what to do): deferred. Per-event or per-turn generative calls explode
  cost, reintroduce nondeterminism into behavior, and open feedback loops; the
  "interestingness" gate that would tame them is itself a hard, possibly-LLM
  problem. Revisiting requires: a strict pre-filter (cheap programmatic gate
  before any generative call), lowest-priority queue scheduling so reactive work
  never starves a user, hard per-subscriber budgets/rate-limits, and a
  generation/lineage marker (the journal's `span_id`/`parent_span_id` gives it a
  home) with loop detection. Out of scope until there's a concrete need the
  programmatic tier cannot meet.
- **Session injection / autonomous turn generation** ("free will"): deferred. The
  *substrate* for it lands here (a subscribable stream), but the injecting
  subscriber is exactly the invasive/drift-prone thing this scope rules out.

---

## 2. The reframe: record → stream

event-log.md v1–v3 treat the journal as a **record**: write once, read back
later for replay/audit/debug. Nothing consumes it live. "Listening" adds a second
consumer class that reads the stream **as it grows** and acts on it — the "log as
integration backbone" pattern.

This is what separates a **reactive** agent (acts only on a user turn or an idle
timer) from a **proactive** one (acts on its own observations). Under the scope
above, "acts" means *enriches derived knowledge*, not *starts talking*: the
proactivity is in what Nine *notices and files away*, not in what it *says
unprompted*.

## 3. Delivery semantics (this is where it gets real)

As a pure journal, the v1 sink is best-effort / drop-on-full — fine, because
nothing depends on any single event. The moment a subscriber drives behavior,
delivery guarantees stop being optional:

- **At-least-once, per-agent ordered.** A subscriber sees every event for an
  aggregate in `seq` order. Handlers must be **idempotent** (a redelivered event
  must not double-count / double-tag) — the durable cursor makes at-least-once,
  not exactly-once, the honest contract.
- **Durable cursor per subscriber.** Each subscriber records the last `seq` it
  processed. On restart it resumes from there — a subscriber that was down catches
  up rather than losing the window (the gap the in-memory supervisor bus has
  today).
- **Live wake + catch-up.** An in-process notify wakes subscribers on new
  events for low latency; the `seq` cursor covers everything missed while asleep
  or between daemon runs. Notify is a hint, the cursor is the truth.
- **Best-effort tier still exists.** High-volume observability events
  (`response_chunk`, per-token) need not be subscribable; subscribers opt into the
  event types they care about.

## 4. Subscriber model

- A subscriber is a small handler: `(event) -> writes to a derived store`. It runs
  on its own goroutine, **never on the turn path**, so a slow or failing
  subscriber cannot slow a user turn (the out-of-band guarantee, §1a).
- **What subscribers produce** (derived state only): topic/entity **tags** on
  sessions; **cross-session links** ("this turn resembles session X" by vector
  similarity);
  **indices** (topic → sessions); **metrics** (tool error rates, stall causes,
  token usage over time); and entries in the existing **notification feed** (which
  is already user-*pull*, not pushed into a session).
- **What subscribers must not do:** call a generative LLM, write conversation
  history, block or enqueue a turn, or emit content the current session consumes
  mid-turn.

## 5. How value reaches the user (pull, not push)

The derived stores are inert until a **user-initiated** turn pulls them. The
natural seam is the **context builder**, which already assembles the self-model
and ranks tools by relevance under a token budget: a subscriber-maintained
"related prior session" or topic index becomes one more optional input the
builder may surface *when relevant and within budget*. So:

- the LLM engages only on turns the user asked for;
- enrichment appears only when relevant (existing relevance ranking);
- it cannot bloat context (existing budget);
- nothing is ever injected proactively.

The reaction did the cheap indexing off to the side; the model benefits from it
only when the user's own question makes it relevant.

## 6. What this unlocks

- **Cross-session/cross-time association** — "have we discussed this before?" as a
  *queryable link* a later turn surfaces, not an interruption. Feeds the roadmap's
  "knowledge hoarding/dumping" and "persisted world data shared across nines".
- **A durable, replayable control-plane** — the supervisor bus
  (`agent_completes`, `goal_stalls`, `gap_reported`, `plugin_crashed`) is a
  subscriber over persisted lifecycle events; its reactions survive restart
  (event-log.md §8a's first ES step, reached from the subscription side).
- **Extensibility without touching the loop** — new passive behaviors are new
  subscribers, not core-loop changes.
- **The substrate for proactive IO plugins / "free will"** — present, but the
  injecting subscriber that would use it is deliberately deferred (§1a).

## 7. Risks even in the narrow scope

- **Subscriber lag / backpressure.** A slow subscriber falls behind the stream;
  its cursor bounds correctness but not freshness. Acceptable for enrichment
  (eventually-consistent by design), but monitor lag.
- **Index growth / staleness.** Derived stores grow unbounded and can go stale;
  they need the same retention discipline as the journal (event-log.md v4
  prune-below-checkpoint) and a rebuild path (re-fold from `seq`).
- **Poison events.** A handler that errors on one event must not wedge the cursor;
  log-and-skip (or a dead-letter mark) with idempotent retry.
- **Embedding cost, if unbounded.** Even non-generative, embedding every event is
  wasteful — subscribers should embed selectively (e.g. once per turn on the final
  answer, not per chunk).
- **Surfacing that drifts.** The context-builder pull must stay under the relevance
  + budget gate; a too-eager "related session" surfacer would reintroduce drift
  through the back door. Keep the surfacing conservative and evaluable.

## 8. Phasing

Each phase is independently shippable and downstream of event-log.md.

1. **Subscription primitive. ✅ Done (2026-07-07).** `internal/subscribe`: a
   `Handler` (id = durable cursor key, opt-in `Types`, idempotent `Handle`) driven
   by a `Subscription` that reads forward from the handler's persisted cursor
   (`memory.SessionEventsAfter` + `event_cursors` table via `EventCursorGet/Set`),
   delivers in `seq` order, advances the cursor per batch, and waits on an
   in-process `Notify()` or a poll tick between drains. Delivery is at-least-once
   (idempotent handlers); a poison event is logged and skipped, never wedging the
   cursor; the runner is a plain goroutine, off the turn path. *Gate met:*
   `TestSubscriptionDeliversInOrderAndFilters` and
   `TestSubscriptionResumesFromCursorAfterRestart` (a fresh subscription with the
   same id resumes from the cursor and processes only new events). **In-process
   `Notify` replaces `LISTEN/NOTIFY`** for the single-node daemon — the durable
   cursor is the source of truth, the wake is only a latency hint; `LISTEN/NOTIFY`
   remains the multi-process generalization when/if the writer and subscribers
   split across processes.
2. **First programmatic subscriber. ✅ Done (2026-07-07).** `subscribers.RelatedIndexer`
   (`internal/subscribers/related.go`): on `turn_end`, embed the answer,
   vector-search prior sessions (the `session-index` namespace), record a
   link in the derived `related_sessions` table (upsert; threshold-gated,
   deduped per agent), then add this turn's vector to the index. No generative
   call; idempotent (per-event vector id + upsert). Config-gated —
   `[daemon] related_sessions_index` (on by default as of 2026-07-08; a no-op
   without an embedder), registered only when an
   embedder is configured, hosted off the turn path via the daemon's subscriber
   registry (`Daemon.AddSubscriber`), woken by the event sink's flush
   (`NewSQLEventSink` `onFlush` → `Daemon.NotifySubscribers`). *Gate met:* unit
   tests (link/index/dedup/threshold, empty-result skip) + a daemon hosting test
   (register → start → wake → drain filtered by type); verified live — the
   indexer consumes `turn_end`s off-band, skips empty results, and stores/links
   vectors (a 0.43-similarity pair correctly did **not** link at threshold 0.75).
3. **Pull surfacing. ✅ Done (2026-07-08).** The context builder optionally
   surfaces a recorded `related_sessions` link on a later user turn, under
   existing relevance + budget. Wiring: `BuildInput.SystemEnrichment` is a new
   priority-2.6 section — capped at 300 tokens and dropped first when budget is
   tight, so enrichment can never crowd out the turn (`internal/context/builder.go`).
   The agent loop computes it once per turn via `Config.RelatedFn` (mirrors
   `SelfModelFn`; `internal/agent/loop.go`). `relatedEnrichmentFn`
   (`internal/runtime/related_surface.go`) reads the derived `related_sessions`
   links the indexer maintains and surfaces the one whose `session-index` vector
   is closest to *this* query (>= 0.6), rendered as a compact gist pulled from
   the linked session's latest `turn_end` (`memory.LatestTurnResult`). The
   surfacing threshold (0.6) is deliberately below the indexer's 0.75 *linking*
   threshold: linking compares two answers, but surfacing compares a *question*
   to a prior answer, which scores structurally lower — live measurement
   (nomic-embed-text) put an on-topic question at ~0.70 vs the matching session
   and an off-topic one at ~0.34, so 0.6 sits inside that gap. No
   generative call; the active session is never mutated — the value reaches the
   user only because their own question made a prior session relevant (pull, not
   push). Gated on `[daemon] related_sessions_index` (the same flag that runs the
   indexer) + a configured embedder, wired in `internal/runtime/builder.go` /
   `cmd/nine`. *Gate met:* the relevance filter is doubly conservative — a match
   must be *both* a recorded link *and* query-relevant, and an off-topic or
   gist-less link stays silent (unit tests: relevant-surface, not-relevant-silent,
   unlinked-match-silent, empty-gist-skip, budget-cap/drop). *Verified live,
   end-to-end* (2026-07-08) against a real daemon (gemma4:e4b) + real
   The vector store + nomic-embed-text: two topically-linked sessions linked at
   0.85–0.93, then on a later turn the enrichment landed in the model's actual
   assembled system prompt (confirmed by inspecting the journaled `llm_request`
   `system` payload) with the correct prior-session gist, while an off-topic
   follow-up ran the surfacer but cleared no link ≥ 0.6 and stayed silent. This
   run produced the ~0.70 / ~0.34 on/off-topic measurement above that sets the
   0.6 surfacing threshold. The turns were driven via the non-interactive
   `nine send [--id <id>] <msg>` CLI command (`internal/cli`).
4. **Fold the supervisor bus in. ✅ Done (2026-07-07).** The
   supervisor durably journals every control-plane event on `Post` (a
   synchronous append that commits before returning and survives restart)
   under the `supervisor` event type, and consumes them via a cursor-backed
   `subscribe.Subscription` (the supervisor implements `subscribe.Handler`),
   resuming from its durable cursor on boot (`internal/runtime/supervisor.go`;
   wired via `Supervisor.Attach(store)` in `cmd/nine`). *Gate met:* verified live — `agent_completes` events
   from the self-reflection loop land in the journal and the supervisor cursor
   advances as they are consumed; unit tests cover journal+consume and
   cursor-resume. Reactions themselves remain stubs (the point of this slice is
   the durable, ordered, resumable substrate; concrete reactions plug into
   `handle`).

## 9. Reference symbols

- `internal/memory/events.go` — `SessionEvent`, `SessionEventsByAgent` (the stream
  to subscribe to; the `seq` cursor).
- `internal/runtime/eventsink.go` — the async batched writer (the producer side).
- `internal/runtime/supervisor.go` — the journal-backed control-plane bus.
- `internal/context/builder.go` — `BuildWithUsage` (the pull seam for surfacing
  enrichment under relevance + budget).
- `internal/memory/vectors.go` — vector search (the retrieval substrate for
  semantic subscribers).
- `internal/memory/user_notifications.go` — the existing pull-based feed
  subscribers can post to.
