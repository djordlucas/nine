# Design note — The generated-tool authoring loop

**Status:** Proposed (design note) · **Depends on:** the toolvm host
(`internal/toolvm/host.go`), `CheckGenerated` (`internal/toolvm/generated.go`),
the QuickJS harness (`internal/toolvm/quickjs/`), the `tools` table
(`spec/contracts/memory-store.md`) · **Follows:** `adr/durable-and-long-running-tools.md`,
which closed the state and duration gaps · **Amends:** none — every proposal here
works inside R-TVM.14 as written

> **Since this note was written (2026-09-23).** **W3 has shipped** — a failing call now
> carries the tail of what it printed, bounded per call, failure-only; `docs/` and
> `spec/` describe it and this section stays as the record of why. Two premises below
> have also gone stale, and are left in place rather than rewritten: §1's item 4 says
> QuickJS ships no `crypto`, but `crypto.getRandomValues` and `crypto.randomUUID` landed
> with `adr/rich-js-tools.md`, so **W4's `nine:uuid` half is already covered** and only
> `nine:hash` is a live gap. W1, W2 and W5 are unbuilt as described.

A generated tool is written blind. `tool_write` persists the row, the next turn
advertises the tool, and only the first real call reveals whether the code
parses, whether it reads the arguments it declared, and what it printed on the
way to failing. This note proposes five changes that shorten that loop without
touching the capability model, the ceiling, or the trust boundary: a parse-only
check at write time, a dry-run path, logs attached to failures, two always-on
stdlib modules, and catalog hygiene surfaced to the agent.

All five share one motivation: Nine is developed against 4–9B models, and a small
model that is told its write succeeded will believe it and spend turns
discovering otherwise.

---

## 1. What the model cannot see today

`tool_write` validates a proposal before persisting it — name shape, ceiling
fit, schema shape, collisions (`Host.CheckGenerated`). What it does not do is
look at the source. The comment at `internal/toolvm/generated.go:191` says why:

> Compiling is not part of this: a `js` tool is never compiled at install time,
> and the interpreter reports a syntax error at first call. Validating the
> source would mean running it, which is exactly what a write must not do.

The invariant is right; the inference is not. Parsing is not running, and the
QuickJS blob the host already ships can tell them apart (`JS_Eval` with
`JS_EVAL_FLAG_COMPILE_ONLY`). The gap is the difference between the two halves
of that comment: the refusal to execute at write time is load-bearing, and the
conclusion that nothing can therefore be checked does not follow.

The consequences, in the order a small model meets them:

| # | What happens | What the model sees |
|---|---|---|
| 1 | A syntax error is written and registered | Success. The tool is advertised next turn and fails on every call. |
| 2 | The first real call is the first execution | Whatever the tool does when its author was never able to run it once. |
| 3 | A failing tool returns only its thrown error | No intermediate values, no printed state — the model guesses at its own code. |
| 4 | QuickJS ships no `crypto` (`tools.d/README.md` names it absent) | A sha256 or UUID gap that no capability can close — it is pure transform over the arguments. |
| 5 | The catalog accumulates single-use tools (the R-TVM.14 cap exists for this) | Nothing. The model cannot see its own catalog's health. |

Items 1–3 are one loop. Item 4 is the stdlib. Item 5 is hygiene.

---

## 2. W1 — Parse at write time

`tool_write` refuses a source that does not parse, with the interpreter's
position in the refusal, before any row is written.

**Mechanics.** The QuickJS blob is an ordinary wasm tool whose input is the
envelope (`internal/toolvm/js.go`): harness, modules, args. The harness runs
`JS_Eval` on the tool module. A parse-only variant is the same host path with a
flag: the harness compiles the module and returns before execution. The host
already owns the blob (`Host.qjs`) and already compiles it once at `Open`
(`host.go:202`), so the check adds one instantiation per write, not a rebuild
of anything. Errors carry QuickJS's own line/column, which is what a model
needs to fix the source in one turn.

**What it does not do.** It does not execute: top-level statements of the tool
module never run, so the write still cannot have side effects. It does not
catch runtime errors — a `TypeError` on first call is still the model's to
discover — and it must not pretend to. A parse pass is a floor, not a review.

**Contract.** `CheckGenerated` gains a parse step; R-TVM.14's "a refusal leaves
no row behind" is unchanged. The refusal is an ordinary `tool_write` error the
model can act on, like the ceiling refusals it already returns.

**Cost.** Small. One host function on the qjs blob's import surface (the blob
is rebuilt on a deliberate bump, per §10.1 of `docs/sandboxed-tools.md`), one
flag in the envelope, one call in `CheckGenerated`, tests. The blob's committed
hash changes — the bump PR is where that is reviewed, same as any other.

---

## 3. W2 — Dry-run against real arguments

A `tool_test(name, args)` core-intercepted tool runs a registered tool against
caller-supplied example arguments and returns the result without the model
having to spend a turn calling the tool for real.

The loop this closes: today the first real call is the first execution, so the
write→call→discover-it's-broken→rewrite cycle costs a turn per iteration. With
a dry-run the model repairs its tool inside a single turn, then calls it for
real. `js_eval` is not a substitute: it runs a snippet under the tool's rules
but not against the registered name, schema, and grant path — the exact surface
the model needs to exercise is the one it does not reach.

**Rules.** It inherits the tier's controls wholesale rather than inventing its
own:

- The call runs under the *tool's own* grant, resolved by name from the store —
  never a grant the caller supplies. The ceiling bounds it as always.
- It routes through the same HITL gate as the tool (`require_approval` keys on
  the *tool's* declaration, so a capability-free dry-run stays ungated).
- It is subject to the same bounds (R-TVM.4): the dry-run gets the tool's own
  `timeout` and memory, not the caller's patience.
- It is journaled like any call (R-TVM.12): a dry-run is an execution of the
  tool's code, and the audit trail must say so.

**What it is not.** It is not a test *tier* — no assertions, no fixtures, no
state. It is one call with arguments the model chose, answered immediately.

---

## 4. W3 — Logs attached to failure

A tool that fails returns its thrown error today, and nothing else. Everything
it printed through `console` on the way to failing is gone.

**Mechanics.** `console.log` already funnels through the single host import
`nine.log` (`harness.js:133–152`); the host writes it to the daemon log. W3
buffers the same lines per call — in the host, keyed by call context — and
attaches the last N (N small, e.g. 8) to the error result, truncated to the
output budget. A success keeps its current shape: logs on success are noise
the model did not ask for.

**What it buys.** For a 4B model, "here are the six values printed before the
throw" is often the difference between a one-turn repair and thrashing. The
cost is one bounded buffer and one field on the failure result.

**What it is not.** It is not a new capability and confers nothing: the tool
already printed those lines through a granted-by-default import; W3 only stops
throwing them away when they turn out to matter. Secrets are a real concern —
a tool may print a fetched credential — so the buffer is attached to *failure*
only, never persisted beyond the call, and inherits whatever redaction the
journal already applies.

---

## 5. W4 — `nine:hash` and `nine:uuid`

Two gaps the stdlib cannot express today, both pure transforms over the
arguments, both always-on like `clock` and `randomness`:

- **`nine:hash`** — sha256/hmac over a string or bytes. QuickJS ships no
  `crypto`; today a generated tool cannot compute a checksum, and the workaround
  (a JS sha256 implementation inlined into the source) bloats the row and the
  context for no security benefit — the algorithm is public, the input is the
  tool's own.
- **`nine:uuid`** — v4 UUIDs from the granted-by-default `randomness`.

Both are in-process computation, leak nothing, and add no host import: the
harness implements them in JS over `__nine_random`, or the blob gains a host
function if that is cheaper than the JS-side implementation.

An HTML/XML parser is the same shape and a larger module — fetching works
today, parsing what comes back means regex. It is deliberately out of scope
here; a parser is a dependency surface (entity handling, nesting depth,
memory), and it belongs in the `deps` tier's company, not the always-on
stdlib's.

---

## 6. W5 — Catalog hygiene surfaced to the agent

R-TVM.14 caps the catalog at 64 with LRU eviction on last-called-at, and keeps
usage counters across rewrites. The counters exist for the cap; the model never
sees them.

**Proposal.** The `tool_write` result gains a short hygiene note when the write
succeeds: names of generated tools that have never been called, and names of
tools whose recent calls consistently fail. The model can then `tool_delete`
its own dead tools instead of waiting for the LRU to silently evict them.

This is read-only surfacing of data the store already holds. It adds no
capability, no tool, no new state — and it directly defends the thing the cap
exists to defend: the tool-selection budget shared with the built-ins.

**What it is not.** It is not auto-eviction by failure rate (the LRU stays the
only evicter) and it is not a judgment about which tools are *good* — a
consistently failing tool may be failing for reasons outside its source, and
the note says what was observed, not what to do.

---

## 7. Sequencing

| Order | Item | Why here |
|---|---|---|
| 1 | W1 — parse at write time | Highest value per line; the refusal is the single biggest turn-waster it removes. Needs a blob bump. |
| 2 | W3 — logs on failure | Same failure mode as W1, pure host-side, no blob change. |
| 3 | W2 — dry-run | The loop-shortener; new tool registration, contracts touch (R-TVM.14 wording). |
| 4 | W4 — stdlib | Independent; a blob bump can carry W1 and W4 together. |
| 5 | W5 — hygiene note | Smallest; rides along whenever `tool_write` is next touched. |

W1+W4 share one blob rebuild; W2 and W3 are host-and-contract work with no
blob change; W5 is a result-field. Nothing here amends the toolvm contract —
W1 strengthens a refusal R-TVM.14 already specifies, W2 adds a core tool the
dispatcher already knows how to carry, and W3–W5 touch nothing the contract
names.

---

## 8. What is deliberately not here

- **No execution at write time.** The line `generated.go:191` draws — a write
  must not run the code — is correct and stays. W1 stops at parsing; W2 puts
  execution behind an explicit, gated, journaled call that already exists in
  shape (`js_eval`), not behind the write.
- **No new capabilities.** Nothing here reaches outside the sandbox. W3
  surfaces what `nine.log` already carried; W4 is computation; W5 is a read of
  the store.
- **No `wasm`-kind writes.** R-TVM.14's "kind is always `js`" is untouched —
  a binary blob is not reviewable, and the compiled-tier question
  (compiler-as-plugin) is a separate proposal if it is ever wanted.
- **No test framework.** `tool_test` returns a result, not a verdict. If the
  model wants assertions it can wrap them in its own tool and read the output.

---

## 9. Limits

| Limit | Detail |
|-------|--------|
| W1 catches syntax only | Runtime errors — the `TypeError` on line 40, the undefined property — still surface on first call. W2 is the answer for those, and it costs a turn to use. |
| W2 costs a call | A dry-run is a tool call with the tool's own bounds and journaling; it is cheaper than a broken real call, not free. |
| W3 leaks what the tool printed | A tool that prints secrets has those secrets in its own failure result. The buffer is failure-only, per-call, never persisted, and the authoring guidance gains a line saying not to print credentials. |
| W4 is a fixed menu | `nine:hash` and `nine:uuid` answer the two most frequent gaps, not the general case. Anything larger (parsers, compression, images) belongs in the `deps` tier, which is off by default and allowlisted. |
| W5 reports, it does not act | The note names dead and failing tools; deletion stays the model's call and the LRU stays the only evicter. |
| Nothing here helps a tool that parses, runs, and returns wrong answers | A parse check and a dry-run verify shape, not correctness. The eval harness and the model's own judgment remain the only check on semantics. |
