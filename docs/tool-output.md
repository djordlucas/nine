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
receives a preview in its place: a banner, then the head of the output, an
elision marker, and the tail.

**The banner comes first**, before the head. The head alone can run to thousands
of characters, so a model reading top-down would otherwise consume a wall of data
before learning anything was withheld. It states what happened, how much was
withheld, the exact path, and — imperatively, as a tool to call rather than a
function signature — how to read the rest. That is the entire mechanism by which
the remainder is reachable.

Head *and* tail matters, roughly two thirds to one. Truncating to a prefix loses
the end of the output, which is where a long command usually reports whether it
worked.

**A successful spill previews far less than the cap allows.** The preview budget
is about 1536 characters against a 2048-token cap: the cap is sized for data that
would be *discarded*, and once the full result is retrievable a large preview is
pure context cost. The full cap applies only when the spill itself failed, where
the data really is lost.

An empty result is also explained rather than sent as nothing, since an empty
string and a failure are indistinguishable to a reader.

## Results that are not text

A sandboxed tool can return bytes — a rendered image, an archive — instead of a
string. Those take the same route for the same reason: the model cannot read
them, and inlining base64 would spend the output budget teaching it nothing. The
bytes go to the file store, base64-encoded because the store is a text column
that does not survive NUL, and the model is handed the path plus a description of
what is there. It costs the tool no `fs.write` grant, the store being Nine's
rather than the operator's filesystem.

Unlike over-cap text, a byte result with no spill sink registered is an **error**
rather than a degraded success: there is no smaller-but-valid form of a truncated
blob.

## Reading a spill back

**One read tool serves both namespaces.** A spill lives in the daemon's file
store, not on the workspace filesystem, but `read_file` routes on the path: a
`spill/` prefix is served from the store, everything else from the workspace. The
alternative — two read tools over two namespaces — produced the most common model
error this design records, which is reaching into the wrong one.

- `read_file` takes a line range, or an offset and a limit, so the model reads a
  window rather than pulling the whole payload back into its context.
- `file_search_text` takes a path, so the model can search *inside* one spilled
  file instead of searching the whole store.

Together these turn a too-large result into a searchable document.

**A spill outlives its turn.** It is kept for seven days and removed by an hourly
sweep — long enough for a later turn to read it back and for `nine trace` to
resolve a journal reference to it, and not forever, because spills are session
debris rather than a namespace the agent curates.

## Passing a payload without reading it

Moving data between tools is a distinct problem from reading it. A model that
must fetch a large file and hand it to another tool would pay for the whole
payload in context, twice, for a value it never needs to see.

A tool can instead declare an input as a **file-store reference**. When the
model supplies a path for that input, Nine substitutes the content before the
tool is called. The model routes the payload by naming a short handle; the bytes
never enter its context at all.

A reference resolves in **either** namespace — a `spill/` path in the store, or an
ordinary workspace file. Resolving only the store would mean "copy this payload"
worked for a truncated tool result and failed for a file the agent had just
written.

This is **declared, never inferred**. A tool must mark the input as a
reference; Nine does not guess that a string looks like a path and silently
swap it for file contents. A string that happens to resemble a path stays a
string.

## Limits

| Limit | Detail |
|-------|--------|
| Spills are swept after seven days | A spill path stays readable across turns and sessions until the hourly sweep removes it at seven days. A path older than that reaches nothing, and `nine trace` on an old journal entry names a file that is gone. |
| A reference needs a path the model has seen | Passing a payload by reference requires the model to know the path, which means it came from a tool result or a file it wrote. There is no listing tool for the store — a forgotten path can only be found by searching content with `file_search_text`. |
| References are declared, never inferred | A tool must mark an input as a file-store reference. Nine does not guess that a string looks like a path and swap in file contents. |
| The cap is global | `max_output_tokens` applies to every tool result. It is not per-tool, so a tool whose useful output is consistently larger always spills. |
| Preview shape is fixed | The model sees a head and a tail. A result whose signal sits in the middle needs a `read_file` or `file_search_text` call to reach. |

> Why the dispatcher rather than a tool-facing API, and the options weighed —
> [../adr/tool-output-spill.md](../adr/tool-output-spill.md).
