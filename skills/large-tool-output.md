---
name: large-tool-output
description: Reading a spilled tool result — search inside a spill/ path with file_search_text rather than paging it from the top
tags: [spill, tool-output, truncation, read_file, file_search_text]
---

## Spilled tool output

A tool result too large for your context is written **whole** to the file store
and replaced by a preview: a banner, the head of the output, an elision marker,
and the tail. Nothing is discarded. The banner comes first and names the exact
`spill/...` path — read it before the data under it.

**Search the spill; do not page it from the top.** The whole point of a spill
is that the result did not fit, so reading it back in windows from byte zero
re-creates the problem the spill solved.

```
file_search_text({"query": "FAIL", "path": "spill/<the path from the banner>"})
# → the matching regions, with their locations

read_file({"path": "spill/<path>", "lines": "840-880"})
# → just that region
```

`read_file` routes on the path: a `spill/` prefix is served from the file store,
everything else from the workspace. One tool, both namespaces — reaching for a
different tool because the path looks unusual is the most common mistake here.

### Working out what to search for

The preview is head **and** tail, roughly two to one, because the end of a long
command's output is usually where it says whether it worked. Read the tail
first: an exit code, a summary line, or a final error tells you what to search
the middle for.

| Result | Search for |
|---|---|
| A test run | `FAIL`, `--- FAIL`, `panic` |
| A build | `error`, `Error:` |
| A long log | The timestamp or request ID the tail points at |
| A large document | The section heading you actually need |

### Handing a payload on without reading it

When a large result is input to another tool rather than something you need to
read, pass the path instead of the content. `write_file` takes `content_ref`:

```
write_file({"path": "logs/run.txt", "content_ref": "spill/<path>"})
```

The bytes go straight from the store to the tool and never enter your context.
A reference resolves in either namespace, so `content_ref` also takes an
ordinary workspace path. This only works where a tool **declares** an input as a
file-store reference; Nine never guesses that a string looks like a path.

### Results that are not text

A tool returning bytes — a rendered image, an archive — takes the same route:
the bytes go to the file store and you are handed the path plus a description
of what is there. You cannot read the bytes themselves. Pass the path to a tool
that can, or report where it is.

## Limits

| Limit | Detail |
|---|---|
| Spills expire | Kept seven days, swept hourly. A later turn can read one back; a much later one cannot. |
| Spills are debris, not a namespace | They are session output, not files you curate. Write anything worth keeping to the workspace. |
| The preview is smaller than the cap | About 1536 characters, well under the token cap, because the full result is retrievable. A larger preview would be pure context cost. |
| A failed spill is different | When the spill itself fails the full cap applies and the rest really is lost — that preview is all there is. |
| Byte results cannot degrade | A byte result with no spill sink is an error, not a truncated success: there is no smaller valid form of a cut-off blob. |
