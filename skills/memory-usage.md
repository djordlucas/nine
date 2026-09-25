---
name: memory-usage
description: The memory store — the self/ namespace that holds your self-model, key-value storage, and semantic search
tags: [memory, kv, persistence, embeddings]
---

## Memory Usage

The memory plugin provides persistent key-value storage and semantic (vector) search. Use it to remember facts, user preferences, and intermediate results across conversations.

### Key-value storage

```
memory_set({"key": "user_language", "value": "Python"})
memory_get({"key": "user_language"})   # → "Python"
memory_delete({"key": "user_language"})
memory_list({})                         # → all keys
```

Keys are strings; values are strings (JSON-encode structured data if needed).

### The `self/` namespace is your self-model

Four `self/` keys are read on every turn and placed in your context — this is
how you know who you are:

| Key | Holds |
|---|---|
| `self/identity` | What you are. Seeded on first boot. |
| `self/persona` | Who you are, on a packaged instance. Absent on a stock deployment. |
| `self/capabilities` | A concise description of what you can currently do. |
| `self/learned` | Dated entries of what recent activity taught you. |

Read and write them by their **exact, whole key**. The assembler looks up these
four names; a key like `self/identity/name` is read by nothing and reaching a
turn is exactly what it will not do.

```
memory_get({"key": "self/learned"})
memory_set({"key": "self/learned", "value": "<existing text>\n2026-09-25: ..."})
```

`memory_set` on `self/learned` replaces the value, so read it first and append
to what is there. Keep entries short and dated.

The whole `self/` prefix is protected: `memory_delete` refuses it and tells you
to use `memory_set` instead. Other `self/` keys exist for Nine's own
bookkeeping (`self/workspace/last_scan`, `self/docs-index-fingerprint`) — read
them if useful, but they are not yours to curate.

### When to use memory

- **User preferences**: language, style, timezone, project context
- **Session state**: current task ID, last processed item, a counter
- **Lookup tables**: frequently needed constants that aren't worth hardcoding
- **Cross-turn continuity**: remember what was done in previous conversations

### Semantic search (vector store)

When an embedder is configured, everything you write with `memory_set` is also
indexed into the `memories` vector namespace, so a value you stored by key is
findable by meaning:

```
memory_set({"key": "tip:testing", "value": "Run make test for the full suite"})

memory_query({"namespace": "memories", "query": "how do I run tests?", "top_k": 3})
# → the most semantically similar stored items
```

`namespace` is required on `memory_query`: a vector is only found by a query
against the namespace it was stored in. Use `memories` to search what
`memory_set` wrote.

`memory_embed` indexes text under a namespace of your choosing, without storing
a key-value pair:

```
memory_embed({"id": "note-1", "namespace": "notes",
              "key": "release-process", "text": "Tag on main after the PR merges"})
memory_query({"namespace": "notes", "query": "when do I tag?"})
```

### Naming conventions

Use namespaced keys to avoid collisions:
- `pref:<setting>` — user preferences
- `ctx:<topic>` — context about a topic
- `tip:<subject>` — reusable tips and notes
- `self/…` — reserved for the self-model above; do not invent new keys under it

### Memory vs. skills

| | Memory | Skills |
|--|--------|--------|
| Format | Key-value strings | Markdown documents |
| Retrieval | Exact key or semantic | Semantic similarity |
| Best for | Dynamic values, state | Stable procedures, guidelines |

## Limits

| Limit | Detail |
|---|---|
| Values are strings | JSON-encode anything structured; nothing parses it for you. |
| `self/` is read whole | The self-model assembler looks up four exact key names. A sub-path under one of them reaches no turn. |
| `memory_set` replaces | There is no append. Read the current value first when adding to `self/learned`. |
| Semantic search needs an embedder | `memory_query` and `memory_embed` do nothing useful when no embedding provider is configured. |
| `self/` cannot be deleted | `memory_delete` refuses the protected prefix; overwrite with `memory_set` instead. |
