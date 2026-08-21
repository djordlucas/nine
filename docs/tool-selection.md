# How tools reach a turn

Nine can expose far more tools, skills and documentation than fit in one
model's context. What the model *sees* on a given turn is therefore a
selection, and the selection is made twice: once for it, and once by it.

## Visibility and callability are separate

A tool that is not shown on this turn is still callable. Visibility is a
context-budget decision; callability is a permission decision. Keeping the two
apart is what makes an active lookup possible at all — a model can discover a
tool mid-turn and call it immediately, without waiting for the next turn to
re-rank.

This is worth stating plainly because the opposite is the more common design,
and it is the assumption that makes catalog growth feel like a hard ceiling.

## The passive layer: ranking

Before each turn the **context builder** ranks the catalog against the
conversation so far and includes the most relevant tool definitions, up to a
configured count. Skills are ranked the same way, with a smaller allowance.

Ranking is good at the common case and weak at the specific one. A request that
uses vocabulary the catalog does not share ranks the right tool below several
plausible wrong ones — and as a catalog grows, the chance that the needed tool
falls outside the cut rises.

## The active layer: search

So the model can also look. `tool_search` and `skill_search` query the full
catalogs mid-turn, beyond whatever was pre-selected, and `doc_search` and
`doc_read` extend the same idea to Nine's own bundled documentation.

The two layers answer different failure modes. Ranking handles "the obvious
tool should already be here". Search handles "I know what I need and it is not
in front of me". Neither alone is sufficient: a purely passive system cannot
recover from a bad ranking, and a purely active one pays a lookup round-trip
before every ordinary request.

## What this means when writing tools

A tool's **description is its retrieval surface**. It is what ranking matches
against and what search queries, so a description that explains what the tool
is *for* — the situation it answers — will be found, and one that only restates
its parameters will not.

This matters most when two tools could plausibly answer the same phrasing. If
they differ in some consequence the model cannot see, each description should
say so and name the other, because the model is choosing between them on their
descriptions alone.

> The options weighed, and why the hybrid won —
> [../adr/tool-exposition.md](../adr/tool-exposition.md).
