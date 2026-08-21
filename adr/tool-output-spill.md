# Large tool output — spilling to the store, and passing it by reference

- **Status:** Implemented (`internal/agent/spill.go`, `internal/agent/refs.go`,
  `internal/runtime/spill.go`).
- **Date:** 2026-07-28.
- **Problem:** a tool result over the dispatcher's cap was cut to a prefix and
  the remainder destroyed. An agent could not fetch a large payload and read it
  later, could not look at the *end* of a long output, and could not hand a big
  result to another tool without the bytes passing through its context.
- **Depends on:** the dispatcher (`spec/contracts/dispatcher.md`), the memory
  file store (`internal/memory/files.go`, `spec/contracts/memory-store.md`), the
  session journal (`event-log.md`).
- **Not this:** plugin capabilities (`docs/plugin-capabilities.md`) — settings, a
  cache dir, and long-running jobs. That note once proposed a reverse channel
  letting a plugin call *into* Nine; the reverse channel was dropped. See §7.

---

## 1. TL;DR

Three mechanisms, all owned by the dispatcher:

1. **Spill.** A result over the cap is written whole to the file store under
   `spill/<agent-id>/`, and the model receives a **head + tail preview** naming
   the path.
2. **Read back.** `file_fetch` takes `offset`/`limit` to read a window, and
   `file_search_text` takes a `path` to search *inside* one file.
3. **Pass by reference.** A tool can declare an input property as a
   **file-store path** (`"x-nine-ref": true`); the daemon substitutes the stored
   content before the call. The model routes a payload by naming a ~40-character
   handle; the bytes never enter its context.

Nothing here involves the plugin, a new socket, or an access-control model. At
the moment the cap is applied the daemon already holds the full output and has
`memory.Store` in hand — the whole feature is one interception.

---

## 2. Why the dispatcher, and not a plugin-facing API

The alternative was to let tools write to memory themselves through a host API
and return a key. Interception wins on three counts:

- **Coverage.** `capOutput` sits on the path of *every* tool. A plugin-facing
  API reaches only native plugins that adopt it — it misses core-intercepted
  tools (`run_agent`, `memory_query`, workflow/goal tools) and misses MCP servers
  entirely (`internal/plugin/mcp.go`), which are third-party stdio processes Nine
  does not control.
- **No new trust boundary.** A host API would need a socket, a capability token,
  and a per-plugin grant model. An in-process function call needs none of it.
  (That cost is also why the host API was dropped outright —
  `docs/plugin-capabilities.md` §2.)
- **One policy, not N.** Otherwise every plugin author re-implements "am I over
  the limit, where do I put it, what key do I return."

The host API remains worth building for its own use cases (plugin-private
cursors, `host.notify`). It is simply not the mechanism for this.

---

## 3. Spill — over-cap results

`Dispatcher.capOrSpill` (`internal/agent/spill.go`) replaces the old
`capOutput`. Output within the cap is passed through untouched. Over-cap output
is handed to the registered `SpillFn`, which `internal/runtime/spill.go` wires
to `memory.FileStore` at `spill/<agent-id>/<tool>-<random>.txt`.

**The preview keeps both ends.** A banner leads, then a head slice, an elision
marker, and a tail slice. The tail is the point: errors, totals, and closing
structure live at the *end* of a long output, and a head-only cut discarded
exactly that.

**A successful spill keeps a *small* preview** — `spilledPreviewChars` (1536),
not the full 8192-character cap. The cap was sized for a world where truncation
had to salvage as much as possible *because the rest was destroyed*. Once the
output is in the store, the remainder is one tool call away, so a large preview
stops being insurance and becomes pure cost — and an actively harmful one. A
live 12B model handed 8 KB of newline-escaped digits lost the request entirely
and replied *"no specific task was provided in your prompt."* A small preview
plus a clear pointer leaves room for the task to survive. When the spill
**fails**, the full cap still applies: there the data really is being discarded,
so keeping as much as possible is correct.

### The banner earns its size

Two findings from live runs, both counter-intuitive enough to record:

1. **Put the banner first, not at the elision point.** The head can be thousands
   of characters; a model reading top-down would otherwise consume a wall of
   data before learning it was truncated. Moving the notice to the front took
   the `tool-output-spill` eval from 1/3 to 2/3 on a 4B model.
2. **Name tools to call, not function signatures.** An earlier banner wrote
   `file_fetch(path, offset, limit)`. Models responded by *writing code* — a
   Python snippet, a JSON fragment — instead of issuing a tool call. The banner
   now says "call the file_search_text tool, set query to… and path to…", in
   the imperative.

The banner therefore costs more than a terse one would (bounded by
`maxPreviewOverhead`, 1024 characters). That is a deliberate trade: it is what
makes the difference between a model retrieving the rest and guessing.

### Empty results must explain themselves

Nine has **two file namespaces** — the workspace filesystem (`shell`,
`read_file`, `write_file`) and the memory file store (`file_store`,
`file_fetch`, spilled output) — and the single most common model error is
reaching into the wrong one. A spill is named `spill/…`, but the file it came
from was `/work/audit.txt`, and models reach for the path they remember.

So every "not there" answer names the namespace and lists what *is* stored
(`MissingStorePathError`, `noSearchHitsMessage`), and says outright when a path
looks like a filesystem path. This matters more than it sounds: `file_search_text`
originally returned a bare `null` for no hits, and a live model (llama3.1:8b)
responded by **inventing a value**. With the explanatory result the same model,
on the same case, corrects itself in one step:

```
file_search_text {path: "/work/audit.txt"}    → No file is stored under … that
                                                looks like a workspace path; this
                                                tool searches the MEMORY FILE STORE
file_search_text {path: "spill/…"}            → [{"snippet": "…pangolin-count=8321…"}]
```

A silent empty result is not a neutral outcome — it is an invitation to
hallucinate.

Slicing is rune-safe at both ends — the previous `output[:maxChars]` could split
a multi-byte character and hand the model invalid UTF-8.

**Failure is not fatal.** If the sink errors, the dispatcher logs it and falls
back to plain truncation. A store outage degrades output quality; it does not
fail the tool call. With no sink registered at all (a bare `Dispatcher`, as in
many tests) the behavior is exactly the pre-spill behavior.

**Units.** Everything the model sees — the notice's counts, `file_fetch`'s
`offset`/`limit`, `FileSlice.Total` — is in **characters**, so the model can do
arithmetic between them without a conversion. The cap itself is compared in
bytes, as a proxy for tokens.

---

## 4. Reading a spill back

| Need | Surface |
|---|---|
| A window of a large file | `file_fetch(path, offset, limit)` → `{content, offset, chars, total}` |
| Find the relevant region | `file_search_text(query, path)` — `path` scopes FTS to one file or prefix |
| The whole thing, to another tool | a ref argument (§5) |

A plain `file_fetch(path)` with no window still returns a bare string, so
existing behavior and recorded fixtures are unchanged. A windowed read returns a
JSON object, because paging requires knowing where you are.

Reading a spill back is itself subject to the cap — a `file_fetch` of a 400 KB
file spills again. That is intended: it is what makes `offset`/`limit` the
correct way to consume one.

### A spill path is addressable only within its own turn

The loop clears the scratchpad at the start of every turn and on success
(`internal/agent/loop.go`). Tool observations — including the banner naming the
spill path — therefore live only inside the turn that produced them. By the next
turn the model has no reference to the spilled file at all.

So the supported shape is **fetch and consume in one turn**. Across turns the
agent must carry the path itself (store it with `memory_set`, or name it in its
answer), or rediscover it with `file_list("spill/")`. Nothing surfaces it
automatically — spills are deliberately kept out of the context builder (§6).

Making recent spills discoverable across turns is a reasonable future
enhancement; it is a context-builder change, not a dispatcher one, and is out of
scope here.

### Tuning the cap

`[tools] max_output_tokens` in `nine.toml` overrides the 2048-token default
(`docs/configuration.md`). It is `[tools]` and not `[agent]` because `[[agent]]`
is already the standing-agent table array. Raising it is rarely necessary — the
data is not lost either way — but it is the knob for a deployment whose models
should routinely see more of a large result inline.

**In the journal**, a `tool_end` event gains `spill_path` and `output_chars`.
The journal stores the *pointer*, not the payload — the bytes are already in the
file store, and duplicating them into the event log would double the cost of
every large result. `nine trace` prints `spilled: <n> chars → <path>`.

---

## 5. Passing by reference — `x-nine-ref`

A tool declares that a string property carries a **file-store path** rather than
a literal value:

```json
"content": {"type": "string", "x-nine-ref": true,
            "description": "File-store path whose content to parse."}
```

Before dispatch, `Dispatcher.expandRefs` (`internal/agent/refs.go`) reads that
path out of the store and substitutes the content. Plugins get this from the
schema they already advertise via `plugin.describe`; core tools declare it in
`InterceptedDefs`.

The end-to-end path for "fetch something large, then process it":

```
http_get  ──▶ 412 KB ──▶ daemon spills ──▶ model sees "spill/c-42/http_get-a3f1.txt"
                                                    │
model calls parse_csv(content_ref: "spill/c-42/…")  ▼
                          daemon expands the ref ──▶ plugin receives 412 KB
```

The model handled a path. The payload moved daemon-side.

### Declared, never inferred

Expanding any argument that *looks like* a path would corrupt tools whose
arguments are genuinely paths: `file_fetch(path)` would have its path replaced
by the file's contents. Only properties a tool explicitly marks are ever
touched. This is the single most important design constraint in the feature, and
`TestUnmarkedPathArgumentIsNotExpanded` pins it.

### Other rules

- **Optional.** Marking a property makes it ref-*capable*, not required. An
  absent or empty ref argument is left alone.
- **Errors surface.** An unresolvable path fails the call, naming the path, so
  the model can correct it — it is never silently passed through as a literal.
- **Bounded.** `MaxRefBytes` (8 MiB) caps one expansion. Refs remove data from
  the model's context, but the bytes still cross the plugin socket into a
  subprocess that may buffer them whole. Over the limit, the error points at
  `file_fetch` windowing instead.
- **Approval and hooks see the handle.** The HITL approval gate and post-call
  hooks receive the model's *original* arguments. A human approving a call
  should read the path the model chose, not the megabyte behind it. A rejected
  call never reads the ref at all.

### The remaining limit

This solves the *context* problem completely. The bytes still transit the daemon
once (plugin → daemon → plugin). A plugin writing directly to the store without
returning the payload would need a reverse channel into memory, which
`docs/plugin-capabilities.md` §2 rules out for good. That would have been a
socket-throughput optimization, never a context one, so nothing is lost here.

---

## 6. Safety

Spilling does **not** widen the trust surface. The model was already going to
see a truncated prefix of the same untrusted tool output as an observation;
spilling adds *retrieval*, not trust. Two invariants keep it that way:

- **`spill/` is daemon-owned.** `file_store` refuses to write under it, so a
  spilled file is always exactly what a tool returned — never something the
  model composed there and later cited as a tool result.
- **Spills are never embedded.** Nothing under `spill/` enters the vector pool,
  so the context builder's pull-surfacing can never drag an untrusted blob into
  a later turn unbidden. It enters context only when the model explicitly reads
  it.

Ref resolution grants no new read authority: the model can already read any
stored path with `file_fetch`. It changes *who carries the bytes*, not *what is
reachable*.

**Retention.** Spills are per-session debris. `RunSpillSweeper` deletes those
older than `SpillRetention` (7 days) on boot and hourly after — without it the
`files` table grows without bound. `FileDeleteOlderThan` rejects an empty prefix
and a non-positive age, so the sweep can never clear the store.

---

## 7. Relationship to the plugin capabilities note

`docs/plugin-capabilities.md` (then a host-API proposal) called this out as a
separate feature, and it shipped separately. The call held: that note has since
dropped the reverse channel entirely, so the one point where the two designs
touched — a plugin writing its own result to the store — no longer exists, and
this feature never needed it. The two do meet again in one place, harmlessly:
a long-running **job**'s output is run through this same cap-or-spill path when
the daemon collects it.

---

## 8. Testing

- **Unit** — `internal/agent/spill_test.go` (preview shape, head+tail retention,
  budget, rune safety, sink-failure fallback, no-sink parity),
  `internal/agent/refs_test.go` (declaration parsing, expansion, the
  unmarked-path guard, approval/hook argument identity),
  `internal/agent/register_files_test.go` (the `spill/` write guard, windowed
  fetch, scoped search, `content_ref`).
- **Store** — `internal/memory/files_range_test.go` (windowing, paging
  round-trip, character orientation, scoped FTS, retention guards).
- **Integration** — `internal/runtime/spill_test.go` drives spill → store →
  ref-expansion against a real store.
- **Evals** — `tool-output-spill` (the answer is in the elided tail, so the case
  fails against head-only truncation), `spill-read-back` (the answer is in the
  elided *middle*, forcing a genuine read-back), `spill-ref-passing` (graded on
  the archived copy containing data the model never saw). Two new assertions
  support them: `side_effects.stored_files` and `trajectory.spills`
  (`docs/evals.md` §2–3).

Both retrieval cases are confirmed end-to-end against a live model
(qwen3.5:9b, a small-class local model):

```
CALL read_file {"path": "/work/audit.txt"}
DONE read_file spill="spill/…/read_file-12fa28d4.txt"
CALL file_search_text {"path": "spill/…", "query": "pangolin-count"}
DONE  out=[{"snippet":"… [pangolin]-[count]=8321 …"}]
ANSWER: The pangolin-count value recorded in /work/audit.txt is 8321.
```

```
CALL read_file  → spilled
CALL file_store {"path": "archive/audit.txt", "content_ref": "spill/…"} → ok
   → archive/audit.txt contains the needle from the elided middle
```

The model chose retrieval on its own from the banner, and routed the payload by
handle without ever seeing it.
