# Deferred tool schemas

**Status:** **Proposed** — 2026-10-07 · **Builds on:** `adr/tool-exposition.md`
(the hybrid it shipped: passive top-K plus `tool_search` / `tool_list`) ·
**Amends:** R-ROLE.4 (what "advertised" means; see §6) · **Depends on:** the eval
work in §7 landing *before* the default flips

Nine should send a **small resident set** of tools with full schemas (~1,650
tokens; ~2,025 with `run_agent`), list **every other tool by name only** in the system
prompt (~170 tokens), and load a deferred tool's schema through `tool_search` only
when the model asks for it or calls it — the pattern Claude Code uses for its own
deferred tools. That cuts the base tool cost of an interactive orchestrator from
~8,000 tokens to **~2,000**. It ships behind `[context] tool_exposition = "deferred"` and
becomes the default only after an A/B run of the full eval corpus, plus eight new
cases (§7), shows no loss in tool-use outcomes.

Today every request carries the full JSON schema of nearly every tool the loop can
call. On an orchestrator that is the largest fixed cost in the request — larger
than the system prompt, self-model and enrichment combined — paid on every inner
iteration of every turn, whether the turn touches one tool or none.

## 1. Problem

### 1.1 Where the tokens go

Measured from the committed turn snapshots
(`tests/evals/runner/testdata/snapshots/`), using the builder's own estimator
(`countTokens`, 3.45 bytes/token — `internal/context/builder.go:367`):

| Session kind | Tools advertised | Tool tokens | System prompt tokens |
|---|---|---|---|
| orchestrator / pursue / executor | 50 | ~6,560 | ~830 |
| orchestrator-interactive | 51 | ~6,640 | ~820 |
| background goal session | 51 | ~6,690 | ~825 |
| standing agent / condition trigger | 24 | ~2,620 | ~375 |
| reflection | 14 | ~1,320 | ~750 |

The snapshot harness runs a thin plugin set. A production daemon adds its plugin and
MCP tools (shell, web, HTTP, browser, operator MCP servers) up to `ToolTopN`, which
puts a typical interactive orchestrator at **~8k tokens of tool definitions** before
the user has said anything.

The heaviest definitions in the orchestrator snapshot:

| Tool | ~Tokens | | Tool | ~Tokens |
|---|---|---|---|---|
| `run_agents` | 499 | | `goal_create` | 290 |
| `run_agent` | 395 | | `list_files` | 238 |
| `capability_request` | 391 | | `doc_search` | 233 |
| `write_file` | 386 | | `restore_file` | 209 |
| `read_file` | 326 | | `diff_file` | 193 |
| `edit_file` | 314 | | `doc_read` | 176 |

### 1.2 Why `ToolTopN` ranking does not reduce it

`tool-exposition.md` framed tool visibility as a top-K problem, and `selectTools`
does rank — but only the **non-always** tools, and almost nothing is non-always.
`alwaysTools` (`internal/runtime/builder.go:627-631`) is `coreToolNames` plus every
`shellTools` entry: job tools, delegation and workflow tools, goal tools, the catalog
meta-tools, the doc pair, the generated-tier meta-tools and `ask_human`. On the
orchestrator that is ~45 of 51 definitions pinned unconditionally. `ToolTopN = 20`
trims the plugin tail; it never touches the ~6.5k that is pinned.

Turning `ToolTopN` down would therefore save little.

### 1.3 Impact on small local models

- **Small local models are the target.** The eval matrix is led by `qwen3.5:4b`
  over Ollama. On an 8k–32k context window, 8k of fixed tool schema is a quarter to
  all of the room, and `ContextBudget` trimming takes it out of history and
  scratchpad — the things the model actually needs mid-task.
- **Attention, not just room.** Fifty schemas compete for a small model's attention
  on every step. Fewer definitions may *improve* tool selection; §7 tests this
  rather than assuming it.
- **Prefix reuse.** Ollama and llama.cpp reuse the KV cache for a matching prompt
  prefix, and the tool block is rendered into the prompt by the model's template.
  Today the block's content can change turn to turn as ranking reshuffles the
  plugin tail. A small, stable resident block followed by an append-only promoted
  block keeps more of the prefix reusable. Unmeasured; §7.4 measures it.

### 1.4 Existing hybrid

`tool-exposition.md` established the facts this design leans on:

- **Visibility and callability are decoupled.** `Dispatch` looks up
  `d.handlers[name]`; it does not check that the definition was shown. A tool the
  model was never sent a schema for is still callable if it survived `RestrictTo`.
- **`tool_search` already returns full schemas** for the loop's callable set.
- The one weakness that ADR named for on-demand lookup still stands: *"the model
  must realize it should search; if it doesn't know a capability could exist, it
  won't look for it."* §3.2 (the index) addresses it.

## 2. Decision

Introduce a second tool-exposition mode, `deferred`, behind a config flag, and make
it the default only once §7's acceptance criteria pass on the model matrix.

In `deferred` mode each request carries three things in place of today's ~50 full
definitions:

1. **Resident tools** — a small fixed set sent with full schemas, chosen for call
   frequency (§3.1). Budget: **≤ 2,000 tokens**.
2. **The tool index** — the name of every remaining callable tool, rendered into
   the system prompt, grouped by toolkit (§3.2). Measured at **~170 tokens** for the
   orchestrator's 40 deferred tools; capped at 600 so plugin and MCP names fit.
3. **Promoted tools** — deferred tools the model has loaded this session, sent with
   full schemas, appended after the resident set (§3.3).

Target: **~2,000 tokens** of base tool cost on an interactive orchestrator, down
from ~8,000 — measured from the snapshot: resident 1,650 (2,025 with `run_agent`)
plus a ~170-token index. The gate in §7.5 is ≤ 2,500, leaving room for production
plugin names. Tool-use outcomes must stay unchanged within the tolerance in §7.5.

`ranked` (today's behavior) stays selectable, and is the comparison arm for every
eval in §7.

## 3. Design

### 3.1 The resident set

The first cut, for interactive and orchestrator sessions, by role capability:

| Tool | Why resident | ~Tokens |
|---|---|---|
| `read_file`, `write_file`, `edit_file`, `list_files`, `file_search_text` | the workspace is the agent's only file namespace; nearly every task touches it | ~1,400 |
| `memory_get`, `memory_set` | cheap and frequent | ~120 |
| `skill_read` | the self-model already names relevant skills; reading one must not need a load | ~55 |
| `tool_search` | the load path (§3.3); rewritten description | ~170 |
| `ask_human` | interactive only; a missing `ask_human` turns into guessing | ~100 |
| `run_agent` | delegating roles only | ~395 |

~1,650 tokens without delegation, ~2,025 with it — the delegating case is the one
place the 2,000 budget is exceeded, and §3.5 trims `run_agent`'s schema to bring it
back under.

This list is a **starting point, not a decision**. Phase 1 (§8) replaces it with one
derived from data: per-tool call frequency over the eval journals and an opt-in
`session_events` query on a real store, keeping tools above a frequency threshold
whose schema is also non-trivial to guess. A tool that is called often but has a
one-argument schema (`time`, `memory_delete`) may be better off deferred — the index
line plus call-to-load (§3.4) is enough for it.

The resident set is configurable, because the right set is deployment-specific — a
browser-automation deployment wants its browser tools resident:

```toml
[context]
tool_exposition = "deferred"   # "ranked" (current) | "deferred"
resident_tools  = []           # extra tools to keep resident, by name; plugin and MCP names allowed
resident_budget = 2000         # resident + promoted schema budget, tokens
```

A role skill may also name resident tools for itself (`resident:` in its
frontmatter), which is how a narrow standing role keeps its handful of tools
resident without an operator editing config.

**Small catalogs are not deferred.** If a loop's whole advertised set fits in
`resident_budget`, every tool is resident and no index is rendered. That covers
reflection (~1.3k) and most allowlist roles, and closes the open item
`tool-exposition.md` left ("only advertising the search tools when the catalog
actually exceeds `ToolTopN`"): the catalog meta-tools only appear when there is
something deferred to find.

### 3.2 The tool index

The index lists every deferred tool by name, so the model never has to guess that a
capability exists before searching for it. It is rendered as a system-prompt section, after the role body and before enrichment:

```text
## More tools
Call any of these by name. Load the full schema first with
tool_search(names=[...]) if you are unsure of the arguments.

workspace: copy_file, move_file, delete_file (to trash), trash_list,
  restore_file, diff_file
memory: memory_list, memory_delete
skills: skill_list, skill_search, skill_write, skill_modify
delegation: run_agents, workflow_create, workflow_get,
  workflow_update, workflow_list, workflow_retry_step, goal_create
jobs: job_check, job_wait, job_cancel, job_list
...
mcp/fixture: fixture__mcp_echo — echo text back
```

- **Grouped by toolkit**, not alphabetical. Group membership comes from a `toolkit`
  field on intercepted definitions and from the plugin or MCP server name for
  external tools. Grouping makes the index shorter (shared prefixes collapse), lets
  the model reason "this is a workspace thing", and gives `tool_search` a unit to
  load (§3.3).
- **Names only.** Nine's own tool names describe themselves (`restore_file`,
  `trash_list`, `job_wait`), so the index carries no descriptions. The exceptions
  are a parenthetical where the name alone misleads — `delete_file (to trash)` —
  and plugin or MCP tools, whose names Nine does not control, which get their
  description's first clause, truncated to ~10 words. Whether names alone are
  enough for a small model is what `deferred-discover-from-index` and
  `deferred-mid-turn-drift` test; if they are not, glosses are the first thing
  to add back.
- **Derived, never hand-maintained.** The index is rendered from the same
  `ToolWithVector` slice `assembleTools` builds, so it is re-rendered whenever
  `toolsForTurn` re-assembles after a catalog change, and it cannot list a tool the
  role allowlist removed.
- **Stable order.** Toolkits in a fixed order, tools within a toolkit in
  registration order, so the index is identical turn to turn for a given catalog and
  stays in the reusable prefix.

### 3.3 Loading and promotion

`tool_search` gains a `names` argument alongside `query`:

```jsonc
{"query": "restore a deleted file"}            // ranked search, as today
{"names": ["restore_file", "trash_list"]}       // exact load
{"names": ["workspace"]}                        // a whole toolkit
```

Both forms return the matched schemas in the tool result **and promote them**:
the definitions join the loop's advertised `tools` array from the next inner
iteration onward. Promotion is what makes this work across providers — after one
load, the call is an ordinary native function call against a declared tool, not a
call to a name the provider has never seen.

- **Sticky for the session**, held on the loop. A tool loaded in turn 3 is still
  there in turn 7, so a conversation about trash does not re-search every turn.
- **Bounded.** Promoted schemas count against `resident_budget`; past it, the
  least-recently-*called* promoted tool is demoted back to the index. Demotion
  only ever happens between turns, never mid-turn, so a tool the model just loaded
  cannot vanish before it calls it.
- **Appended after the resident set**, in promotion order, so promotion extends
  the prompt prefix rather than rewriting it.
- **Journaled.** A `tool_promoted` / `tool_demoted` event in `session_events`, so the
  evals and `nine` inspection tools can see the exposition state at every step.
- **Embedder-independent.** Today `tool_search` is gated on an embedder. In
  deferred mode it cannot be: it is the load path. `names` needs no ranking, and
  `query` gains the BM25 fallback `doc_search` already uses, fused with the vector
  half when an embedder exists.

`tool_list` stays, deferred and indexed under `catalog:` — the index makes it
redundant for discovery, but "what can you do?" is still a real question.

### 3.4 Call-to-load

Because callability is already decoupled from visibility, a model that calls an
indexed tool directly — without loading it — is dispatched normally. For the many
tools whose arguments are obvious from the name (`trash_list`, `job_list`,
`memory_delete`), that costs zero extra round trips.

When such a call fails argument validation, the error result carries the tool's
schema and the tool is promoted, so the model's next attempt is against a declared
definition — the same approach as the dispatcher's `didYouMean` hint for unknown
names.

Provider caveat: Ollama and most OpenAI-compatible local servers parse whatever
function name the model emits; a hosted OpenAI-style endpoint may constrain calls
to declared functions. On such a provider call-to-load simply never fires and the
`tool_search` path carries everything. §7.3 includes the case that tells the two
apart.

### 3.5 Schema hygiene

Independently of deferral, several definitions are larger than their behavior
requires. Trimming them changes no behavior:

- `run_agents` (~500) restates most of `run_agent`'s schema; reference the shared
  shape in the description instead of repeating each field's prose.
- `capability_request` (~390) carries explanatory prose that belongs in the doc
  bundle (`doc_read`), not the schema.
- Property `description`s that restate the property name ("path: the path") go.

It applies in both modes and ships alone in phase 0, so its saving is measured
separately from deferral's.

### 3.6 Unchanged behavior

- **Dispatch.** No new execution path. The handlers map, `RestrictTo`, approval
  gates and the generated-tier gates are untouched.
- **Role boundaries.** Boundary 1 (the advertised list) and boundary 2 (dispatch)
  both still exclude a disallowed tool; §6 restates boundary 1.
- **Skills.** The self-model's `## Relevant skills` block is already the
  index-then-read pattern this record generalizes; it stays as is.

## 4. Alternatives considered

| Option | Saves | Why not (alone) |
|---|---|---|
| Lower `ToolTopN` | ~0–1k | Only trims the non-pinned plugin tail; the ~6.5k pinned block is untouched (§1.2). |
| Schema hygiene only (§3.5) | ~1–1.5k | Worth doing; nowhere near the target. Kept as phase 0. |
| Unpin, and let `selectTools` rank everything | ~4k | Puts core tools at the mercy of a once-per-turn query embedding — the exact blind spot `tool-exposition.md` documented — and gives the model no idea what it is not seeing. |
| Index only, nothing resident | ~6.4k | Every task's first file read costs a load round trip. On a 4b model each extra step is a chance to stall. The resident set exists so the common path pays nothing. |
| `tool_search` only, no index | ~5.5k | Leaves the "model must know to look" weakness fully open. |
| Toolkits as the only load unit | — | Folded in: the index is grouped and `names` accepts a toolkit, but loading single tools stays possible so a one-tool need does not pull in six schemas. |

## 5. Risks

| Risk | Mitigation | Measured by |
|---|---|---|
| Small models do not load before calling, and then thrash on argument errors | Call-to-load returns the schema in the error (§3.4) | `deferred-call-to-load`, `no_stall` across the corpus |
| Discovery round trips cost more tokens over a task than they save per request | The metric is **input tokens per passed case**, not per request (§7.4) | A/B report |
| A tool used every turn ends up deferred | Data-derived resident set; `resident_tools`; stickiness | Per-tool load counts in the A/B report |
| Index wording steers behavior (a gloss is effectively prompt text) | Generated, minimal, no imperative wording; reviewed in snapshots | Snapshot diffs |
| A forbidden tool leaks through the index or `tool_search` | Index and search read the same role-filtered slice as today | `role-report-writer-no-shell` extended (§7.3) |
| Mid-turn promotion breaks prefix reuse more than ranking did | Append-only promotion, between-turn demotion | Prefix-reuse measurement (§7.4) |

## 6. Contract changes

- **R-ROLE.4** — "absent from the advertised list" becomes "absent from the resident
  set, the tool index, the promoted set and every `tool_search` result." The
  observable guarantee is unchanged; the definition of *advertised* widens to cover
  the new surfaces so the conformance check keeps meaning what it says.
- **spec/contracts/dispatcher.md** — record call-to-load: a validation failure on a
  non-promoted tool returns its schema and promotes it.
- **spec/contracts/embedder.md** — `tool_search` is no longer embedder-gated in
  deferred mode.
- **docs/** — the tool-exposition behavior is user-visible through config; a
  `docs/` page for `[context] tool_exposition` once it ships, via `/sync-nine`.

## 7. Evals

The corpus today cannot show "no impact on tool use": every case runs under one
exposition mode and nothing records token cost. This section adds an A/B arm and
token reporting to the harness, new assertions, and new cases.

### 7.1 Harness exposition arm

- **`NINE_EVAL_ARMS=ranked,deferred`** adds an arm dimension to the live matrix,
  applied as a `context.tool_exposition` override through the same path
  `session.config` already uses. Default stays one arm (the configured default) so
  `make eval-live` cost does not double unasked.
- **Every existing Track-L case runs in both arms.** The existing corpus is the
  main regression eval: it already asserts on side effects and trajectories that
  do not care how tools were advertised, so a case that passes in `ranked` and
  fails in `deferred` is a direct, model-measured regression.
- **Report columns** per case × model × arm: pass rate, turns, inner iterations,
  `tool_search` calls, call-to-load events, **prompt tokens** (provider-reported
  `prompt_eval_count` where available, else the builder's estimate), and the
  base tool-token figure from `BuildReport`.
- **`/sync-evals`**: the harness mirrors `runtime.Assemble`; the deferred-mode
  wiring in `internal/runtime/builder.go` is exactly the kind of additive change
  the stop hook exists to catch.

### 7.2 New assertions

Additions to `Trajectory` / `LLMRequestExpect` (`tests/evals/runner/case.go`):

| Assertion | Checks |
|---|---|
| `llm_request.tool_tokens_max: N` | No request's tool block (resident + promoted schemas + index) exceeded N estimated tokens |
| `llm_request.tool_advertised_all_of` | Each name was advertised with a full schema in at least one request (proves promotion) |
| `llm_request.tool_indexed_none_of` | A name never appeared in the index — the role-leak check for the new surface |
| `tool_calls: {name: {min, max}}` | Per-tool call bounds — e.g. `tool_search: {max: 1}` across a multi-turn case |
| `promotions: {min, max}` | Count of `tool_promoted` journal events |

`tool_advertised_none_of` keeps its meaning and is extended to fail on an index or
`tool_search` appearance too, so existing cases using it get the stronger check for
free.

### 7.3 New cases

Each forces the behavior through the prompt, never names the target tool, and
asserts on the strongest available signal (docs/evals.md §1). Every case declares
`session.config: {context.tool_exposition: deferred}` unless noted, so it tests the
mode even outside an A/B run.

| Case | Tier | Forces | Asserts |
|---|---|---|---|
| `deferred-discover-from-index` | basic | A task only a deferred tool does: "I deleted `plan.md` by mistake, get it back." (`setup` puts it in the trash) | side effect: `plan.md` restored with content; trajectory: `restore_file` called; `no_stall` |
| `deferred-mid-turn-drift` | multi_step | The drift case `tool-exposition.md` identified: "Read `instructions.txt` and do what it says." The file asks for a deferred capability (move a file into `archive/`), so the opening query gives no hint | side effect: file moved; `move_file` called; `max_turns` |
| `deferred-call-to-load` | basic | A deferred tool with a non-obvious argument shape, so a bare call from the index fails validation once | side effect correct; a call-to-load event present; `tool_calls: {<tool>: {max: 3}}` — it recovers rather than loops |
| `deferred-promotion-sticky` | multi_step | Two turns needing the same deferred toolkit (trash, then restore) | `tool_search: {max: 1}`; `promotions: {max: 2}`; second turn's request has the tool advertised |
| `deferred-mcp-discovery` | basic | `mcp-tool-call`'s fixture, without naming the tool — the model must find it in the index under `mcp/fixture` | `fixture__mcp_echo` called; output in the answer |
| `deferred-resident-no-load` | smoke | A plain file read-and-answer task | `tool_search: {max: 0}` — the common path pays nothing; `tool_tokens_max: 2500` |
| `deferred-no-embedder` | basic | `deferred-discover-from-index` with no embedder configured | same side effect; proves the BM25 / `names` path |
| `deferred-small-catalog` | smoke | An allowlist role whose tools fit `resident_budget` | `system_not_contains: ["## More tools"]`; `tool_search` not advertised |
| `role-report-writer-no-shell` (extended) | — | existing case, both arms | `tool_indexed_none_of` for its forbidden tools |

Track R: `deferred-discover-from-index` and `deferred-promotion-sticky` are recorded
as replay fixtures once they pass live, so the promotion and index-rendering code is
guarded on every PR without a model.

Snapshots: `runner/snapshot_test.go` gains a deferred variant of each session kind,
so the exact resident set, index text and promoted block are reviewed in diffs like
every other prompt change.

### 7.4 Token and prefix measurement

Not pass/fail cases — measurements reported alongside the matrix:

- **Base tool tokens** per session kind, from `BuildReport`, both arms. The headline
  number: target ~2,000, gate ≤ 2,500, for the interactive orchestrator.
- **Prompt tokens per passed case**, median per model and arm. This is the honest
  cost metric: deferral that saves 5k per request but adds three discovery turns can
  lose overall.
- **Prefix reuse** on Ollama: time-to-first-token and `prompt_eval_count` vs. total
  prompt size across a multi-step case, both arms. Reported, not gated (§1.3).

### 7.5 Acceptance criteria for flipping the default

Per model in the matrix (at minimum `qwen3.5:4b` and the largest model the matrix
runs; plus one OpenAI-compatible endpoint if available, for §3.4's caveat):

1. **No regression on the existing corpus.** No case meeting its `pass_threshold` in
   `ranked` misses it in `deferred`. A case that misses is investigated per
   docs/evals.md §1 — fixed in Nine or in the case, never by adding steering prompt
   text to pass it.
2. **New cases pass** at their thresholds in `deferred`.
3. **Base tool tokens ≤ 2,500** on the interactive orchestrator (target ~2,000).
4. **Median prompt tokens per passed case** in `deferred` ≤ `ranked`.
5. **Median turns per passed case** in `deferred` ≤ `ranked` + 1.

Noise: at `runs: 3`, a single 2/3 → 1/3 flip is within the spread docs/evals.md
already documents for small models. A case that regresses is re-run at `runs: 9`
in both arms before it counts against the criteria.

## 8. Phases

| Phase | Ships | Default |
|---|---|---|
| 0 | Schema hygiene (§3.5). `BuildReport` tool-token figure surfaced. Baseline numbers recorded in this file. | unchanged |
| 1 | Eval harness: arms, report columns, §7.2 assertions. New cases written; run in `ranked` to confirm they are mode-agnostic where they should be. Per-tool frequency query; resident set derived from it. | unchanged |
| 2 | `deferred` mode behind `[context] tool_exposition`: resident set, index, `tool_search(names)`, promotion, call-to-load, BM25 fallback, journal events. Snapshots and Track-R fixtures. | `ranked` |
| 3 | A/B on the matrix; tune resident set and index wording against §7.5. Results recorded here. | `ranked` |
| 4 | Flip default to `deferred`. Spec (§6) and docs via `/sync-nine`. | `deferred` |

## 9. Touch points

- `internal/context/builder.go` — `selectTools`, a resident/promoted split,
  index rendering as a system section, `toolTokens` for the budget, `BuildReport`.
- `internal/runtime/builder.go` — `alwaysTools`, `assembleTools`, `toolsForTurn`;
  per-loop promoted set; config plumbing.
- `internal/agent/register_search.go` — `tool_search` `names`, BM25 fusion,
  promotion callback.
- `internal/agent/dispatcher.go` — call-to-load on validation failure.
- `internal/agent/*` intercepted defs — `toolkit` field; schema hygiene.
- `tests/evals/runner/` — `case.go`, `assert.go`, `live.go`, `report.go`,
  `harness.go`, `snapshot_test.go`.
- `tests/evals/cases/` — §7.3.
- `spec/contracts/roles.md`, `dispatcher.md`, `embedder.md`; `spec/conformance.md`
  R-ROLE.4.

## 10. Open idea: tools as skill-backed processes

*Raised 2026-10-08; not decided.*

Every tool is described by a skill — when to use it, its arguments, examples — and the
model sees tools by name only. To use one, the model spawns it with its input in plain
words; a process reads the tool's skill, turns the input into a valid call, runs it, and
returns the result. The model never writes a tool's JSON.

| | Gain | Cost |
|---|---|---|
| Prompt | no schemas in the request at all, below §3's ~2,000 tokens | — |
| Small models | intent instead of JSON, where 4b and 9b most often fail (wrong or missing arguments) | — |
| One surface | a tool is a skill plus a program; its usage is readable with `skill_read` and lives beside it | — |
| Calls | — | a model turn per call to map intent to arguments: twice the calls, the latency and the budget |
| Validation | — | the mapping can be wrong in ways a schema check catches today |
| Process model | — | a process runs until stopped (`adr/process-sessions.md` §2); a per-call spawn is a job's shape, not a process's |

A middle form keeps most of the gain without the extra turn: the **skill is the load unit**
of §3.3 — `tool_search`, or calling a deferred tool, returns its skill (usage and schema)
instead of a bare schema, so how to use a tool arrives with the means to call it. The full
form is worth measuring only where models fail schemas, as one more exposition arm in
§7.1, against the same corpus.

## Limits

| Limit | Detail |
|-------|--------|
| Nothing is built | Every section above is proposed. Baseline token figures are estimates from `countTokens` (3.45 bytes/token), not a real tokenizer — see `adr/accurate-token-counting.md`. |
| Resident set is provisional | §3.1's list is hand-picked; phase 1 replaces it with one derived from call frequency. |
| Stickiness scope is open | Promotions are session-sticky. A long-lived process session could churn the LRU; whether to reset promotions per wake is undecided. |
| Sub-agent inheritance is open | Proposed: a delegated leaf does not inherit its parent's promoted set — its role defines its resident set. |
| Large MCP catalogs | One operator MCP server with ~100 tools exceeds the 600-token index budget alone. Collapsing an over-budget server to a single index line is proposed but undesigned. |
| Call-to-load is provider-dependent | A provider that constrains calls to declared functions never triggers it (§3.4); `tool_search` carries those providers. |
| Learned resident sets out of scope | Deriving the resident set per deployment from its own store would make the prompt drift under the operator. Deliberately excluded. |
