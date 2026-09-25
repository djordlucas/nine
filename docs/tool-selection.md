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
includes the most relevant tool definitions, up to **20** ranked tools per turn.
Skills are ranked the same way, with a smaller allowance.

**Some tools skip ranking entirely.** The memory, file-search, skill and
queued-message tools, whatever the role's own capability tools are (shell,
delegation, goal management, the job tools), and — in an interactive session —
`ask_human` are always included, whatever the turn is about. They are added
before the ranked set and charged to the same budget: a model that cannot see
`memory_set` because the turn's phrasing ranked it low would lose a capability it
is expected to have at all times.

Ranked tools are then taken in relevance order until either 20 of them are
included or the remaining token budget cannot fit the next one. A tool too large
for what is left is skipped rather than ending the pass, so a single verbose
schema does not shut out everything behind it.

Ranking handles the common case and fails on the specific one. A request phrased
in vocabulary the catalog does not share ranks the right tool below several
plausible wrong ones, and the chance of the needed tool falling outside the cut
rises with catalog size.

## Search, during the turn

Six tools reach the full catalogs mid-turn, past whatever was pre-selected. Each
catalog has both a ranked search and a query-free enumeration, because "find me
something that does X" and "what can you do" are different questions:

| Tool | Reaches |
|------|---------|
| `tool_search` | The tool catalog, by relevance to a query |
| `tool_list` | Every callable tool, names and descriptions |
| `skill_search` | The skill catalog, by relevance to a query |
| `skill_list` | Every skill |
| `doc_search` | Nine's bundled documentation |
| `doc_read` | One bundled document, by name |

Both tool surfaces read the loop's **advertised** set, so they agree with each
other and never name a tool the loop cannot call. `tool_list` needs no embedder —
enumeration does not rank.

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
| Ranking quality depends on the embedder | With `[embeddings].provider = "keyword"` the built-in feature-hashing embedder is used; it matches shared vocabulary, not meaning. |
| `provider = "none"` does not mean "show everything" | With no embedder every tool scores zero, so the always-included set is joined by the first 20 tools in catalog order. The cut still applies; only the ranking is gone. `tool_search` is also unavailable, leaving `tool_list` as the only route to the rest. |
| The ranked count is not configurable | 20 ranked tools per turn is fixed in the daemon. The context budget bounds it from below — a turn with little room includes fewer — but nothing raises it. |
| No feedback loop | A tool that ranked badly and was then found by search does not rank better next time. Rankings are computed per turn from the conversation alone. |
| Search costs a round-trip | A mid-turn lookup spends a tool call before the real work starts. |
| Descriptions are not validated | Nothing checks that two similar tools distinguish themselves, or that a description describes a situation rather than parameters. |

> The options weighed, and the reasoning behind the hybrid:
> [`adr/tool-exposition.md`](../adr/tool-exposition.md).
