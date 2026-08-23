# Contract — Memory Store (the single database gateway)

**Status:** Built · **Depends on:** nothing · **Used by:** everything that persists

One store object owns the only database handle. All persistence — agent memory,
checkpoints, goals, workflows, session plans, vectors, skills, and the session event
journal — flows through it (invariant I3). The reference backend is **SQLite**, reached
via the pure-Go `modernc.org/sqlite` driver over `database/sql`; nine therefore ships
with no database server and nothing to provision. The store opens a file path
(`[memory].path`), creating the file and its parent directory if absent, **fails fast**
if the database is unusable (it holds primary state, so an unopenable database is a
startup error, not a degraded mode), and applies its schema idempotently on `Open`
(`CREATE TABLE IF NOT EXISTS`), followed by any outstanding schema migrations
(**R-MEM.10**).

Because SQLite serializes writes, the store owns **two connection pools**: a
single-connection read-write pool and a concurrent read-only pool, with each statement
routed by its leading keyword. Routing is by statement text rather than by Go method
because `UPDATE … RETURNING` arrives through `Query` but is a write; anything not
recognizably a `SELECT` routes to the writer. The database runs in WAL mode with a busy
timeout, so a second process (`nine trace`, `nine replay`) can read a live database
without blocking, or being blocked by, the daemon's writer.

> **Timestamps are fixed-width RFC3339 UTC microseconds stored as TEXT.** SQLite compares
> TEXT bytewise, so ordering and range predicates *are* string comparisons: a
> variable-width or mixed-offset format silently corrupts both. A conforming
> implementation **MUST** write every timestamp in one fixed-width, always-UTC form, and
> **MUST NOT** bind a native time value as a query argument unless the driver is known to
> format it identically.

---

## R-MEM.1 — Single gateway

A conforming implementation **MUST** route every database access through one store type
that holds the sole connection/handle. Domain services (e.g. the workflow service)
**MUST** depend on a narrow repository interface, not on the database directly. This is
what keeps the "single database gateway" invariant and makes domain logic testable against
a fake repository.

---

## R-MEM.2 — Schema (exactly these tables)

The reference database contains these **twenty** tables. An implementation **MUST**
provide equivalent storage for each; it **MUST NOT** require additional operational
tables to be agent-visible (R-MEM.4).

| Table | Purpose | Access tier |
|-------|---------|-------------|
| `kv` | agent key-value memory | agent (tools) |
| `files` | file content, full-text indexed by a companion FTS5 table kept in sync by triggers; the `spill/` prefix is daemon-owned (R-MEM.9) | agent (tools) |
| `vectors` | embeddings as packed float32 blobs, ranked by cosine similarity, namespaced (`skills`, `session-index`, `memories`, `docs`, agent namespaces) | mixed (see below) |
| `skills` | skill records (name, description, tags, body, source) | mixed |
| `tools` | generated sandboxed tools Nine authored (name, description, input_schema, `js` source, capability **declaration**, usage counters) — code and declaration only, never a grant (see [`toolvm.md`](toolvm.md) R-TVM.14) | daemon-private |
| `conversations` | message history, scratchpad checkpoint, status, display name | daemon-private |
| `goals` | open-ended intentions; status; parent (`parent_id` is the only edge) | daemon-private |
| `workflows` | multi-step plans; steps as a JSON array on the row | daemon-private |
| `notifications` | pending push messages to the next active turn | daemon-private |
| `user_notifications` | human-facing feed posted by background agents (`nine notifications`) | daemon-private |
| `session_plans` | per-session stage state + idle config | daemon-private |
| `human_requests` | HITL question/answer state (see [`hitl.md`](hitl.md)) | daemon-private |
| `interactive_sessions` | which sessions are HITL-eligible | daemon-private |
| `session_events` | append-only execution journal (see [`event-journal.md`](event-journal.md)) | daemon-private |
| `event_cursors` | per-subscriber durable journal position | daemon-private |
| `related_sessions` | derived cross-session links (see [`subscriptions.md`](subscriptions.md)) | daemon-private |
| `jobs` | long-running work tracked across turns and restarts, for both backends — a plugin's detached goroutine and a resumable sandboxed tool (see [`plugin.md`](plugin.md), [`toolvm.md`](toolvm.md) R-TVM.19). Was `plugin_jobs`; `backend` says which, and `cursor`/`calls` belong to the tool backend | daemon-private |
| `standing_tools` | resumable tools the daemon runs indefinitely on their own cadence (see [`toolvm.md`](toolvm.md) R-TVM.20). Separate from `jobs`: a job is conversation-owned and terminates, a standing run is operator-owned, reconciled by a stable id, and has no terminal state | daemon-private |
| `tool_state` | a sandboxed tool's durable state, keyed `(tool, scope_key, key)` — the store behind the `state` capability (see [`toolvm.md`](toolvm.md) R-TVM.18). Deliberately separate from `kv`, which is Nine's own namespace and must not become tool-writable | daemon-private |

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
| `file_fetch` (windowed) | `FileFetchRange(path, offset, limit)` → `FileSlice{content, offset, chars, total}` | reads a window of a large file; offsets are **characters**, sliced in the database so the file is never materialized whole |
| `file_search_text` | `FileSearchTextScoped(query, pathPrefix, limit)` | FTS5 full-text search ranked by `bm25`, highlighted via `snippet`; an optional path prefix scopes the search to one file or directory |
| `memory_embed` / `memory_query` | `VectorStore`, `VectorQuery(ns, vec, topK)` | **core-intercepted** |
| `file_search_semantic` | `VectorQuery` over file chunks | **core-intercepted** |
| `skill_*` | `SkillUpsert/Get/List/Delete`, `SkillNamesBySource` | see [`skills.md`](skills.md) |

**Search input is untrusted.** `file_search_text` receives whatever the model composed,
which routinely includes unbalanced quotes, stray operators, trailing conjunctions and
column-filter syntax. A conforming implementation **MUST NOT** surface a query-syntax
error to the agent: malformed input yields *no results*, never a failed tool call. The
reference does this by emitting every user term as a quoted literal, so no input can
reach the matcher as syntax; an input with nothing searchable in it short-circuits to an
empty result.

---

## R-MEM.4 — Daemon-private methods (never tools) — invariant I4

The following are reachable only by the daemon/runtime, never advertised as agent tools.
An agent **MUST NOT** be able to mutate its own conversation row, the goal/workflow
tables, notifications, or session plans through a tool call.

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
| Spill retention | `FileDeleteOlderThan(pathPrefix, age)` — the sweep for spilled tool output. **MUST** reject an empty prefix and a non-positive age, so it can never clear the store (see [`../../adr/tool-output-spill.md`](../../adr/tool-output-spill.md) §6) |

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
store into and query the `embedding` blob column, ranking by cosine similarity and
filtered to matching dimensionality. The reference implementation computes similarity in
process over a namespace scan; this is not a shortcut but the same work the previous
backend did, which likewise had no approximate-nearest-neighbour index. Namespaces partition
the space so queries don't collide: `skills` (skill descriptions, written on skill
create/modify), `session-index` (one vector per completed turn, written by the
related-session subscriber), `memories` (one vector per KV key — the embedded value —
mirrored on every `memory_set` when memory surfacing is enabled, so the context builder
can pull-surface memories relevant to the current turn; see R-MEM.8), `docs` (one vector
per section of the embedded documentation; see
[`self-documentation.md`](self-documentation.md)), and per-agent
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

`internal/memory/` — `db.go` (open + schema + pool routing), `time.go` (the stored
timestamp format), `kv.go`, `files.go` (FTS5 search), `fts.go` (the MATCH-expression
builder), `vectors.go` (blob encoding + cosine ranking), `skills.go`, `conversations.go`,
`goals.go`, `workflows.go` (delegates to `internal/workflow.Service`), `notifications.go`,
`user_notifications.go`, `session_plans.go`, `hitl.go`, `events.go`
(journal), `cursors.go` (subscriber cursors), `related.go` (related sessions),
`jobs.go` (the job registry, both backends). Driver: `modernc.org/sqlite` (pure Go, no cgo).
Backend: one SQLite file, on the container's `/data` volume or at `~/.nine/nine.db`
natively.

---

## R-MEM.11 — Session deletion

A session may be **erased**: the `conversations` row and every row keyed to it —
`session_events`, `notifications`, `user_notifications`, `session_plans`,
`related_sessions`, `human_requests`, `interactive_sessions`, the session's
`tool_state` scope, its `jobs`, and its display-name key in `kv`.

The cascade **MUST** be one transaction. A partial cascade leaves journal rows
pointing at a conversation that no longer exists, which is worse than either
completing or not starting.

It **MUST** report what it removed, per table. This is the only operation in the
store that destroys history rather than bounding it (contrast `SessionEventsScrub`,
R-EVT.4), and "deleted session X" is not an auditable statement.

**Tool state is removed for the conversation scope only** — rows whose
`scope_key` is the session id. Tool-scoped state (`scope_key = ''`) is shared
across every caller of a tool and is nobody's session to delete
([`toolvm.md`](toolvm.md) R-TVM.18).

### Retention

Sessions older than a retention window **MAY** be deleted automatically. Age
**MUST** be measured from last activity (`updated_at`), not creation.

Two kinds of session **MUST NOT** be selected, whatever their age:

- one whose id matches an **active goal** — a pursue session's id *is* its goal
  id, so this is an exact test rather than a heuristic;
- one carrying an **active session plan**, which covers standing agents.

Both are idle by design. A standing agent that wakes weekly looks abandoned after
ten days precisely because it is working correctly, and reaping either would
silently dismantle configured behaviour.

Retention has three distinguishable states — unset (the default applies), a
number, and `0` (disabled) — so the configuration **MUST NOT** collapse the first
and last.

---

## R-MEM.10 — Schema versioning and migration

`PRAGMA user_version` records which schema generation a database is at, and `Open`
brings it up to date before returning. `CREATE TABLE IF NOT EXISTS` covers new tables
and indexes; everything it cannot express — a column added to an existing table, a
backfill, an FTS rebuild after a tokenizer change — is a numbered **migration step**.

Steps form an append-only ordered list where entry *i* migrates a database from version
*i* to *i+1*, so the current schema version **MUST** be the number of steps rather than a
separately-declared constant: two sources for one fact desynchronize silently, and the
symptom is a step that never runs. Renumbering or reordering an existing entry is
forbidden — databases in the field have already recorded which steps ran.

A conforming implementation **MUST**:

- **Apply each step and its version bump atomically.** They commit or roll back
  together, so an interrupted migration leaves the database at the last version that
  *fully* applied — never part-way through a step — and re-opening resumes correctly.
- **Skip every step for a freshly created database**, stamping it at the current
  version directly. The idempotent schema above already builds the current shape, and a
  step written against an older one (a rename, a rewrite) would fail against it. A step
  therefore need only be correct for databases at its own "from" version.
- **Refuse a database newer than the binary understands**, rather than operating on a
  shape it does not know. Downgrading is not supported; the error **MUST** say so and
  **MUST NOT** rewrite the version.
- **Not offer down-steps.** A reversal cannot restore data a destructive step dropped,
  so the honest recovery is to restore the file and apply a corrected forward step.

`OpenReadOnly` skips this entirely (as it skips schema creation), so a stale CLI process
can never run DDL against a running daemon's database.
