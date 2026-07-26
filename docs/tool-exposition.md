# Tool Exposition: top-K ranking vs. on-demand lookup

**Status:** **Option 3 (hybrid) is the live behavior.** The passive always-on
top-K layer already existed (`selectTools`, `ToolTopN` = 20 tool defs; the
assembler's top-3 skills); this change added the active layer on top —
`tool_search` and `skill_search` let the model query the full catalogs mid-turn.
The two coexist, which is the hybrid. The remaining open item is **Option 1**
(re-rank the passive top-K against the evolving scratchpad, not just the opening
query) — orthogonal to the hybrid: it makes the always-on layer fresher, whereas
the search tools compensate for its staleness reactively. See "Implemented".

## Implemented

- `tool_search(query, top_k)` — `internal/agent/register_search.go`. Ranks the
  loop's advertised (and therefore callable) tool set against a model-supplied
  query, returning name + description + input schema. Registered per-loop in
  `AgentBuilder.build` once the tool list and its vectors exist, over a closure
  that returns exactly what this loop can call.
- `skill_search(query, top_k)` — `internal/agent/register_skills.go`. Ranks the
  `skills` vector namespace and returns name + description; the model then
  `skill_read`s the one it wants. Gated on an embedder, like the memory
  semantic tools.
- Both are granted like `gap_report` — advertised, always-included, and surviving
  `RestrictTo` regardless of the role allowlist — but only when an embedder is
  present. Wired as shell-like capabilities in `internal/runtime/builder.go`.

Still open: **Option 1** (re-rank the passive selection against the evolving
scratchpad, not just the opening query) and the token-cost refinement of only
advertising the search tools when the catalog actually exceeds `ToolTopN`.

Spec reconciliation still owed (via `/sync-nine`): `spec/contracts/skills.md`
R-SKILL.3 currently says "There is **no `skill_search` tool**" — now false.

## Problem

Tool *definitions* are shown to the model by relevance, ranked against the
current query's embedding. The ranking is computed from a query vector that is
embedded **once per turn** and reused for every iteration of the inner ReAct
loop:

- `internal/agent/loop.go:267` — `queryVec := embedText(ctx, l.cfg.Embedder, userText)`
  ("Embed user text once per turn; reused across all inner Build() calls.")
- `internal/agent/loop.go:338` — the same `queryVec` is passed to
  `BuildWithUsage` on every inner iteration.
- `internal/context/builder.go:232` — `selectTools` scores each tool by cosine
  similarity to `queryVec`, pins `AlwaysTools` to the front, and keeps the top
  `ToolTopN` (default **20**) non-always tools that fit the budget.

Consequence: a multi-step turn ranks tools against the *initial* user request
for its entire duration. The scratchpad accumulates observations across steps,
but the tool ranking never re-embeds. If a later step needs a capability the
opening prompt didn't hint at, and that tool sits outside the top-N, it is
invisible for the whole turn. "The request changes halfway through" is a real
blind spot for plugin tools beyond top-N.

Scope note: this only affects **plugin tools**. Core "intercepted" tools are
pinned via `AlwaysTools` and are always present.

## Key architectural fact: visibility and callability are decoupled

`selectTools` only decides what defs are *shown* to the model. `Dispatch`
(`internal/agent/dispatcher.go:105`) just looks up `d.handlers[toolName]` — it
does not check whether the def was shown. So a tool pruned by top-K is still
*callable* if the model knows its name and schema, provided it survived
`RestrictTo` (the role allowlist, `dispatcher.go:87`).

This makes a lookup/discovery surface cheap to add: it's a new *discovery* path,
not a new *execution* path. The handlers already exist in the dispatcher map.

## Skills: the parallel surface

Skills are not a separate philosophical case; they are the *same* mechanism
applied to a different catalog. Every turn, the self-model assembler ranks
skills by relevance to the current query and injects the top matches by **name**
into a `## Relevant skills` block of the system prompt:

- `internal/selfmodel/assembler.go:74` — `querySkills` runs
  `store.VectorQuery("skills", queryVec, 3)` and returns the top **3** skill
  names.
- It is fed the **same** per-turn `queryVec` as tool ranking (`assembler.Build`
  is called once per turn with the embedded user message).
- The `skills` vector namespace is kept current by the write/modify hook
  (`internal/agent/register_skills.go:55`) and the boot seed
  (`internal/runtime/skills_seed.go:40,135`), exactly as
  `spec/contracts/skills.md` R-SKILL.4 promises ("current for any future
  ranking/search").

So the model already sees relevant skill *names* passively → it then calls
`skill_read`. The full-enumeration path (`skill_list` → `skill_read`) is the
fallback, not the only discovery route.

That makes tools and skills structurally identical:

| Surface | Passive relevance ranking | Active search tool | Top-K | Same root cause |
|---------|---------------------------|--------------------|-------|-----------------|
| Plugin tools | `selectTools` (top-N defs, default 20) | none | 20 | once-per-turn `queryVec` |
| Skills | `assembler.querySkills` (top-K names) | none | **3** | once-per-turn `queryVec` |

Two consequences for the options below:

- **The root cause is shared.** Both surfaces rank against the *initial* query
  and never re-rank as the turn evolves, so **Option 1 (re-rank per iteration)
  fixes both at once** — re-embed/blend the vector inside the inner loop and
  both `selectTools` and the assembler track the evolving task, no per-surface
  work.
- **The top-K ceiling bites skills harder.** Skills surface only **3** names
  (vs. 20 tool defs) and their bodies are read lazily anyway, so a skill the
  opening prompt didn't hint at is easily excluded. If a `*_search` meta-tool is
  worth adding to *either* surface, skills qualify at least as much as tools —
  and `skill_search` is cheap, since `VectorQuery("skills", …)` and the
  populated namespace already exist (the handler is a thin wrapper over
  `querySkills` with a model-supplied query).

## Is tool lookup already implemented? No.

There is no `tool_search` / `tool_lookup` meta-tool. For skills there is no
model-callable search tool either (`spec/contracts/skills.md:91` — "There is
**no `skill_search` tool**; discovery is `skill_list` followed by
`skill_read`"). But that is not the same as "no semantic search": skills are
already relevance-ranked *passively*. See the next section — the real gap is an
*active*, model-driven search surface, and it is a gap tools and skills share.

## Options

### 1. Re-rank per iteration (smallest change; targets drift directly)

Blend `queryVec` with embeddings of the recent scratchpad, or re-embed each
inner iteration, so the shown tool set tracks the evolving task. Fixes "the
request changed halfway through" with no new model-visible surface.

- Change: ~10 lines across `loop.go` (recompute/blend the vector inside the
  inner loop) and possibly `selectTools`.
- Cost: one extra embed call per inner step (cheap).
- Limit: top-N is still a hard ceiling on how many plugin tools can be seen.

### 2. `tool_search` meta-tool (scales to large catalogs)

Register a meta-tool as an `AlwaysTool`; its handler ranks the full plugin-tool
set (the `ToolWithVector` list already carries per-tool embeddings) against a
model-supplied query and returns matching names + input schemas. The model then
calls a returned tool directly — the handler is already in the dispatcher map.
This is the deferred-tools + `ToolSearch` pattern the Claude Code harness uses.

- Change: new `internal/agent/register_toolsearch.go` + wire it as an
  `AlwaysTool`.
- Cost: an extra LLM round-trip when discovery is needed.
- Weakness: the model must *realize* it should search; if it doesn't know a
  capability could exist, it won't look for it.

### 3. Hybrid (recommended)

Keep a small always-on top-K seeded by the initial query for zero-latency common
cases, **plus** `tool_search` for on-demand discovery of the long tail. Matches
how the Claude Code harness works (top tools inline + `ToolSearch` for the rest).

## Recommendation

- Small tool catalog (dozens): **option 1** alone likely closes the gap.
- Large catalog (hundreds of plugin/MCP tools): **option 3 (hybrid)**.

Note this adds an *active* (model-driven) search surface. It is not, however, a
departure from a "list, don't search" stance — skills are already ranked by
relevance *passively* (see "Skills: the parallel surface" below), so nine
already does semantic ranking; what is new is letting the model *drive* a query
rather than only receiving a top-K it did not ask for. If a `tool_search` is
adopted, consider the symmetric `skill_search` and reconcile the MAY in
`spec/contracts/skills.md` R-SKILL.3 via `/sync-nine`.

## Touch points (for whoever implements)

- `internal/agent/loop.go` — per-turn `queryVec`, inner loop.
- `internal/context/builder.go` — `selectTools`, `ToolTopN`, `AlwaysTools`.
- `internal/agent/dispatcher.go` — `handlers` map, `RestrictTo` (callability).
- `internal/agent/register_*.go` — pattern for a new `register_toolsearch.go`.
- `internal/plugin/contract.go` — `ToolDefinition` / `ToLLMDef` (plugin defs).
