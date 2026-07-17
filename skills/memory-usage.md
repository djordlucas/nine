---
name: memory-usage
description: How to use the memory store for persistent key-value data and semantic search
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

### When to use memory

- **User preferences**: language, style, timezone, project context
- **Session state**: current task ID, last processed item, a counter
- **Lookup tables**: frequently needed constants that aren't worth hardcoding
- **Cross-turn continuity**: remember what was done in previous conversations

### Semantic search (vector store)

When embeddings are configured, memory also supports similarity search:

```
memory_query({"query": "how do I run tests?", "top_k": 3})
# → most relevant stored values
```

Store things you want to retrieve by meaning:
```
memory_set({"key": "tip:testing", "value": "Run go test -mod=vendor ./... for all tests"})
```

### Naming conventions

Use namespaced keys to avoid collisions:
- `pref:<setting>` — user preferences
- `task:<id>` — task-related state
- `ctx:<topic>` — context about a topic
- `tip:<subject>` — reusable tips and notes

### Memory vs. skills

| | Memory | Skills |
|--|--------|--------|
| Format | Key-value strings | Markdown documents |
| Retrieval | Exact key or semantic | Semantic similarity |
| Best for | Dynamic values, state | Stable procedures, guidelines |
