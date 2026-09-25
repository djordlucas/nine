---
name: tool-discovery
description: Finding a tool you cannot see — every tool stays callable even when the turn shows only a ranked subset of the catalog
tags: [tools, discovery, tool_search, tool_list, capabilities, gap_report]
---

## Finding a tool you cannot see

**Never conclude you lack a capability from the tools in front of you.** Each
turn shows you a ranked subset of the catalog — about 20 tools plus the ones
always included — because the whole catalog does not fit in one context. Every
other tool is still callable. Visibility is a budget decision; callability is a
permission decision, and they are not the same.

So when the obvious tool is not there, search for it. A tool found mid-turn can
be called immediately, by name, in that same turn.

| You need | Call |
|---|---|
| A capability, by description | `tool_search({"query": "..."})` |
| The whole catalog | `tool_list({})` |
| A procedure, by description | `skill_search({"query": "..."})` |
| A named skill's full text | `skill_read({"name": "..."})` |
| How Nine itself works | `doc_search` then `doc_read` — see `self-documentation` |

### Searching for a capability

```
tool_search({"query": "convert an image to another format"})
# → the most relevant tool names, descriptions, and input schemas
```

Query the **situation**, not a tool name you are guessing at. Descriptions
describe what a tool is for, so "read a web page as text" finds more than
"scraper". Call whatever comes back directly by its name; `tool_search` only
returns tools the loop can actually call, so a result is never a dead end.

Search before you improvise. Reaching for `shell` to do something a dedicated
tool already does gives up that tool's confinement and its error messages.

### Listing everything

```
tool_list({})
tool_list({"include_schemas": true})   # verbose; only when you need the arguments
```

Use `tool_list` for "what can you do" — a user asking about your capabilities,
or an inventory before planning. Use `tool_search` when you are looking for one
specific thing. `tool_list` needs no embedder, so it works when `tool_search`
does not.

### Finding a skill

```
skill_search({"query": "how do I delegate a research task"})
skill_read({"name": "delegation"})
```

Skills are ranked into your context the same way tools are, with a smaller
allowance, so the same rule applies: a skill you cannot see still exists.
`skill_search` when you do not know its name, `skill_list` for the whole set.

### When the capability genuinely is not there

```
gap_report({"description": "No tool can render Markdown to PDF; the task needs one."})
```

Use `gap_report` only after searching — it reports a capability gap to the
supervisor, which tries to resolve it autonomously. Reporting a gap for a tool
that exists and did not rank well wastes that, and you still have not done the
task.

Before reporting a gap, consider whether you can close it yourself:
`tool_write` makes a small JavaScript transform into a permanent tool, and
`js_eval` runs one without saving it. See the `tool-authoring` skill.

### The order to work in

1. Use what is in front of you when it fits.
2. `tool_search` when it does not.
3. `tool_list` when you need the whole picture, or when search is unavailable.
4. Write the tool yourself (`tool_write`, `js_eval`) if it is a transform.
5. `gap_report` only when none of the above can work.

## Limits

| Limit | Detail |
|---|---|
| Search costs a round-trip | A mid-turn lookup spends a tool call before the real work starts. Do not search reflexively when a visible tool already fits. |
| No embedder, no search | With `[embeddings].provider = "none"` nothing ranks and `tool_search` is unavailable, leaving `tool_list` as the only route to the rest of the catalog. |
| Ranking has no memory | A tool that ranked badly and was then found by search does not rank better next turn. Rankings are computed per turn from the conversation alone. |
| Finding a tool is not being granted one | Both tool surfaces read the loop's advertised set, so they never name a tool you cannot call — but a role's allowlist has already narrowed that set before you search it. |
| The ranked count is fixed | 20 ranked tools per turn, bounded from below by the context budget. Nothing raises it. |
