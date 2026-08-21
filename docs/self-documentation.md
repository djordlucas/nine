# Self-Documentation

Nine ships its own manual inside its binary and can retrieve it on demand. When
you ask how Nine works, it searches the `docs/` and `spec/` trees compiled into
the running binary and answers from the text rather than from impression.

---

## Why Retrieval Rather Than Context

The bundled corpus is about 730 KB across roughly 570 sections. Placing any
meaningful portion of it in every turn would consume the context budget for the
majority of turns that have nothing to do with Nine itself.

So the manual is a **retrieval surface, not context**. The standing cost is two
tool definitions. Documentation enters a turn only when the model asks for it,
and it asks for a section, not a document.

The index holds **no document text**. A vector's key is an address —
`skills.md#tools` — and the text is sliced back out of the embedded
filesystem when it is read. There is no second copy to drift, which means the
docs Nine cites are always the docs its own version ships (see
[versioning.md](versioning.md)).

---

## The Tools

| Tool | Effect |
|---|---|
| `doc_search` | Rank bundled sections against a natural-language query; returns addresses and snippets |
| `doc_read` | Return the exact text at an address — one section, or a whole topic |

```
doc_search({"query": "how does a turn reach a worker", "top_k": 5, "bundle": "docs"})
# → {"count": 5, "results": [{"addr": "daemon.md#request-routing",
#                             "title": "Daemon Architecture", "heading": "Request Routing",
#                             "snippet": "…", "score": 0.71}, …]}

doc_read({"ref": "daemon.md#request-routing"})
```

Both are **core-intercepted** tools (handled in-process against the embedded FS
and the memory store), not a plugin subprocess.

`doc_read` accepts any address a caller might plausibly hold: a `doc_search`
result (`skills.md#tools`), a topic name (`agent-loop`), a bundle path
(`spec/contracts/wire-protocol`), with or without `.md`. An unresolvable
reference returns an error that lists the whole catalog, so a wrong guess
recovers without a second call.

`doc_read` returns Markdown behind an address header rather than JSON. A whole
document can exceed the dispatcher's output cap, and truncated Markdown is still
readable where a truncated JSON object is unparseable
([tool-output-spill.md](tool-output.md)).

### Availability

**Neither tool requires an embedder**, which is what sets this pair apart from
`tool_search` and `skill_search` ([tool-exposition.md](tool-selection.md)).
Those rank catalogs that exist only in the store, so without an embedder they
have nothing to rank and the daemon omits them. The documentation is compiled
into the binary, so it can be ranked lexically with no outside help: a
deployment running `provider = "none"` still gets a searchable manual, and
simply gains the vector half when an embedder is configured.

For the same reason, a failing embedder or an unreachable index does not fail a
search — it degrades to whichever retriever survived. The caller asked a
question about the documentation, not about the embedder.

Both tools are granted regardless of a role's allowlist, on the same footing as
`gap_report`: a narrowed role is the one most likely to be uncertain about what
Nine can do, and reading the manual is read-only. See [roles.md](roles.md).

---

## Addressing

An address is `<bundle>/<path>#<anchor>`, where the anchor is a slug of the
section heading. Addresses are stable, human-meaningful, and resolve on the CLI:
a section Nine cites as `configuration.md#daemon` is one you can open with
`nine docs configuration`.

Documents are chunked at their `##` headings, the unit these docs are written
in. Two refinements matter:

- **Headings inside fenced code blocks are ignored.** The docs quote Markdown
  samples — [skills.md](skills.md) embeds an entire example skill file, headings
  and all — and splitting on those would produce sections addressing text that
  is only an example.
- **An oversized section is split again at its `###` headings.** One vector
  averaged over several unrelated subtopics ranks poorly for all of them.

Text before the first `##` (the title and any preamble) is a section in its own
right, anchored by the document title. It needs an anchor because an
*unanchored* address means the whole document — without one, a hit on an
introduction would read back as the entire file.

---

## Indexing

The index is built at daemon boot by `runtime.SeedDocs`, alongside the skill
seeders ([skills.md](skills.md)), into the `docs` vector namespace.

It is **fingerprinted**: a SHA-256 over every section address and body plus the
embedder's provider/model. When the fingerprint matches what is stored, boot
costs one key-value read. A rebuild happens only when the binary's docs change
or the embedder does — the latter matters because vectors produced by one
embedder are meaningless to another, so a provider switch must reindex even
though the documents are identical.

A rebuild replaces the namespace wholesale rather than reconciling entry by
entry. The namespace is derived state, and a renamed or merged section leaves
behind an address that no longer resolves — a stale hit is worse than a missing
one, because it sends the agent to read something that is not there. (`doc_search`
also skips unresolvable addresses at query time, so drift degrades to a shorter
result list rather than a dangling reference.)

A pass that fails to embed some sections does **not** record its fingerprint, so
the next boot retries. A half-indexed manual that Nine believed was whole would
silently answer from whichever sections happened to make it in.

---

## Ranking: Two Retrievers, Fused

`doc_search` is a **hybrid**. It runs two independent retrievers and fuses their
rankings:

1. **Vector** — cosine similarity over the `docs` namespace, which catches
   phrasing the documents do not use literally.
2. **Lexical** — BM25 over the same sections, built in-process from the embedded
   corpus (`docindex.Lexical`). No database, no embedder, no boot cost: it is
   constructed on first search and cached for the life of the process.

The lexical half exists to supply **inverse document frequency**, which the
default embedder ([`keyword`](configuration.md#embeddings)) does not have. That
embedder is raw term frequency, so every term counts the same and long
vocabulary-dense sections win everything: a compatibility matrix mentions every
feature once and therefore matches every feature query, outranking the short
section on the actual subject. IDF is exactly the missing signal — it discounts
terms that appear everywhere (`agent`, `session`, `tool`) and rewards the ones
that single a document out.

Queries and documents are also lightly **stemmed**, so "how do I *configure* the
database" reaches a document titled "*Configuration*".

### Why fuse ranks rather than scores

A cosine similarity and a BM25 score share no scale — BM25 is unbounded and
moves with query length — so any weighted sum would need a normalisation that is
itself arbitrary. Reciprocal rank fusion combines *positions* instead:
`score(d) = Σ 1/(k + rank(d))`. Ranks are directly comparable, and a document
missing from one retriever's list simply contributes nothing from it, which is
the right behaviour when the two disagree about what is even a candidate.

Because fusion can only rescue what it can see, each retriever contributes a
pool far wider than the result count (`docCandidateFactor`).

### How good it actually is

Measured on the shipped corpus with the **default** embedder, over 30 labelled
questions (`TestDocSearchRetrievalQuality`):

| | top 3 | top 5 |
|---|---|---|
| Cosine only | 22/30 | 23/30 |
| Lexical only (no embedder) | 21/30 | 25/30 |
| **Hybrid (live)** | **24/30** | **27/30** |

Worth noting the middle row: a daemon with **no embedder at all** now retrieves
better than the original vector-only implementation did with one. The two
retrievers are close in strength and fail on different queries, which is exactly
the condition under which fusing them pays.

Top 5 is the number that matters, since that is what `doc_search` returns by
default: it decides whether the answer is in front of the model at all — 90% of
the time it is.

That test is a regression floor, and it keeps the still-failing cases in the
suite on purpose: a suite pruned to what already passes cannot demonstrate an
improvement.

Two things follow for anyone relying on this:

- Configuring a real embedding model (`provider = "ollama"`) improves the vector
  half further. The index rebuilds automatically on the switch.
- A miss is usually a near miss — a related section from a related document — so
  the model can re-query or `doc_read` a topic by name. This is not designed to
  be right first try every time; it is designed to make the manual reachable.

---

## Using It Well

The [`self-documentation` skill](../skills/self-documentation.md) carries the
policy: search before answering questions about Nine, read the section rather
than the document, quote rather than paraphrase when the answer turns on exact
behavior, and cite the address. It surfaces semantically like any other skill,
so it costs nothing on turns that are not about Nine.

```bash
./nine "What does context_budget actually control?"
./nine "What is the difference between a goal and a workflow?"
./nine "What guarantees does the wire protocol make about message ordering?"
```

**When the docs and the code disagree**, that is a bug to reconcile, not a doc
to quietly follow — the documentation and specification hold the intended
behavior. Nine is instructed to surface the mismatch rather than smooth it over.

### Delegation

Sub-agents inherit both tools, so a broad question spanning many documents can
be delegated with `run_agent` to keep the intermediate reading out of the parent
conversation. This is worth it only for genuine synthesis across documents: for
a specific lookup, spawning an agent costs more than the section it would read,
and it returns a paraphrase where the value was in the quote.

---

## Keeping It Accurate

The index is only as good as the documents, and the documents are kept in
lockstep with the code by `/sync-nine`. A behavior change that skips the
embedded docs does not merely leave the docs stale — it teaches Nine to state
the stale behavior as fact, with a citation.
