# Design note — Durable state and long-running sandboxed tools

**Status:** Proposed (design note) · **Depends on:** the toolvm host, the memory
store, the plugin-job registry and its sweeper · **Follows:** `adr/rich-js-tools.md`,
which names both of these as what it precedes · **Amends:** I-TVM.3, R-TVM.3

Two roadmap items, one idea. A sandboxed tool cannot remember anything and cannot
outlive the turn that called it, and both restrictions come from the same line in
the contract: the wasm instance is destroyed when the call returns. This note
argues that the line is worth keeping exactly as it is, and that both features are
reached by putting the state somewhere the instance never was — **in the host**.
Where in the host is the only real choice, and lifetime decides it: a granted,
quota-bounded store for what should outlive the job, and a row in the job registry
for what should not.

They are designed together because they meet at one question — *what is a tool
allowed to carry from one call to the next?* — and answering it twice would
produce two answers.

---

## 1. What is actually missing

**Durable state.** A tool is instantiated fresh per call and torn down after it,
so nothing survives: not a global, not a cached credential, not a parsed index.
Today the only way to remember is to write a file, and only where `fs.write` was
granted — which means the tool that wants to cache 200 bytes of ETag has to be
handed a directory on the operator's disk. The capability is wildly out of
proportion to the need, so in practice nobody grants it and the tool re-fetches.

**Long-running work.** Every call runs to completion under a wall-clock deadline,
five seconds by default (R-TVM.4). The tool tier is therefore strictly
request/response: no background work, no job that outlives its turn. That work
currently belongs to goals, standing agents, and native plugins — all of which are
whole processes, which is a large thing to become in order to poll a URL for ten
minutes.

---

## 2. The invariant audit

Both features run into **I-TVM.3 — no state survives a call**, and its expanded
form in R-TVM.3: *"Two calls to the same tool MUST NOT be able to observe each
other, and a tool MUST NOT be able to accumulate anything across a session."*

That is the strongest sentence in the toolvm contract, and this note weakens it.
So it is worth being precise about what it currently buys, item by item, before
deciding which items are being spent.

| What I-TVM.3 protects | Under this design |
|---|---|
| No interpreter-state carryover — a poisoned prototype, a patched global, a monkeyed `JSON.parse` | **Preserved, unchanged.** The JS realm is still built from scratch per call. |
| No memory-safety carryover — a half-freed heap, a dangling pointer, a reused allocation | **Preserved, unchanged.** Linear memory is still destroyed per call. |
| No cached credential | **Weakened, bounded.** A tool can cache only what it could already obtain, and only with the `state` grant. |
| No cross-session information flow | **Weakened, and this is the sharp one.** See §3.3. |
| A turn is replayable | **Weakened.** See §8.1. |

The first two are the ones the *sandbox* rests on, and they survive because they
are properties of the instance, not of the contract's wording. What is being
spent is the second two, and they are spent deliberately and under a grant.

The amended invariant:

> **I-TVM.3 (amended)** — No state survives a call **implicitly**. The guest's
> globals, heap, and interpreter realm are destroyed at return, so two calls
> cannot observe each other through the machine. Anything that persists does so
> through a host-owned surface the operator granted, named by the tool, bounded by
> quota, and visible in the audit trail.

The distinction that makes this worth doing is **implicit versus conferred**. The
original invariant conflates "a tool cannot accidentally leak into its next call"
with "a tool cannot deliberately remember". The first is a sandbox property. The
second is a capability, and Nine already has a model for capabilities.

---

## 3. Part A — `state`

### 3.1 The shape

A new capability, `state`, on the same footing as `net.http`: one exported host
function, no wazero primitive behind it, so every check that makes it safe is
Nine's own.

| | |
|---|---|
| Capability | `state` |
| Grant parameters | `scope` (**required**), `max_keys`, `max_value_kb`, `max_total_kb`, `ttl` |
| Default | **none** |
| Enforced by | host fn `nine.state`, backed by a new `tool_state` table |
| Guest surface | the `nine:state` module — `get`, `set`, `delete`, `list`, `swap` |

`state` is one capability, not `state.read` and `state.write`, and that follows
`env` rather than `fs`. A store that a tool may read and never write is not a
reduced capability, it is an empty map — nobody else writes it. Splitting the verb
would produce a grant with no use.

`nine.state` is exported unconditionally and its grant is read per call from the
context, exactly as `hostHTTP` does. The reasoning transfers verbatim: a wasm
module's imports are fixed at compile time and the QuickJS blob is shared by every
`js` tool, so the function must exist for the module to instantiate at all. **What
is shared is the import, not the permission.**

`swap(key, expected, value)` is not a convenience. Two turns calling one tool at
once is ordinary — `moduleConfig` gives each instance an anonymous module name
precisely so that it is — and a read-modify-write across two host calls is racy by
construction. Without a compare-and-set the first non-trivial use of this
capability is a bug the tool author cannot fix.

### 3.2 Not the `kv` table

`memory.Store` already has `Get`/`Set`/`Delete`/`List` over a `kv` table, and
reusing it would be wrong in a way worth recording. `kv` is Nine's own namespace:
no scoping, no quota, no expiry, and no notion of who wrote a row. Admitting tool
writes to it makes every existing `kv` prefix agent-writable, and Nine reads its
own configuration and bookkeeping out of that table. A separate `tool_state` table
with `(tool, scope_key, key)` as its primary key keeps the blast radius at the
tool's own namespace and gives the quota columns somewhere to live.

### 3.3 Scope is the whole security argument

`scope` is a **required** grant parameter with no default, following the precedent
`methods` sets in R-TVM.12 — a parameter this consequential should be a thing the
operator wrote, not a thing they inherited.

| `scope` | Key | For |
|---|---|---|
| `conversation` | `(tool, owner_id)` | a cursor into a feed, a per-thread preference, a rate-limit budget |
| `tool` | `(tool, "")` | a parsed index, a memoized fetch, a content-addressed cache |

**`scope = "tool"` is a cross-session information channel that needs no other
capability.** This is the risk, stated plainly: a tool's arguments come from the
model, and the model's arguments can contain anything in that session's context.
A tool-scoped store lets a call in session A write those arguments down and a call
in session B read them back. No `net.http`, no `fs`, no egress of any kind is
required — the exfiltration is *into another conversation's context*, which is
inside Nine, where none of the network controls look.

That does not make `tool` scope wrong. A cache is a real need and it is the whole
point of the item. It makes `tool` scope a thing an operator confers on a tool
whose code they have read, which is what R-TVM.7 already says about every grant.
The consequence is documented rather than prevented, and `nine tools` shows the
resolved scope alongside the rest.

For the **generated tier** the ceiling carries the scope like any other parameter,
and the recommended posture is `conversation`. A tool-scoped store granted to code
Nine wrote itself is the one mechanism in the design by which the agent could
build itself a memory that the event journal does not record as a message. It
should be a deliberate act.

### 3.4 Quotas, and what a refusal looks like

Per `(tool, scope_key)`: `max_keys` (default 128), `max_value_kb` (default 64),
`max_total_kb` (default 1024). `ttl` is optional and unset by default; when set,
expiry is swept alongside the existing spill sweeper rather than on read, so a
tool cannot keep a value alive by never looking at it.

Exceeding a quota is **an error the guest catches**, never a silent drop. This
follows R-TVM.12's rule for a refused request: letting a policy decision look like
a successful write invites a tool to believe it remembered something it did not,
and the failure would then surface a call later as a cache that never hits.

### 3.5 What it is not

- **Not a filesystem.** Flat keys, string values, no directories, no streaming.
  A tool that needs a file should be granted `fs.write`.
- **Not a channel between tools.** The namespace is keyed by tool name. Two tools
  cannot see each other's state, and there is no shared prefix that would let them.
- **Not reachable by the model.** There is no `state_get` tool. The store exists
  for a tool's own bookkeeping; a model that wants to remember something has the
  memory store, the journal, and its own context.
- **Not audited per operation.** `net.http` audits every call because each one
  reaches the outside world; a `get` reaches a row the tool already owns. What is
  recorded is the grant at load and the quota refusals, not the traffic.

---

## 4. Part B — long-running work

### 4.1 Two shapes rejected first

**Keep the instance alive for the job's duration.** This is the obvious reading of
"long-running tool" and it is the one thing the design cannot do. It breaks
R-TVM.3 outright rather than amending it; it holds up to 16 MiB of resident linear
memory per outstanding job; it reintroduces every carryover §2 just finished
preserving; and it still does not survive a restart, so it fails at the one thing
that distinguishes a job from a slow call.

**Copy the plugin shape: a detached goroutine running the work.** A plugin job
works because a plugin is a process with its own scheduler and its own memory,
and it can be asked `job_status` later. A wasm instance detached onto a goroutine
has neither. Worse, R-TVM.4 is explicit that wazero has no fuel metering and the
wall-clock deadline is *the only CPU bound there is* — enforced by closing the
module out from under the guest. Detach the instance from the deadline and the
tool tier has no CPU bound at all.

### 4.2 The shape: a job is a sequence of ordinary calls

A tool declared `resumable` may end a call with a third result form:

```json
{"ok": true, "continue": {"cursor": "<opaque, ≤64 KiB>",
                          "progress": "41% · 1.2 GB/2.9 GB",
                          "after_ms": 2000}}
```

The host writes `cursor` into the job row, waits `after_ms`, and calls the tool
again with the cursor handed back in its input. The tool picks up where it left
off. Eventually it returns an ordinary `{"ok": true, "output": …}` and the job is
done, or `{"ok": false, …}` and it failed.

The naming is deliberate: there is no new unit here. Each one is **an ordinary
tool call** — same instance model, same deadline, same memory cap, same host
functions, same audit. "Step" is not used for it, because a step is already a
durable unit of delegated work in a workflow (`docs/glossary.md`) and the two
would be confused on sight. `cursor` is used in the same sense a subscription
uses it: a position to resume from.

What this buys, in order of how much it matters:

- **R-TVM.3 holds unchanged per call.** The instance is still created and
  destroyed around each one. No amendment to the instance model is needed at all —
  the amendment in §2 is for Part A, and Part B rides on it without adding to it.
- **The CPU bound survives.** Each call is deadline-bounded exactly as today; the
  guest never runs unsupervised.
- **A job survives a daemon restart** — see §4.4.
- **Cancellation is exact** — see §4.5.
- **No new execution path in the host.** The driver calls `Host.call`, the same
  function `CallOutput` and `EvalGenerated` already go through.

The cost is that the tool must be written to make progress in bounded increments
and to serialize what it needs to resume. That is a real constraint on authors and
it should be stated in `docs/writing-sandboxed-tools.md` as the first thing on the
page, not discovered.

Adding `continue` does **not** bump `ABIVersion`. It follows the precedent
R-TVM.1 sets for `error_detail` and `output_b64`: the two-export contract is
unchanged, the field is additive, and a guest that never emits it produces exactly
the envelope it produced before.

### 4.3 Reuse the job registry, do not build a second one

Nine already has all of this, built for plugins (`docs/plugin-capabilities.md` §5):
a `plugin_jobs` table, a daemon-level sweeper with age-based backoff, cap-or-spill
of the result, a completion notification delivered on the owner's next turn, the
context builder surfacing outstanding jobs as one line each, and four
model-facing tools — `job_wait`, `job_check`, `job_list`, `job_cancel`.

Crucially, the model-facing surface is **already backend-agnostic**:
`agent.JobTools` is an interface implemented in the runtime, and `handle` is
already specified as "stable and unique across plugins". So this feature adds
**zero model-facing tools**. A tool job and a plugin job are the same thing to the
agent, which is correct — the difference is an implementation detail of where the
work runs.

The work is therefore a generalization, not a parallel structure:

| Today | Proposed |
|---|---|
| `plugin_jobs` table, columns `plugin` + `plugin_job_id` | `jobs` table, columns `backend` (`plugin` \| `tool`) + `backend_ref`, plus `cursor` and `calls` |
| `jobSweeper.reconcile` polls `mgr.JobStatus` | `reconcile` dispatches on `backend` to one of two implementations |
| `MarkOrphanedJobsLost` at boot | applies to `backend = "plugin"` only (§4.4) |

Two tables was the alternative and it is worse: `job_list` would union two
queries, a handle would need a backend prefix to stay unambiguous, and the
notification, spill, expiry, and waiter paths would each grow a second copy. The
migration is a numbered step in `migrate.go`, which is what that file is for.

One semantic difference deserves naming rather than glossing. For a plugin the
sweeper **polls work someone else is doing**; for a tool the sweeper **is the
executor**. So `after_ms` is a *requested delay*, not a poll interval, and the
age-based backoff in `PluginJobsDueForPoll` does not apply to tool jobs — backing
off a job whose next call *is* the work would simply make it slower.

### 4.4 Restart survival: the thing the plugin tier cannot do

`MarkOrphanedJobsLost` marks every running plugin job `lost` at boot, because the
work lived as a goroutine inside a process the previous daemon left behind and the
new daemon can never reach it. That is honest but it is a real limitation.

A tool job has no process. Its entire live state is `cursor`, in a row, in SQLite.
So at boot a tool job is **resumed**, not lost: the sweeper finds a non-terminal
row and makes the next call. `lost` becomes a plugin-only state.

This is not a consolation prize for the step shape; it is the strongest argument
for it. "Started, ask me later" is worth much more when *later* can be after a
service restart.

### 4.5 Bounds, cancellation, concurrency

- **Per call**: the ordinary deadline (R-TVM.4), unchanged, including the per-tool
  `[tool.<name>] timeout` override.
- **Per job**: the existing `job_max_seconds` (default 1h) still applies, **plus a
  new `max_calls`** (proposed default 720). This bound is essential rather than
  defensive: without it, `continue` with `after_ms: 0` converts a bounded CPU story
  into an unbounded one, one legal 5-second call at a time. A floor on `after_ms`
  (proposed 250ms) stops a tool busy-looping the sweeper.
- **Cancellation is exact, with one call of latency.** `job_cancel` makes the row
  terminal and the sweeper simply never makes the next call. It cannot interrupt a
  call in flight — that one dies at its own deadline — so worst-case latency is one
  call rather than the plugin tier's best-effort-and-hope.
- **Calls within one job are strictly serial.** There is one cursor; two concurrent
  calls would both read it and one would win. Two *jobs* of the same tool are
  independent and may run at once, as two ordinary calls already may.
- **`max_jobs_per_conversation`** (default 8) already exists and applies unchanged.
  Unlike the plugin tier — where the cap is enforced post-hoc by cancelling a job
  the plugin already started — a tool job can be **refused before the first call**,
  because the daemon decides when the work begins. The awkward admission dance in
  `docs/plugin-capabilities.md` §5 is not needed here.

### 4.6 Who may be resumable

`resumable = true` is a manifest field, not a capability: it confers no reach, so
it sits outside `[capabilities]` alongside `timeout`. R-TVM.10 makes the manifest
authoritative for a tool's shape, and this is shape.

Declaration alone is enough for the **developer** and **shipped** tiers, matching
the precedent that a plugin advertises `async_jobs: true` on its own say-so and is
bounded by the operator's limits rather than gated by an opt-in.

The **generated tier is gated**: `[tools.agent] allow_long_running`, default
false, mirroring `[tools.agent.deps].mode = "off"`. Nine writing itself a tool
that runs for an hour across restarts is a different proposition from Nine writing
itself a date formatter, and it should be the operator's decision.

**`js_eval` refuses `continue` outright.** It persists nothing by definition
(R-TVM.14), and a job is persistence.

---

## 5. Where the two parts meet

They compose but do not depend on each other, and the boundary is lifetime.

- A job's `cursor` is **not** durable state. It lives in the job row, dies with the
  job, needs no capability, and needs no scope reasoning — it is just the return
  value coming back as an argument.
- A resumable tool that wants to remember something **between jobs** declares
  `state` as well, and gets it under the ordinary rules.

Forcing the cursor through the `state` capability was considered and rejected: it
would make every resumable tool require a `state` grant it does not need, and it
would put job-lifetime data under a quota sized for cross-job data. Two lifetimes,
two placements, one principle — the state is in the host, never in the instance.

---

## 6. Config

```toml
# A developer tool that caches, and one that polls.

[tool.geocode.capabilities.state]
scope         = "tool"        # required; "tool" or "conversation"
max_keys      = 512
max_value_kb  = 8
ttl           = "24h"

[tool.site_watch]
timeout = "10s"               # each call; the job is bounded separately
[tool.site_watch.capabilities.state]
scope = "conversation"

[tools]
job_max_calls    = 720        # new: bounds a resumable job's total calls
job_min_delay_ms = 250        # new: floor on the guest's requested after_ms

[tools.agent]
allow_long_running = false    # new: may Nine write itself a resumable tool
[tools.agent.capabilities.state]
scope = "conversation"        # the recommended ceiling posture (§3.3)
```

`job_max_seconds` and `max_jobs_per_conversation` stay where they are, under
`[plugins]`, and apply to both backends. Moving them would be a breaking config
change for a cosmetic gain; the note recommends documenting the shared ownership
instead. `nine tools show` gains the resolved `state` scope and quotas, and a
`resumable` marker.

---

## 7. Phasing

Each phase is shippable and leaves the tree consistent.

1. **`tool_state` + the `state` capability.** Table, host function, grant parsing,
   quota enforcement, `nine:state` in the QuickJS blob, `nine tools` reporting.
   No job machinery. Delivers the durable-state roadmap item on its own.
2. **Generalize `plugin_jobs` → `jobs`.** Migration, `backend` dispatch in the
   sweeper, no behavior change for plugin jobs. Pure refactor, verified by the
   existing plugin-job tests.
3. **The `continue` envelope and the driver.** `resumable` in the manifest,
   `max_calls` and the delay floor, cursor round-tripping, resume-at-boot.
4. **The generated tier.** `allow_long_running`, the ceiling's `state` scope,
   `js_eval` refusal.
5. **Docs.** `docs/sandboxed-tools.md`, `docs/writing-sandboxed-tools.md` (the
   authoring constraint from §4.2 goes at the top), `docs/plugin-capabilities.md`
   §5 becomes backend-neutral where it describes the registry, and
   `docs/glossary.md` gains `resumable tool` and disambiguates *cursor*.

Phase 1 and phases 2–4 are independent; 1 can ship alone if the long-running item
is deferred again.

---

## 8. What this costs

### 8.1 Replay

R-TVM.11 declines to make a new tool visible mid-turn on the grounds that *"a tool
set that mutated mid-turn would make the turn unreplayable, and `adr/event-log.md`
depends on replay."* A stateful tool is a softer version of the same problem: a
replayed turn calling a `state`-granted tool will not necessarily reproduce the
recorded output.

It costs less than it first appears, because of how replay is actually built.
`internal/replay` (`FromEvents` → `Recorded` → `Session`) rebuilds the loop on a
**recorded provider and recorded tool results** — it reproduces the recorded
answers rather than re-executing tools. So `TestRecordThenReplay` and the eval
suite's replay track are unaffected: they never call the tool.

What is genuinely lost is *re-execution* fidelity — running a recorded turn
against live tools and expecting the same result. That was already false for any
tool with `net.http` or `fs.read`, so `state` widens an existing hole rather than
opening one. It should be said out loud in `spec/contracts/event-journal.md`
rather than left implied.

A resumable job is a further wrinkle: a turn that starts a job records the ack,
and the result arrives in a later turn's notification. That is already exactly how
plugin jobs read in the journal, so it needs no new treatment.

### 8.2 The disk

`tool_state` is operator-visible growth in the SQLite file, bounded by
(tools granted `state`) × (scope instances) × `max_total_kb`. With
`scope = "conversation"` the second factor grows with conversation count and
nothing reaps it when a conversation is archived. **Open question:** whether
conversation-scoped state should be deleted with its conversation, or expire on
`ttl` alone. Deleting with the conversation is more obviously correct and is
probably right, but it couples two subsystems that are currently independent.

### 8.3 The sweeper

Tool jobs make the sweeper an executor, so a daemon with 50 outstanding tool jobs
at a 250ms floor is doing real work on the sweeper's cadence. The existing loop is
sequential (`for _, j := range jobs { s.reconcile(ctx, j) }`), which is fine for
polling and not fine for executing. Phase 3 needs a bounded worker pool, and
`max_jobs_per_conversation` alone does not bound the daemon-wide total — a global
cap is a likely addition.

---

## 9. Spec impact

| Requirement | Change |
|---|---|
| **I-TVM.3** | Amended (§2) — implicit carryover, not all carryover |
| **R-TVM.3** | Amended — two calls may observe each other *only* through a granted store |
| **R-TVM.1** | Extended — the `continue` result form; no `ABIVersion` bump |
| **R-TVM.5** | Extended — `state` joins the capability table |
| **R-TVM.7** | Extended — `[tool.<name>.capabilities.state]` |
| **R-TVM.10** | Extended — `resumable` is a manifest field |
| **R-TVM.11** | Extended — `nine tools` reports scope, quotas, and resumability |
| **R-TVM.14** | Extended — `allow_long_running`; `js_eval` refuses `continue` |
| **R-TVM.4** | Unchanged per call; the job-level bounds are new and additive |
| **R-PLUG.7 / I-TVM.2** | **Unchanged and still load-bearing.** `state` is granted by the operator; nothing agent-reachable writes a grant. |
| `spec/contracts/memory-store.md` | New `tool_state` table; `plugin_jobs` → `jobs` |
| `spec/contracts/event-journal.md` | Say plainly what re-execution fidelity means now (§8.1) |

R-TVM.13 ("fully built") stops being true and should be restated as built-through
a named set rather than deleted.

---

## 10. Open questions

1. **Conversation-scoped state and conversation deletion** (§8.2). Leaning toward
   deleting with the conversation.
2. **A daemon-wide cap on outstanding tool jobs** (§8.3). Almost certainly needed;
   the number is the question.
3. **Should `state` be visible to the operator?** A `nine tools state <name>`
   read-only dump would make a misbehaving cache diagnosable. It is also a way for
   a human to read whatever a tool-scoped store accumulated across sessions, which
   may be the point or may be a surprise.
4. **`ttl` semantics on `swap`.** Whether a successful swap refreshes the
   expiry. Refreshing makes a heartbeat trivial; not refreshing makes expiry mean
   what it says.
