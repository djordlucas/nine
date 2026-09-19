# Tool selection

Nine exposes more tools, skills and documentation than fit in one model's
context, so each turn shows the model a subset. The subset is chosen twice: the
context builder ranks the catalog before the turn, and the model searches the
full catalog during it.

Visibility and callability are separate. A tool that is not shown this turn is
still callable — visibility is a context-budget decision, callability is a
permission decision. That separation is what lets a model discover a tool
mid-turn and call it immediately, without waiting for the next turn to re-rank.

## Ranking, before the turn

The context builder ranks the catalog against the conversation so far and
includes the most relevant tool definitions, up to a configured count. Skills
are ranked the same way, with a smaller allowance.

Ranking handles the common case and fails on the specific one. A request phrased
in vocabulary the catalog does not share ranks the right tool below several
plausible wrong ones, and the chance of the needed tool falling outside the cut
rises with catalog size.

## Search, during the turn

Four tools query the full catalogs mid-turn, past whatever was pre-selected:

| Tool | Searches |
|------|----------|
| `tool_search` | The full tool catalog |
| `skill_search` | The full skill catalog |
| `doc_search` | Nine's bundled documentation |
| `doc_read` | One bundled document, by name |

The two layers cover different failures. Ranking answers "the obvious tool
should already be here." Search answers "I know what I need and it is not in
front of me." A purely passive system cannot recover from a bad ranking; a
purely active one pays a lookup round-trip before every ordinary request.

## Writing a tool description

A tool's description is what ranking matches against and what search queries, so
it determines whether the tool is ever found. Describe the situation the tool
answers, not its parameters.

When two tools could answer the same phrasing, each description should state the
difference and name the other. The model chooses between them on their
descriptions alone.

## Limits

| Limit | Detail |
|-------|--------|
| Ranking quality depends on the embedder | With `[embeddings].provider = "keyword"` the built-in feature-hashing embedder is used; it matches shared vocabulary, not meaning. Setting `none` disables ranking and includes every tool each turn. |
| No feedback loop | A tool that ranked badly and was then found by search does not rank better next time. Rankings are computed per turn from the conversation alone. |
| Search costs a round-trip | A mid-turn lookup spends a tool call before the real work starts. |
| Descriptions are not validated | Nothing checks that two similar tools distinguish themselves, or that a description describes a situation rather than parameters. |

> The options weighed, and the reasoning behind the hybrid:
> [`adr/tool-exposition.md`](../adr/tool-exposition.md).
