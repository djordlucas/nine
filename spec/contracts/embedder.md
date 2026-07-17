# Contract — Embedder

**Status:** Built · **Depends on:** config, memory store (for vector put/query) · **Used by:** context builder, skills, semantic memory/file search, self-model

Embeddings power **tool relevance ranking**, **skill search**, **semantic memory/file
search**, and feed the **self-model** assembler. The embedder is configured independently
of the chat LLM so a deployment can run a local keyword embedder with a cloud chat model
or vice versa.

---

## R-EMB.1 — Interface

```interface
Embedder {
  Embed(ctx, text string) ([]float32, error)
}
```

That is the whole contract. Everything else (cosine similarity, top-K) lives in the
store's vector methods (see [`memory-store.md`](memory-store.md) R-MEM.6).

---

## R-EMB.2 — Providers

Built from `[embeddings]` config:

| Provider | Network? | Notes |
|----------|----------|-------|
| `keyword` | no | **Default.** Deterministic, dependency-free bag-of-words/hashing vector. Good enough for ranking; needs no host process. |
| `ollama` | yes (host) | local embedding model via Ollama at `endpoint` |
| `none` | no | disables ranking — `Embed` yields no usable vector and all tools/skills are included unranked |

There is no OpenAI embedder: `internal/embed/openai/` is an empty placeholder, and
`Build` falls through to `keyword` for any unrecognized provider value.

A conforming implementation **MUST** provide `keyword` as a no-network default and
**MUST** support `none` as an explicit "rank nothing" mode.

---

## R-EMB.3 — Two embedding code paths

There are exactly two ways embeddings get produced, and they **MUST** be kept distinct:

1. **Daemon-orchestrated (no tool call).** At skill create/modify the skill handlers embed
   the description into the `skills` namespace; the related-session subscriber embeds each
   completed turn into `session-index`; the self-model assembler embeds query text for
   relevance; and the `AgentBuilder` embeds each tool's description for context-builder
   relevance ranking (cached by tool name, held **in memory** — not written to the
   `vectors` table, so there is no `tools:` namespace). These call the embedder directly.
2. **Agent-invoked (core-intercepted tools).** `memory_embed`, `memory_query`, and
   `file_search_semantic` are tools the agent calls; the **dispatcher** intercepts them,
   calls the embedder, and routes to the store. The agent never holds the embedder.

No embedding logic lives inside a plugin subprocess.

---

## R-EMB.4 — Namespaces

All vectors are stored namespaced to avoid cross-domain collisions during similarity
search: `skills` for skill descriptions, `session-index` for per-turn session vectors
(the related-session subscriber), and per-agent namespaces for `memory_embed`. Semantic
file search uses its own keying within the store. See R-MEM.6.

---

## R-EMB.5 — Determinism & degradation

- `keyword` embedding **MUST** be deterministic for identical input (so re-indexing is
  stable).
- If the embedder errors or returns an empty vector for a candidate, that candidate's
  similarity score is treated as `0` rather than failing the turn. Ranking degrades
  gracefully toward "include the always-include set, then arbitrary others up to budget."

---

## Reference symbols

`internal/embed/embedder.go` (interface + `Build`), `internal/embed/{keyword,ollama}/`
(`internal/embed/openai/` is an empty placeholder).
