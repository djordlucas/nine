# Design note — Standing tools

**Status:** **Implemented** — `R-TVM.20`, all five phases; driven by `runtime.StandingRunner`,
wired at `cmd/nine/daemon.go:299-313`. Open questions §12.1 and §12.3 are answered; see
`adr/tool-facilities.md`. · **Depends on:** `adr/durable-and-long-running-tools.md`
(Parts A and B) · **Related:** standing agents, goal sessions, scheduling ·
**Amends:** nothing — see §1

A **standing tool** is a sandboxed tool that runs indefinitely on its own cadence,
started by configuration or by an operator-approved generation rather than by a
turn. It does deterministic background work — watching, scraping, indexing,
processing — under the same sandbox, the same capability grants, and the same
bounds as every other tool, with no LLM in the loop.

It is deliberately **not a new kind of tool**. It is a second way to *run* the
resumable tool that `adr/durable-and-long-running-tools.md` Part B already
defines. That is the whole point: long-running work in Nine should be one
standardized thing with one set of rules, one isolation story, and one place to
look at it.

---

## 1. Why this needs no amendment to Parts A and B

Worth stating first, because it is the load-bearing claim.

Part B defines two separable things: a **mechanism** — the `continue` envelope,
the cursor, the driver that calls a tool, persists what it returns, waits, and
calls it again — and a **lifecycle**, the job: owned by a conversation, started by
the model mid-turn, terminating in an `output`, bounded by `max_calls` and
`job_max_seconds`.

Every bound Part B specifies is attached to the job lifecycle, not to the tool.
The manifest field is `resumable`; the config keys are `job_max_calls`,
`job_max_seconds`, `max_jobs_per_conversation`. Nothing in the envelope, the
cursor, or the driver assumes a run ever ends or that it has a conversation
behind it.

So a standing run is the same mechanism under a different lifecycle:

| | Job (Part B) | Standing run |
|---|---|---|
| Started by | the model, mid-turn | config at boot, or an approved generation |
| Owner | the conversation (`owner_id`) | the operator |
| Ends | when the tool returns `output` | when stopped |
| Bounds | `max_calls`, `job_max_seconds` | health limits (§5), not a call budget |
| Result | delivered as a notification | side effects, and what it writes (§6) |

Same manifest field, same envelope, same driver, same sandbox, same capability
resolution, same per-call deadline. A tool author writes one kind of resumable
tool and the operator decides how it runs.

---

## 2. Why not a standing agent

Nine already has standing agents, and for most recurring work they are the right
answer. This note should be read as claiming a narrow gap, not a general one.

**The composition that already works.** A standing agent on an `interval` calls a
sandboxed tool that does the deterministic part, and decides what to do with the
result. The expensive tier does judgment; the cheap tier does scanning. That is
correct layering and it needs nothing new.

**Where it stops working** is when the useful cadence is much faster than an
agent's thinking time *and* the overwhelming majority of ticks are no-ops. Then
every tick spends a full LLM turn to be told nothing happened. That is not a
tuning problem, it is a shape problem: the work is deterministic and the judgment
is only occasionally needed.

The other half of the gap is **cost-free continuous processing**. "Index this
corpus as it grows", "keep this derived file current", "process this stream for
an ongoing research effort" — work with no decision in it at all, that should not
consult a model, and that today has no home except a native plugin (a whole
process with the daemon's uid) or a cron script outside Nine (the user's full
uid, none of the capability model, invisible to `nine tools`).

That last comparison is the argument. R-TVM.16 made the same one for shipped
tools: before it, Nine's own built-in capabilities were plugins, "so a tool that
read a clock had, in principle, the reach to read the operator's home directory."
A standing tool is strictly safer than either alternative, and unlike both it is
visible, bounded, and subject to the capability model.

**What this does not do.** A standing tool has no judgment, calls no model, and
cannot delegate. Work needing a decision belongs to a standing agent, and a
standing tool that finds something hands off to one (§6) rather than deciding
itself.

---

## 3. Two provenances

### 3.1 Declared in configuration

```toml
[[standing_tool]]
tool     = "corpus_index"     # a loaded resumable tool
id       = "corpus"           # stable, operator-chosen; reconciliation keys on it
interval = "10s"              # or schedule = "*/5 * * * *"
args     = { root = "/srv/corpus" }
enabled  = true
```

The shape follows `[[agent]]` deliberately — an operator who has declared a
standing agent should recognize this immediately. `interval` and `schedule` are
mutually exclusive and reuse the existing trigger parsing (`docs/scheduling.md`),
including its stated limits: no backfill, minute granularity for cron.

`args` is the tool's input on its first call. Subsequent calls receive the cursor
the tool returned, exactly as a job does.

### 3.2 Generated by Nine

An operator asks Nine, in a session, to write a tool that keeps doing something —
"generate a tool that processes incoming papers for this research and keeps the
summary file current." That is `tool_write` (R-TVM.14) plus a request to run the
result standing.

This is the flavor that needs controls, because it is the one where the code and
the decision to run it forever both originate inside a turn.

- **`[tools.agent] allow_standing`**, default **false**. Mirrors
  `[tools.agent.deps].mode = "off"`: the capability exists, off, until an operator
  turns it on.
- **Promotion always gates through HITL**, regardless of
  `[tools.agent].require_approval` — including `never`. This is the one place that
  setting does not mean what it says, and the exception is justified: `never` says
  "the ceiling is the only control", and the ceiling bounds *reach*, not
  *duration*. A capability-free tool that runs forever is inert per call and
  unbounded in aggregate, which is precisely the case the ceiling cannot express.
  The approval prompt names the tool, its cadence, its declared capabilities, and
  that it will run until stopped.
- **`[tools.agent] max_standing`** (default 4) caps how many may exist at once, on
  the same reasoning as `max_tools`: this is a resource an agent can accumulate.
- **Auto-disable on repeated failure** (§5), which config-declared tools do not
  get. An operator watching their own declaration is a different situation from
  Nine having written something nobody has read since the approval.

> **The invariant is unchanged.** I-TVM.2 and R-PLUG.7 hold exactly as before.
> A generated standing tool is granted through the ceiling like any other
> generated tool; `allow_standing` is written by the operator, the approval is
> given by a human, and nothing agent-reachable writes a grant. What is new is
> that Nine may ask to run something indefinitely — and asking is all it can do.

---

## 4. Lifecycle and ownership

The split that standing agents already settled applies unchanged, and it is worth
reusing rather than re-deciding: **configuration owns the definition, the runtime
owns the run state.**

| | Owner | On restart |
|---|---|---|
| tool, id, trigger, args | **config** | reconciled in place |
| running / stopped / failing | **the runtime** | restored, never overridden |

So a standing tool an operator stopped stays stopped across a restart, the way a
finished standing agent stays finished. Removing the config entry stops Nine
reconciling it; it does not resurrect or rewrite the run state. A generated
standing tool has no config entry, so its definition lives in the store beside
the tool.

**Starting.** Config-declared tools start at boot after tool loading, since a
standing run needs its tool to have loaded successfully. A generated one starts
when approved — next-turn visibility (R-TVM.11) does not apply, because the
daemon, not an agent loop, is what runs it.

**Stopping is exact**, reusing Part B's cancellation: the driver simply does not
schedule the next call. A call in flight dies at its own deadline, so worst-case
latency is one call. There is no partial-teardown state to reason about, because
there is nothing resident to tear down.

**Restart resumes**, for the reason Part B §4.4 gives: the entire live state is a
cursor in a row. A standing run is never `lost`.

---

## 5. Health, and the failure that matters

The likely bug in this feature is not a crash. It is a standing tool that throws
on every call for a week while nobody notices.

- **Consecutive failures back off** exponentially from the configured cadence to a
  ceiling, so a broken tool stops burning the cadence it asked for.
- **A circuit breaker** moves it to `failing` after N consecutive failures. The
  state is visible in `nine tools` and carries the last error.
- **A generated standing tool auto-disables** at that point. A config-declared one
  keeps retrying at the backed-off cadence, because an operator's declaration is a
  standing instruction and silently disabling it would be the more surprising
  behavior.
- **Transitions notify**, once per transition rather than per failure: entering
  `failing` and recovering out of it each post to the human feed
  (`UserNotificationCreate`, `nine notifications`). A tool flapping does not
  produce a notification storm because only the edges are reported.

Resource bounds are unchanged and are already sufficient per call: the wall-clock
deadline (R-TVM.4) still applies to every call, including the per-tool
`[tool.<name>] timeout` override, and memory is capped as ever. What a standing
run adds is aggregate, not per-call, so the controls are the cadence floor
(reusing Part B's `job_min_delay_ms`) and a daemon-wide cap on concurrent standing
runs.

---

## 6. What it produces, and how Nine finds out

A standing tool has three ways to be useful, and they are deliberately ordered
from least to most coupled.

1. **Side effects within its grants.** It writes a file where `fs.write` was
   granted. Nine learns nothing, and for a great many cases — keep this derived
   file current — that is the entire requirement.
2. **Its own durable state** (Part A). Bookkeeping across calls: what it has
   already seen, where it is in a corpus. `scope = "tool"` is the natural setting,
   since a standing run is the tool's only caller.
3. **A note for a later turn.** When it finds something a human or an agent should
   act on, it posts to the human feed, or to a named owner's notifications.

Point 3 is the one that needs a rule, and the rule already exists.
`adr/reactive-events.md` draws the line at **enrich, don't interject**, and
R-SUB.3 makes it mechanical: a background reaction may write derived stores,
including the notification feed, and may **not** enqueue or wake a turn. A
standing tool sits exactly there. It leaves a note; the note is read on the
owner's next turn, which for a standing agent arrives on its own cadence and for
an interactive conversation arrives with the human's next message.

**A standing tool cannot wake anything.** If the desired behavior is "check often
and act immediately when true", that is not this feature — that is a
condition-based wake trigger for a standing *agent*, which Nine does not have
(`docs/scheduling.md` has only `interval` and `schedule`). It is a good idea and a
separate one; conflating them would put a push channel inside the tool tier,
which is the door §2 of `docs/plugin-capabilities.md` deliberately closed.

The owner of a note is named in the declaration — a standing agent id, or the
human feed by default. Defaulting to the human feed is the safe direction: a note
nobody reads is a wasted line, but a note delivered into an agent's context that
the operator did not intend is a change in that agent's behavior.

---

## 7. Observability

An operator must be able to answer "what is this thing doing" without reading the
daemon log, and this is the surface the feature lives or dies on.

### 7.1 What is and is not a journal event

**A standing run's own calls are not journal events.** A tool on a 10s cadence is
8,640 calls a day; writing each to `session_events` would swamp the journal,
distort the retention scrub, and pollute what `nine trace` exists to show. What is
journaled is **transitions and outputs**: started, stopped, entering and leaving
`failing`, and any call that produced a note (§6.3). Volume is proportional to
things happening, not to time passing.

Per-call detail lives in **counters on the run's row** — total calls, failures,
consecutive failures, last call, next call, last error, current `progress` — and
in a **bounded ring buffer of recent calls** for `nine tool logs`.

### 7.2 HTTP is audited normally

R-TVM.12's audit requirement is **unamended**. Every `net.http` call a standing
tool makes is recorded with its tool, method, host, status, bytes, truncation and
duration, whatever its outcome — exactly like every other tool.

Aggregating the audit for standing runs was considered and rejected. Most standing
tools make no HTTP calls at all; the ones that do are precisely the ones whose
egress an operator most wants a record of, and a security MUST that weakens under
load is not a MUST. A high-frequency HTTP standing tool will produce journal
volume proportional to its egress, which is the correct relationship, and the
retention scrub (`SessionEventsScrub`) already bounds journal growth.

This leaves a clean rule: **a standing tool's own heartbeat is not an event;
anything it does that reaches the world is.**

---

## 8. Control surface

### 8.1 CLI

```
nine tools                          # roster gains a standing section with state
nine tools show <name>              # gains cadence, run state, counters
nine tool status <id>               # one run: state, uptime, calls, failures,
                                    #   last error, next call, progress
nine tool logs <id> [-n N]          # recent activity from the ring buffer
nine tool stop <id>                 # exact; the next call is not scheduled
nine tool start <id>                # resume a stopped run
nine tool call <name> '<json>'      # one call, for testing — see below
```

This follows the existing split: `nine tools` is the subsystem and the roster,
`nine tool` acts on one thing (`nine tool validate` is already there).

### 8.2 `nine tool call` — testing, and the trap in it

Invoking a tool once from the CLI is the missing operator affordance, and it is
useful well beyond this feature: it is how you check what a tool does before
declaring it standing, and how anyone debugging a tool sees a raw result.

It prints the **whole envelope**, including a `continue` a resumable tool returns
— the cursor, the progress line, and the requested delay — because that is exactly
what is being debugged.

**It runs against isolated state by default.** A test call sharing the live run's
Part-A store could overwrite the cursor of a running standing tool, which would be
a genuinely nasty surprise: the operator "just tested it" and the production run
skipped a day of work. So the default is a scratch namespace discarded at exit,
with `--live-state` as an explicit opt-in for the case where reproducing a bug
requires the real store. Capabilities are the tool's real resolved grant either
way — the isolation is of *state*, not of *reach*, since a test that cannot make
the tool's actual HTTP calls tests nothing.

### 8.3 TUI

`/tools` gains the standing section; `/tool <id>` shows one run; `/tool stop <id>`
and `/tool start <id>` mirror the CLI. This follows the existing slash-command
surface (`adr/tui-slash-suggestions.md`), which already exposes tools read-only —
the addition here is the two lifecycle verbs.

---

## 9. Naming

**Standing tool**, parallel to standing agent: permanently constituted, running
without a turn behind it.

**"Persistent" was considered and rejected**, for both this and a possible rename
of standing agents. In Nine, "persistent" already means *stored* — the glossary
uses it of goals, workflows, session plans, and the store itself. Every standing
agent is persistent, as is every goal; the word does not discriminate. Worse, for
tools it collides directly with Part A: a "persistent tool" would read as a tool
with durable state, which is a different feature in the same design.

**"Service" was rejected** because a wasm module cannot accept a request — there
is no socket and no listener, by construction (R-TVM.5) — and a name promising an
inbound interface would be a standing invitation to look for one.

A run of a standing tool is a **standing run**, distinct from a **job** (Part B).
Both are runs of a resumable tool.

---

## 10. Spec impact

| Requirement | Change |
|---|---|
| **R-TVM.12** | **Unchanged.** Every HTTP call audited normally (§7.2) |
| **R-TVM.3 / I-TVM.3** | Unchanged beyond Part A's amendment — a standing run is a sequence of ordinary calls |
| **R-TVM.4** | Unchanged per call; cadence floor and a concurrency cap are new and aggregate |
| **R-TVM.11** | Extended — the roster reports standing runs; next-turn visibility does not apply, since the daemon runs them |
| **R-TVM.14** | Extended — `allow_standing`, `max_standing`, mandatory HITL on promotion |
| **R-SUB.3** | **Unchanged and load-bearing** — a standing tool may write derived stores and may not wake a turn (§6) |
| **I-TVM.2 / R-PLUG.7** | **Unchanged.** Nine may ask to run something indefinitely; it cannot grant itself anything |
| `spec/contracts/toolvm.md` | New requirement for the standing lifecycle |
| `spec/contracts/wire-protocol.md` | Status/stop/start/call messages |
| `docs/scheduling.md` | Trigger parsing gains a second consumer |
| `docs/glossary.md` | *standing tool*, *standing run*; *job* clarified against both |

---

## 11. Phasing

Assumes Parts A and B have landed; the driver and the cursor come from B.

1. **The standing lifecycle.** Registry rows, the config block, reconciliation at
   boot, the run/stop state split, restart resume. Config-declared only.
2. **Health.** Backoff, circuit breaker, `failing`, transition notifications.
3. **Observability.** Counters, ring buffer, the journal rule, `nine tools`
   reporting, `nine tool status` / `logs`.
4. **Control.** `nine tool stop` / `start`, `nine tool call`, and the TUI slash
   commands.
5. **The generated flavor.** `allow_standing`, `max_standing`, the mandatory HITL
   gate, auto-disable.

Phase 5 last, deliberately: everything an operator needs to *watch and stop* a
standing tool should exist before Nine is allowed to create one.

---

## 12. Open questions

1. **The daemon-wide cap on concurrent standing runs.** Needed; the number is the
   question, and it interacts with the same cap Part B §8.3 raises for jobs.
   Probably one shared budget rather than two.
2. **Does a stopped generated standing tool keep its row?** Keeping it makes the
   history legible and lets an operator restart it; dropping it keeps the catalog
   honest about what is live. Leaning toward keeping, with `nine tools` marking it
   stopped.
3. **Should a standing tool be able to name a standing agent as its note owner at
   all**, or should everything go to the human feed in v1? Agent-owned notes are
   more useful and are also the path by which a standing tool changes an agent's
   behavior without a human in between.
4. **`args` on reconfiguration.** Changing `args` in config for a running standing
   tool — does the cursor survive? Probably not: the cursor was produced under the
   old arguments and resuming with it would be incoherent. Restarting the run on
   an `args` change is the safer default.
