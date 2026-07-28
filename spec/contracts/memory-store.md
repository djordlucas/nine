# Contract — Memory Store (the single Postgres gateway)

**Status:** Built · **Depends on:** nothing · **Used by:** everything that persists

One store object owns the only database handle. All persistence — agent memory,
checkpoints, goals, workflows, session plans, vectors, skills, and the session event
journal — flows through it (invariant I3). The reference backend is **PostgreSQL** with
the **`pgvector`** extension, reached via the pgx v5 driver over `database/sql`. The
store connects by DSN (`[memory].database_url`), **fails fast** if the database is
unreachable (Postgres holds primary state, so an unavailable database is a startup error,
not a degraded mode), and applies its schema idempotently on `Open`
(`CREATE TABLE IF NOT EXISTS`, `CREATE EXTENSION IF NOT EXISTS vector`) — there is **no
migration table or version counter**. A thin wrapper rewrites `?` placeholders to `$N`
so query strings stay driver-agnostic.

---

## R-MEM.1 — Single gateway

A conforming implementation **MUST** route every database access through one store type
that holds the sole connection/handle. Domain services (e.g. the workflow service)
**MUST** depend on a narrow repository interface, not on the database directly. This is
what keeps the "single Postgres gateway" invariant and makes domain logic testable against
a fake repository.

---

## R-MEM.2 — Schema (exactly these tables)

The reference database contains these **sixteen** tables (plus the `vector` extension).
An implementation **MUST** provide equivalent storage for each; it **MUST NOT** require
additional operational tables to be agent-visible (R-MEM.4).

| Table | Purpose | Access tier |
|-------|---------|-------------|
| `kv` | agent key-value memory | agent (tools) |
| `files` | file content + generated `tsvector`/GIN full-text index; the `spill/` prefix is daemon-owned (R-MEM.9) | agent (tools) |
| `vectors` | pgvector embeddings + `<=>` cosine query, namespaced (`skills`, `session-index`, agent namespaces) | mixed (see below) |
| `skills` | skill records (name, description, tags, body, source) | mixed |
| `conversations` | message history, scratchpad checkpoint, status, display name | daemon-private |
| `goals` | open-ended intentions; status; parent; `subtree` JSON | daemon-private |
| `workflows` | multi-step plans; steps as a JSON array on the row | daemon-private |
| `notifications` | pending push messages to the next active turn | daemon-private |
| `user_notifications` | human-facing feed posted by background agents (`nine notifications`) | daemon-private |
| `reflections` | idle-reflection summaries (one row per reflection) | daemon-private |
| `session_plans` | per-session stage state + idle config | daemon-private |
| `human_requests` | HITL question/answer state (see [`hitl.md`](hitl.md)) | daemon-private |
| `interactive_sessions` | which sessions are HITL-eligible | daemon-private |
| `session_events` | append-only execution journal (see [`event-journal.md`](event-journal.md)) | daemon-private |
| `event_cursors` | per-subscriber durable journal position | daemon-private |
| `related_sessions` | derived cross-session links (see [`subscriptions.md`](subscriptions.md)) | daemon-private |

There is **no `plugin_registry` table** (plugins are immutable image content) and **no
`tasks` table** (finite work is a sub-agent or a workflow step).

---

## R-MEM.3 — Agent-facing methods (exposed as tools)

These back the agent-visible tools. (The embedding-backed ones are *core-intercepted* —
the dispatcher calls the embedder and then the store; see
[`dispatcher.md`](dispatcher.md) and [`embedder.md`](embedder.md).)

| Tool(s) | Store methods | Notes |
|---------|---------------|-------|
| `memory_get/set/delete/list` | `Get`, `Set`, `Delete`, `List(prefix)` | exact-key K/V; `List` is prefix-scan |
| `file_store/fetch/list` | `FileStore`, `FileFetch`, `FileList(prefix)` | arbitrary content storage; `file_store` refuses the reserved `spill/` prefix |
| `file_fetch` (windowed) | `FileFetchRange(path, offset, limit)` → `FileSlice{content, offset, chars, total}` | reads a window of a large file; offsets are **characters**, sliced in Postgres so the file is never materialized whole |
| `file_search_text` | `FileSearchTextScoped(query, pathPrefix, limit)` | Postgres full-text search (`websearch_to_tsquery` + `ts_rank`, highlighted via `ts_headline`); an optional path prefix scopes the search to one file or directory |
| `memory_embed` / `memory_query` | `VectorStore`, `VectorQuery(ns, vec, topK)` | **core-intercepted** |
| `file_search_semantic` | `VectorQuery` over file chunks | **core-intercepted** |
| `skill_*` | `SkillUpsert/Get/List/Delete`, `SkillNamesBySource` | see [`skills.md`](skills.md) |

---

## R-MEM.4 — Daemon-private methods (never tools) — invariant I4

The following are reachable only by the daemon/runtime, never advertised as agent tools.
An agent **MUST NOT** be able to mutate its own conversation row, the goal/workflow
tables, notifications, session plans, or reflections through a tool call.

| Group | Methods (reference) |
|-------|---------------------|
| Conversations / checkpoints | `ConversationCreate/Get/SetStatus`, `ConversationSave/Load` (checkpoint blob), `ConversationUpdateHistory/Scratchpad`, `ConversationName{Save,Load}` |
| Goals | `GoalCreate/Get/List/UpdateStatus/AppendSubtree` (the agent reaches these only via the core-intercepted `goal_*` tools, which the daemon mediates) |
| Workflows | `WorkflowCreate/Get/Update/List/Scrub/Fail/Cancel/ResetStep` (mediated via core-intercepted `workflow_*` tools and operator commands) |
| Notifications | `NotificationCreate/ListPending/MarkDelivered`; human feed `UserNotificationCreate/List/MarkSeen` |
| Reflections | `ReflectionCreate/List` |
| Session plans | `SessionPlanGet/Save/ListActive` |
| HITL | `human_requests` / `interactive_sessions` state (see [`hitl.md`](hitl.md)) |
| Event journal | `SessionEventsAppend`, `SessionEventsByAgent`, `SessionEventsAfter`, `SessionEventsScrub`, `LatestTurnResult` (see [`event-journal.md`](event-journal.md)) |
| Subscriptions | `EventCursorGet/Set`, `RelatedSessionAdd`, `RelatedSessions` (see [`subscriptions.md`](subscriptions.md)) |
| Spill retention | `FileDeleteOlderThan(pathPrefix, age)` — the sweep for spilled tool output. **MUST** reject an empty prefix and a non-positive age, so it can never clear the store (see [`../../docs/tool-output-spill.md`](../../docs/tool-output-spill.md) §6) |

> The `goal_*` and `workflow_*` tools *appear* in the agent's tool list, but they are
> **core-intercepted**: the dispatcher validates and routes them to these daemon-private
> methods. The agent never gets a raw handle to the tables. This is the precise meaning
> of I4.

---

## R-MEM.5 — Checkpoints

The checkpoint unit is `ConversationState{History, Scratchpad}`, serialized to JSON.

```text
end of every turn:   SaveState() → JSON{history, scratchpad} → ConversationSave(id, data)
attach / resume:     ConversationLoad(id) → LoadState(data) → worker rebuilt
```

A session is fully reconstructable from its checkpoint blob plus its `session_plans` row
(invariant I5). `ConversationLoad` returns `(data, found, err)` so a missing checkpoint
is distinguishable from an error.

---

## R-MEM.6 — Vectors and namespaces

`VectorStore(id, namespace, key, vector)` and `VectorQuery(namespace, vector, topK)`
store into and query the pgvector `vector` column, ranking by `<=>` cosine distance
(similarity = `1 - distance`), filtered to matching dimensionality. Namespaces partition
the space so queries don't collide: `skills` (skill descriptions, written on skill
create/modify), `session-index` (one vector per completed turn, written by the
related-session subscriber), `memories` (one vector per KV key — the embedded value —
mirrored on every `memory_set` when memory surfacing is enabled, so the context builder
can pull-surface memories relevant to the current turn; see R-MEM.8), and per-agent
memory namespaces (from `memory_embed`). `VectorDelete(id)` removes a stored vector
(used when a skill is deleted/replaced, or when `memory_delete` removes a mirrored KV key).

> Note: tool-relevance ranking is a separate, in-memory computation over
> `ToolWithVector` embeddings in the context builder — the `AgentBuilder` embeds each tool
> description and caches it in memory. Those per-tool vectors are **not** stored in the
> `vectors` table; there is no `tools:` namespace.

---

## R-MEM.7 — JSON convenience variants

Several reads have `…JSON` / `…String` variants (`KVListString`, `FileListString`,
`FileSearchTextJSON`, `SkillListJSON`, `GoalList` → JSON at the tool boundary) that
return a ready-to-emit payload. These are an optimization, not a requirement; an
implementation **MAY** serialize at the call site instead.

---

## R-MEM.8 — Memory surfacing (KV pull-surfacing)

When `[memory].surface_memories` is enabled (**default on**; a no-op without an embedder),
key-value memory becomes semantically retrievable without the agent asking:

- **Index on write.** Each `memory_set` also embeds the value and mirrors it into the
  `memories` namespace (id `memories:<key>`, vector key `<key>`). `memory_delete` removes
  the mirror. Indexing is **best-effort** — an embed/store failure never fails the KV write
  (R-EMB.5). This is a side effect of the agent-facing tools, not a new tool.
- **Pull-surface on read.** Once per turn the context builder embeds the current query,
  ranks the `memories` pool, and injects up to a small N (reference: 3) memories that clear
  a similarity floor (reference: 0.6) as **advisory enrichment** — the same priority-2.6,
  small-capped, drop-when-tight band the related-session surfacer uses, so it never crowds
  out the turn. Surfaced values are re-read from the KV store (not the vector row) so they
  reflect the latest `memory_set`, and are framed non-authoritatively ("draw on them only
  if they help; don't assume they are still current").

The pool is a **single shared namespace**, not partitioned per agent: any session can
surface any recorded memory. The runtime composes this surfacer with the related-session
surfacer into the loop's single enrichment channel.

---

## R-MEM.9 — The `spill/` namespace is daemon-owned

Over-cap tool output is written to `files` under `spill/<agent-id>/`
([`dispatcher.md`](dispatcher.md) R-DISP.2). That prefix carries two invariants, both of
which keep untrusted tool output from being laundered into trusted context:

- **Not agent-writable.** `file_store` **MUST** refuse a path under `spill/`, so a
  spilled file is always exactly what a tool returned — never something the model
  composed there and later cited as a tool result. It stays agent-*readable*: reading it
  back is the point.
- **Never embedded.** Nothing under `spill/` enters the vector pool (R-MEM.6/8), so
  pull-surfacing can never inject an untrusted blob into a later turn. It reaches the
  model only when the model explicitly reads it.

Spills are session debris and **MUST** be swept on a retention window
(reference: 7 days, hourly, via `FileDeleteOlderThan`), or the table grows without bound.
Retention is production-only bootstrap; a per-case harness that discards its store omits
it by design.

---

## Reference symbols

`internal/memory/` — `db.go` (open + schema; pgx v5; `?`→`$N` rebind), `kv.go`,
`files.go` (tsvector FTS), `vectors.go` (pgvector), `skills.go`, `conversations.go`,
`goals.go`, `workflows.go` (delegates to `internal/workflow.Service`), `notifications.go`,
`user_notifications.go`, `reflections.go`, `session_plans.go`, `hitl.go`, `events.go`
(journal), `cursors.go` (subscriber cursors), `related.go` (related sessions). Driver:
`github.com/jackc/pgx/v5/stdlib`. Backend: `docker-compose` `pgvector/pgvector:pg17`.
