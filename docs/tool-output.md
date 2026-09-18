# Large tool output

A tool can return more than a model can read. Nine caps what reaches the
conversation and writes the rest to the file store, where the model can search
and page through it. Nothing is discarded.

Three mechanisms cover this, all applied by the **dispatcher** — the component
that sits between the model and every tool. Because it holds the complete
result before anything is truncated, none of this requires cooperation from the
tool itself. Tools do not know their output was large.

## The cap

Every tool result is measured against a per-result cap, set by
`max_output_tokens` in configuration. Results under it are passed through
untouched, which is the overwhelming majority.

## Spilling

A result over the cap is written **whole** into the file store, and the model
receives a preview in its place: the head and the tail of the output, with a
banner naming the path where the complete result now lives.

Head *and* tail matters. Truncating to a prefix loses the end of the output,
which is where a long command usually reports whether it worked.

The banner is deliberately verbose. It states what happened, how much was
withheld, and the exact path — a model that receives a preview without knowing
one exists cannot ask for the rest, so the banner is the entire mechanism by
which the remainder is reachable.

An empty result is also explained rather than sent as nothing, since an empty
string and a failure are indistinguishable to a reader.

## Reading a spill back

A spilled result is an ordinary file in the store, so the normal file tools
reach it:

- `file_fetch` takes an offset and a limit, so the model can read a window
  rather than pulling the whole payload back into its context.
- `file_search_text` takes a path, so the model can search *inside* one spilled
  file instead of searching the whole store.

Together these turn a too-large result into a searchable document.

A spill path is addressable **only within the turn that produced it**. Paths do
not accumulate across a conversation, and a stale path from an earlier turn is
not a way to reach old data.

## Passing a payload without reading it

Moving data between tools is a distinct problem from reading it. A model that
must fetch a large file and hand it to another tool would pay for the whole
payload in context, twice, for a value it never needs to see.

A tool can instead declare an input as a **file-store reference**. When the
model supplies a stored path for that input, Nine substitutes the stored
content before the tool is called. The model routes the payload by naming a
short handle; the bytes never enter its context at all.

This is **declared, never inferred**. A tool must mark the input as a
reference; Nine does not guess that a string looks like a path and silently
swap it for file contents. A string that happens to resemble a path stays a
string.

## Limits

| Limit | Detail |
|-------|--------|
| Spill paths live for one turn | A spill path is addressable only within the turn that produced it. Paths do not accumulate across a conversation, and a stale path from an earlier turn reaches nothing. |
| A reference needs a handle from the same turn | Passing a payload by reference requires the model to know the handle, which means it came from an earlier result in that same turn. |
| References are declared, never inferred | A tool must mark an input as a file-store reference. Nine does not guess that a string looks like a path and swap in file contents. |
| The cap is global | `max_output_tokens` applies to every tool result. It is not per-tool, so a tool whose useful output is consistently larger always spills. |
| Preview shape is fixed | The model sees a head and a tail. A result whose signal sits in the middle needs a `file_fetch` or `file_search_text` call to reach. |

> Why the dispatcher rather than a tool-facing API, and the options weighed —
> [../adr/tool-output-spill.md](../adr/tool-output-spill.md).
