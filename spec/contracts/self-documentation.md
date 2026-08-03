# Contract — Self-Documentation

**Status:** Built · **Depends on:** embedder, memory store (vector put/query), embedded `docs/` + `spec/` bundles · **Used by:** agent loop (tool dispatch), CLI (`nine docs` / `nine spec`)

Nine ships its own manual inside its binary and retrieves it on demand rather than
carrying it in context. This contract fixes how a bundled document is addressed, how the
index is built and invalidated, and what the two retrieval tools guarantee.

See [`../../docs/self-documentation.md`](../../docs/self-documentation.md) for the
narrative treatment.

---

## R-DOC.1 — Addressing

A bundled section is addressed as `<bundle>/<path>#<anchor>`:

- `bundle` is `docs` or `spec`; `path` is the document's path within that bundle's
  embedded FS; `anchor` is a slug of the section heading (lowercased, non-alphanumeric
  runs collapsed to `-`).
- An address **MUST** resolve to exactly one section. Headings repeated within a document
  are disambiguated with a `-2`, `-3` suffix.
- An **unanchored** address (`docs/skills.md`) denotes the **whole document**. Therefore
  the leading section — text before the first `##`, including the title — **MUST** carry
  an anchor of its own, derived from the document title.

Addressing is shared with the CLI: a name accepted by `nine docs <topic>` **MUST** resolve
here, so an address Nine cites is one a human can open. Documents hidden from the CLI
listing (`ROADMAP`) **MUST NOT** be indexed.

---

## R-DOC.2 — Chunking

Documents are split at `##` headings, with two required refinements:

| Rule | Why |
|---|---|
| Headings inside fenced code blocks (` ``` `, `~~~`) **MUST NOT** split a document | The bundled docs quote Markdown samples; splitting on those addresses example text as if it were content |
| A section exceeding `SectionMaxBytes` **MUST** be split again at its `###` headings, its own lead-in retained under the `##` anchor | One vector averaged over several unrelated subtopics ranks poorly for all of them |

The text indexed for a section **MUST** include the document title and section heading in
addition to the body: a section rarely restates its own context, so body-only indexing
makes it unfindable by the terms a query would use.

---

## R-DOC.3 — The index holds addresses, not text

One vector per section in the `docs` namespace, keyed by address. The section body
**MUST NOT** be copied into the store; a read slices it out of the embedded FS.

This is what guarantees the corpus cannot drift from the binary: there is no second copy
to fall out of date, so the documentation Nine cites is always the documentation its own
version ships.

---

## R-DOC.4 — Boot-time indexing and invalidation

The index is built at daemon boot and **MUST** be fingerprinted over (a) every section
address and body and (b) the embedder's provider/model identity.

| Condition | Required behavior |
|---|---|
| Fingerprint matches stored | Skip indexing entirely |
| Corpus changed | Rebuild |
| Embedder provider/model changed | Rebuild — vectors from one embedder are meaningless to another, even though the documents are identical |
| Any section failed to embed | Index what succeeded but **MUST NOT** record the fingerprint, so the next boot retries |
| No embedder configured | Skip indexing; not an error |

A rebuild **MUST** replace the namespace wholesale rather than reconcile entry by entry:
renamed, merged, or removed sections otherwise leave addresses that resolve to nothing,
and a stale hit is worse than a missing one.

---

## R-DOC.5 — Tools

| Tool | Guarantee |
|---|---|
| `doc_search(query, top_k, bundle?)` | Ranks the `docs` namespace and returns address + title + heading + snippet + score. Every returned address **MUST** be one `doc_read` accepts; addresses that no longer resolve **MUST** be skipped rather than returned. A `bundle` filter **MUST NOT** cause the result to fall short of `top_k` when enough in-bundle matches exist. |
| `doc_read(ref)` | Returns the exact text at `ref`. **MUST** accept a `doc_search` address, a short topic name, and a bundle-relative path, with or without `.md`. An unresolvable `ref` **MUST** error with the list of available topics. Output is Markdown behind an address header, not JSON, so a result truncated by the dispatcher's output cap stays readable. |

`doc_search` ranking **MAY** post-process the vector order; the live
implementation adds a subject boost proportional to query/address term overlap,
compensating for the default embedder's lack of inverse document frequency. Any
such reranking **MUST** preserve R-DOC.5's address guarantees. Ranking *quality*
is not specified — it depends on the configured embedder — but a conforming
implementation **SHOULD** carry a labelled retrieval suite as a regression floor.

`doc_read` **MUST NOT** require an embedder. `doc_search` **MUST** be gated on one and
omitted from the advertised set without it, consistent with `tool_search`/`skill_search`
([`../../docs/tool-exposition.md`](../../docs/tool-exposition.md)).

Both are granted regardless of a role's tool allowlist, like `gap_report` (R-ROLE.5).
Granting them together is required: a role able to search the docs but not read them can
locate an answer it cannot retrieve.

---

## R-DOC.6 — Docs and spec are the source of truth

Where the bundled documentation and the observed implementation disagree, the
documentation states the **intended** behavior. Nine **MUST** surface the mismatch as a
defect rather than silently reporting the implementation's behavior as correct.

This rule is what makes the retrieval trustworthy, and it is load-bearing in the other
direction too: a behavior change that skips the embedded docs does not merely leave them
stale, it teaches Nine to assert the stale behavior with a citation.
