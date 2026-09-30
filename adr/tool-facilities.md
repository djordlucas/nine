# Sandboxed tool credentials, progress and reactions

- **Status:** Proposed (design note). Nothing here is built.
- **Date:** 2026-09-29.
- **Scope:** `[tool.<name>.capabilities.net.http.auth]` and its `methods`
  allowlist, a `nine:progress` stdlib module and its host import, `[[reaction]]`
  with `notify`, a `reactions_get` tool and its pending-count context line,
  `[tools.agent] allow_reactions`, `skills/tool-authoring.md` and a new
  `skills/tool-reactions.md`.
- **Depends on:** the toolvm host, `net.http` and its audit, the session journal
  and its subscriptions, the standing-run driver.
- **Follows:** `adr/rich-js-tools.md`, `adr/durable-and-long-running-tools.md`,
  `adr/standing-tools.md`.
- **Rejects:** tool→tool dispatch, and with it MCP reach — §7.

Four additions, none of which widens what a sandboxed tool can reach. Credentials
give a tool less than the `env` capability would, because it authenticates without
holding the secret. A wider method allowlist changes which verb a granted request
carries, not which address it reaches. Progress gives a new destination for text the
tool already produces. Reactions give a new trigger for a run mode that exists. No
row is added to the R-TVM.5 capability table and the sandbox boundary is unchanged in
every case.

| | Gap today | Shape | New capability |
|---|---|---|---|
| **Credentials** (§3) | a granted tool reaches an API and cannot authenticate to it | a parameter on the existing `net.http` grant; the host injects, the guest never sees the value | no — a grant parameter |
| **Methods** (§4) | no wildcard, and the WebDAV family is ungrantable | the WebDAV verbs added and `methods = ["*"]`, with CONNECT and TRACE always refused | no — a grant parameter |
| **Progress** (§5) | a tool is silent for the whole of a call, however long | a host import writing to the rail `EmitProgress` already provides | no — unconditional, like `log` |
| **Reactions** (§6) | reacting to the journal requires writing Go and rebuilding; a finding reaches no agent | a third run mode triggered by a journal event, delivered as a count plus `reactions_get` | no — a run mode |

Credentials and progress both write tool-influenced text into the journal, so §2
maps that leak surface once and both build on it.

---

## 1. Why credentials come first

`internal/config/config.go:500-504` is the whole of the `net.http` grant —
`allow_hosts`, `methods`, `max_bytes` — and has nowhere to put a token.
`internal/config/config.go:1407` refuses any env key matching `NINE_*` or
`*_API_KEY`, because the daemon's environment holds the LLM provider keys.

Both decisions stand. Their combination means a tool can be granted
`api.github.com` and can do nothing with it: every authenticated API is out of
reach for the entire sandboxed tier, and the only workaround is a native plugin
running as the daemon's uid with none of the checks in `ssrf.go`. The capability
model currently pushes work out of the sandbox.

---

## 2. The journal leak surface

The journal is append-only (R-EVT.1), so a leaked secret is not correctable by a
later pass — only the retention scrub removes it, by deleting the whole event. That
scrub is bounded by turn count, not by time: `event_retention_days` defaults to no
age limit (`config.go:807-810`), so on a quiet instance a journalled secret has no
expiry at all. `nine trace` reads it meanwhile.

| Site | What lands there | Today |
|---|---|---|
| `nethttp.go:232` | `call.URL = req.URL` — the guest's requested URL, recorded before any check runs | unredacted by construction, so a refused attempt audits faithfully |
| `nethttp.go:318` | `call.URL = resp.Request.URL.Redacted()` — the post-redirect URL | `Redacted()` covers userinfo only, not query parameters |
| `journal.go:163-177` | `toolHTTPPayload{… URL …}` as a `tool_http` event | persisted |
| `nethttp.go:387` | the unconditional `slog.Info("sandboxed tool http", … "url", c.URL …)` | written to the daemon log, which ships |
| `journalToolEnd` | the tool's returned `Output` | persisted |
| `host.go:339` | `nine.log` lines via `logFor(ctx).add` | bounded ring, carried back to the model on failure |

Headers are audited nowhere. §3 places credentials in headers for that reason.

### Redaction by exact value, not by pattern

A matcher for things resembling secrets has silent false negatives and false
positives that corrupt an audit record. It is also unnecessary: the host performs
the injection, so it holds the exact byte string and knows where it placed it.
Redaction removes a value the host wrote rather than recognising one it found,
which makes the no-leak property checkable.

Each call carries the secret values resolved for it. The audit path replaces exact
occurrences with `«redacted:<header-name>»` before anything is logged, journaled,
or handed to `HTTPAuditFn`. The pass covers the tool's returned envelope and its
`nine.log` lines as well as the audit record, because an API that echoes a token in
its response body — OAuth refresh responses do — otherwise reaches the journal
through `tool_end.Output`. Exact substring search over a bounded output is cheap.

A tool that returns a *derivative* of a secret — a signed URL, a cookie minted from
the token, a re-encoding — leaks it, because the host cannot recognise what it did
not write. The mitigation is the existing one: the operator reads the source before
conferring the grant, and `nine tools show` prints it. That is the trust boundary
`scope = "tool"` state already sits on.

---

## 3. Host-injected credentials

### Auth is a grant parameter

The tool declares `net: ["http"]` and nothing more. Auth attaches to the operator's
grant:

```toml
[tool.gh-issues.capabilities.net.http]
allow_hosts = ["api.github.com"]
methods     = ["GET", "POST"]

  # header name → where the value comes from. The value never enters the guest.
  [tool.gh-issues.capabilities.net.http.auth]
  Authorization = { env = "GH_TOKEN", prefix = "Bearer " }
```

Three properties follow from putting it at grant level rather than making it a
capability:

1. **R-TVM.6 resolves correctly with no manifest change.** `resolveGrant` matches
   declaration against grant at capability granularity and refuses either
   direction of mismatch. `auth` is a parameter, like `allow_hosts` and `methods`,
   and `capability.go` already states that the declaration contributes no
   parameters. An operator adding `auth` to an existing grant triggers no mismatch.
   A tool that declares `net: ["http"]` has already declared everything it can do
   with a credential, so a separate declaration would add no information to review.
2. **`env` stays closed.** This path reads the environment in the daemon and writes
   a header on the wire; it does not read env into the guest. `GH_TOKEN` remains
   refused as an `env` grant, and is usable here because the tool cannot see it.
3. **The credential's reach is the host allowlist.** A credential is sent only to a
   host that passed gate 1 and an address that passed gate 2 (R-TVM.12). A tool
   cannot retarget its own token.

`prefix` keeps the scheme string in the operator's hands. Without it the tool
composes `"Bearer " + secret` and therefore holds the secret.

### Headers only

Query-parameter auth (`?api_key=…`) is out of scope. §2's table is the reason: the
URL is journaled at three sites and a header at none, so query placement would make
the redaction pass the only barrier for the common case rather than a backstop for
the rare one.

Some APIs accept nothing else, and for those the answer is a native plugin or no
coverage. The §2 machinery would make query placement safe; shipping headers first
keeps the guarantee testable in isolation.

### What the guest learns

`nine.caps` gains the header **names** only, consistent with `grantDescription`
reporting guest paths and hiding host paths (I-TVM.8). It confers nothing and no
enforcement decision reads it. A tool that knows it is authenticated reports `this
tool has no credential for api.github.com` instead of returning an opaque 401 —
the argument that justified `caps` existing.

### Refusals

An `auth` entry naming an unset environment variable is a config error at load, not
a request-time failure. A tool that half-authenticates is the silent degradation
R-TVM.6 refuses, and a request-time failure surfaces as a 401 in a journal read
days later.

---

## 4. HTTP methods

`config.go:1498` validates a grant's `methods` against a closed set — GET, HEAD,
POST, PUT, PATCH, DELETE, OPTIONS — and `methodAllowed` (`nethttp.go:242`) enforces
it before the request is built, independently of the two address gates.

The WebDAV family is absent: PROPFIND, PROPPATCH, MKCOL, COPY, MOVE, LOCK, UNLOCK,
REPORT, MKCALENDAR, ACL, SEARCH. CalDAV and CardDAV sync and git-over-HTTP need
them, and nothing in the transport objects — `http.NewRequestWithContext` accepts
any token-valid method.

There is no wildcard. `allow_hosts` may be a bare `*` under R-TVM.12's amendment
and `methods` may not, so a tool needing broad verb coverage enumerates it.

**`methods = ["*"]` means every token-valid verb except CONNECT and TRACE**, and the
explicit list grows to include the WebDAV family.

The wildcard follows the hosts wildcard exactly: it grants any **method**, not any
**address**. Method checking happens before the request is built and both address
gates are untouched, so `*` weakens no SSRF control.

| Verb | Refused whatever the grant says |
|---|---|
| CONNECT | Defeats gate 1 by delegation. An allowlisted host running an open proxy tunnels to any host, so the hostname allowlist stops describing where the tool reaches. |
| TRACE | Reflects request headers into the response body. With §3's injected credentials a hostile allowlisted host puts the credential in `tool_end.Output`. §2's redaction catches it, and refusing the verb costs less than relying on a backstop. |

This section is independent of §2 and §3 and ships on its own.

---

## 5. Progress

### The progress rail

`internal/runtime/daemon.go:320` `EmitProgress(agentID, msg)` carries
`protocol.Msg` to a turn's progress stream, and `ToProgressEvent`
(`protocol.go:819`) translates `response_chunk`, `thinking_chunk`, `tool_start` and
`tool_end` for the TUI and the API. No tool-authored chunk type exists, so a tool
is silent between `tool_start` and `tool_end` — 7ms for a transform, an hour for a
job. `Continuation.Progress` (`abi.go:137`) is one line, at a call boundary,
available only to a `resumable` tool.

### Progress is narration, not output

A tool that emits 50,000 rows as chunks and also returns them produces its result
twice: one copy takes `tool-output-spill.md`'s spill path and the other enlarges the
journal. A tool that emits rows instead of returning them has built a result
channel the model cannot read, because the LLM tool protocol delivers one result
per call, at the end.

The host import is therefore named `progress` and the stdlib module `nine:progress`
— `emit` or `output` would invite the misreading. A tool with a large result spills,
which already works.

`progress()` mid-call and `Continuation.Progress` at call end are the same channel
at two granularities: the latter becomes the final progress line when the tool sets
none. The continuation envelope is unchanged.

### Destination per run mode

| Run mode | `progress()` reaches |
|---|---|
| Ordinary call in a turn | the turn's progress stream (TUI, API), journaled under the tool span as `tool_http` is |
| Job | the job row's live progress (`JobUpdateLive`, `jobs.go:329`), so `job_check` shows it — the one path by which streamed progress reaches the model, by polling rather than push |
| Standing run | the run's bounded ring buffer, read by `nine tool logs`. Not the human feed, which stays cycle output only (R-TVM.20) |
| Reaction (§6) | the same ring buffer |

### Why `progress` is separate from `log`

`log` reaches the daemon log plus a bounded ring the model sees only on failure: a
diagnostic channel for a broken tool. `progress` reaches whoever is watching a call
that is working. Merging them floods the daemon log with narration, or hides failure
diagnostics inside a TUI stream.

### Bounds

R-TVM.4 gains a per-call cap on the number of progress lines and on total bytes.
Excess lines are dropped, with one line recording the drop; a tool that failed
because it narrated too much would be a worse outcome than truncated narration.
Lines pass through §2's redaction and through `logsafe.Value`
(`internal/logsafe/logsafe.go`), which strips control characters and line
structure from tool-authored text entering a log record.

---

## 6. Reactions

### The gap

`internal/subscribe/subscribe.go` is a durable cursor-backed subscription
primitive: at-least-once delivery, resumption from `event_cursors` after a restart,
off the turn path, poison events skipped rather than wedging the cursor. It has two
implementations in the daemon — `internal/subscribers/related.go` (`daemon.go:683`)
and the supervisor (`supervisor.go:68`).

Reacting to the journal therefore means writing Go and rebuilding. For an operator
that is a fork. For Nine it is prohibited by R-PLUG.7 and should remain so.

### A reaction satisfies R-SUB.3 by construction

R-SUB.3 requires a subscriber to make no generative LLM call, never mutate an
active session, and write only derived stores.

| | Go handler | Sandboxed tool |
|---|---|---|
| No generative LLM call | by discipline — the store and the LLM client are in scope | structurally — no host function reaches a model, and none will be exported |
| Writes only derived stores | by discipline — it holds a `*memory.Store` | structurally — its only durable reach is its own quota-bounded `state` namespace |
| Never wakes a turn | by discipline | structurally — the run mode carries no wake target |
| Bounded cost | by review | `timeout`, `max_ops`, `memory_mb` (R-TVM.4) |
| Auditable | slog | the `tool_http` audit, the ring buffer, and `nine tools show` printing the source |

A Go handler holds the discipline because someone read it; a tool has no other
option. For the guarantee R-SUB.3 protects, the tool tier is the stronger
substrate.

### A third run mode

| | Job (R-TVM.19) | Standing run (R-TVM.20) | Reaction |
|---|---|---|---|
| Started by | the model, mid-turn | configuration, at boot | configuration, at boot |
| Trigger | the call | `interval` xor `schedule` | a journal event type |
| Owner | the conversation | the operator | the operator |
| Input | the model's arguments | configured `args` | configured `args` plus the event |
| A returned result means | the job is done | one cycle is done | one event is handled |
| Bounds | `job_max_calls`, `job_max_seconds` | health limits | health limits |

Standing runs ship, so this reuses code rather than a design. `StandingRunner`
(`internal/runtime/standing_tools.go`, wired at `cmd/nine/daemon.go:304`) already
holds every part a reaction needs: `runDue`/`runOnce` for the cycle, `report` and
`notifyHuman` for the human feed, `recordFailure`/`backoff` for the health state
machine, `standingLog` for the activity ring, and `ReconcileStandingTools` for the
configuration-owns-definition split. A reaction is that driver with the trigger
replaced, which makes it the smallest of the four additions to build.

The reporting rule carries over unchanged: a reaction's output goes to the human
feed, empty output is silent, and it cannot wake anything of its own accord
(R-TVM.20).

### Why a reaction may not wake when a condition trigger may

A standing run **can** wake an agent today: `ReconcileConditionTriggers`
(`standing_tools.go:244`) turns a standing agent's `when = { … }` block into a
standing run carrying `WakeAgent`, and `Daemon.WakeAgent` (`daemon.go:725`)
delivers it. Reactions do not get that, and the asymmetry is the event rate rather
than the destination.

A condition trigger's cadence is `interval` xor `schedule` — operator-set, so wake
frequency is bounded by the clock whatever the tool finds. A reaction's cadence is
the journal's event rate, which is driven by the agent's own activity. A reaction
that could wake would close the loop: the agent acts, the action journals an event,
the reaction fires, the wake makes the agent act. That is the
reaction→event→reaction feedback loop `adr/reactive-events.md` §1a states it
eliminated at the source, and R-SUB.7 keeps deferred.

So a finding reaches an agent by the count and `reactions_get` below — read at a
turn boundary the human started — and never by a wake.

### The trigger is the subscription wake, not the standing tick

`RunStandingTools` (`cmd/nine/daemon.go:313`) drives due runs on a ticker whose
period is `[plugins] job_poll_seconds`. A reaction driven off that ticker would
have a latency floor of one tick, which defeats the point: a reaction exists to
respond to an event, and a standing run already covers "check every N seconds".

So a reaction takes its trigger from the subscription's in-process wake (R-SUB.2,
"an in-process wake delivers promptly"), and only the health machinery —
`recordFailure`, `backoff`, the `failing` transition, the activity ring — comes from
`StandingRunner`. Catch-up after a restart is the subscription's, from its cursor,
not a sweeper's scan for due rows.

This is the one place a reaction is not simply a standing run with a different
trigger, and the split matters for M5: reuse the health and reporting halves of the
driver, not `runDue`.

### Two cursors

| | What it is | Owner | Advances when |
|---|---|---|---|
| Subscription cursor | the last journal `seq` handled, in `event_cursors` | the host (R-SUB.1) | one event finishes being handled |
| Continuation cursor | the tool's resume point, opaque to the host | the tool (R-TVM.19) | the tool returns `continue` mid-event |

A reaction may take several calls to handle one event, through the ordinary
resumable mechanism. The subscription cursor must not advance until the last of
them returns a result; otherwise a restart mid-event drops the event and
at-least-once delivery becomes at-most-once.

### Configuration

`[[reaction]]` is a sibling of `[[standing_tool]]` (`config.go:43`) and follows the
same ownership split R-TVM.20 settles: configuration owns the definition, the
runtime owns the run state.

```toml
[[reaction]]
name   = "tag-large-diffs"
tool   = "diff-tagger"
on     = ["tool_end"]        # journal event types
args   = { threshold = 500 }
notify = "reviewer"          # optional; omitted, findings reach the human feed
```

`on = []` is a config error rather than a subscription to every event type. A
subscriber to everything is one sandboxed call per event on the daemon's busiest
stream.

The generated tier is gated by `[tools.agent] allow_reactions`, off by default, for
the reason `allow_standing` exists: Nine writing itself a reaction to its own
journal is a different decision from Nine writing itself a date formatter, and the
capability ceiling cannot express it, since a trigger is not reach.

### Delivering findings to an agent

A finding reaches the human feed (R-TVM.20), which no agent can read, so a reaction
informs a human and not an agent. Two mechanisms already carry pending work into a
turn, and choosing between them is the design.

| Mechanism | Site | Shape | Marked consumed by |
|---|---|---|---|
| `prependNotifications` | `agent_worker.go:699` | every message text prepended to the user's turn | `Fetch`, on delivery |
| `QueuedMessagesCount` | `builder.go:215` | a count in the system prompt naming the tool that reads them | the `queued_messages_get` tool |

The count shape is the one to copy. A reaction on `tool_end` fires as often as tools
are called, so prepending every finding is unbounded context injection into a turn
the human started; a count costs one line until the model decides to look. It is
also the stricter reading of pull — prepending is a push into context that happens
to be timed to a turn boundary.

Three pieces, each an instance of something that exists:

1. Findings accumulate in the agent-facing `notifications` table
   (`memory/notifications.go`), already agent-scoped, already carrying a
   `Delivered` flag, and already the destination for job completions
   (`jobs.go:303`).
2. `ninectx.BuildInput` gains a count field rendering `[System: 5 reaction findings
   are pending. Use the reactions_get tool to read them.]` beside the queued-messages
   line.
3. `reactions_get` returns pending findings oldest-first and marks them delivered,
   as `queued_messages_get` does.

`reactions_get` puts the verb last, matching `queued_messages_get` and every other
paired tool in the catalog. It does not follow `job_check`, which is keyed by a
handle the model holds; a finding has no handle an agent knows.

### This answers standing-tools open question 3

R-TVM.20 says a cycle's output reaches the human feed and nowhere else, and
`adr/standing-tools.md` §12.3 leaves open whether a run may name an agent as its
note owner at all — observing that agent-owned notes are the path by which
deterministic tool code changes an agent's behaviour with no human in between. A
finding an agent can read crosses that line. Three properties keep it on the right
side of R-SUB.3:

1. **The owner is operator-declared.** `[[reaction]] notify = "<agent-id>"` is
   written in configuration and requestable by neither the tool nor the agent — the
   safeguard the shipped condition trigger already relies on, where
   `memory.StandingTool.WakeAgent` is likewise set by reconciling an operator's
   `when = { … }` block and never by a request.

   `notify` is deliberately not `wake_agent`, and the store must keep them apart:
   `WakeAgent` interrupts a running agent, `notify` names where a note is filed for
   a later turn to collect.
2. **Delivery is a pull.** The context builder assembles the count at a turn
   boundary the human started, and `reactions_get` runs only when the model calls
   it. Nothing wakes.
3. **`notify` redirects rather than duplicates**, and an undeliverable finding falls
   back to the human feed rather than being dropped — the condition-trigger rule
   R-TVM.20 already states, applied unchanged.

---

## 7. Why tool→tool dispatch is rejected

An MCP server is already a plugin: one process per `[[mcp.server]]`, its own wire
name (`mcp:github`), its own roster row (`internal/builtins/mcp.go:17-28`). Letting
a tool reach MCP is tool→tool dispatch, which `spec/contracts/toolvm.md:256` lists
beside spawning a process as structurally absent.

The sandbox would survive it. Nested calls cost wazero nothing, the fresh-instance
model is untouched, and a depth cap plus a deadline carved from the caller's
remaining time is what `net.http` already does with its four-fifths rule.

The grant table would not survive it. An operator reading `[tool.summarize]` sees
`Grant.Summary() == "none"`; if that tool can name `mcp:github__create_issue`, it
can create issues and `none` is false. A tool's effective reach becomes the
transitive closure of every tool it can name, and `docs/sandboxed-tools.md` §2
reduces to one sentence — the agent writes the code, the operator writes the
grants, these are never the same actor — which a closure the operator cannot see
breaks.

A containable shape exists, recorded for a later reader: a named allowlist of
callee names rather than a general dispatch verb, the callee running under its own
grant with no inheritance in either direction, a depth and fan-out cap, per-call
audit parented as `tool_http` is, and `Summary()` printing the closure rather than
the direct edge. The last item is the expensive one and is not optional.

MCP is the worst starting point for it. An MCP server's tool list is discovered
from the server at runtime, not declared in `nine.toml`, so `mcp:github__*` is a
grant whose meaning changes when the upstream adds a tool — the operator reviews
one thing and confers another. Such an allowlist needs exact names and no globs.

The motivating case is also narrower than it appears. What a tool wants from MCP is
reach to one upstream service, and for an HTTP-transport MCP server `net.http` with
a host allowlist plus §3's credentials supplies that. What remains is stdio
servers, which have no URL to allow — better closed by giving the bridge an HTTP
endpoint than by opening dispatch.

---

## 8. The authoring skill

`skills/tool-authoring.md` is what an agent reads before calling `tool_write`, and
each addition changes the correct answer:

- a tool needing an authenticated API declares `net: ["http"]` and must not
  construct an `Authorization` header, because the operator's grant supplies it;
- a tool declares the verbs it uses, and the WebDAV family is now declarable — the
  skill lists it rather than leaving an agent to guess that PROPFIND is refused;
- a tool doing slow work narrates with `nine:progress`, and progress is not how it
  returns data — the likely misuse, so the skill carries a worked example of the
  wrong version;
- a tool can be the thing that reacts, a shape no existing skill describes, and one
  arriving with a constraint an agent will otherwise fight: it leaves a note and
  cannot wake anyone.

The first three extend `tool-authoring.md`. Reactions become a separate
`skills/tool-reactions.md`, because the audience differs — `tool-authoring` is read
when an agent wants a helper for itself, while a reaction is a standing arrangement
an operator enables through `allow_reactions`, and its body is mostly what a
reaction must not attempt.

`reactions_get` belongs to neither, because reading a finding is not authoring
anything. The pending-count line names the tool in the system prompt, which is how
`queued_messages_get` is already found, so the catalog description carries the whole
explanation: what a finding is, that draining it marks it delivered, and that a
finding is a note from deterministic code rather than an instruction from a person.
The last part is what stops an agent treating a finding as a user turn.

All are built-in skills seeded from `skills/` at boot (`docs/skills.md`), so the
`description` line is what the vector search matches. It needs the words an agent
uses — `api key`, `authenticate`, `token`; `react`, `watch`, `when something
happens` — not this note's vocabulary.

---

## 9. Spec impact

| Rule | Change |
|---|---|
| R-TVM.12 | amended: `auth` as a grant parameter, host-injected, headers only; the guest never receives the value; `nine.caps` reports header names only |
| R-TVM.12 | amended: the audit record, the `tool_http` payload, the `slog` line and the tool's returned envelope MUST have injected secret values removed by exact match before leaving the host |
| R-TVM.12 | amended: the method allowlist gains the WebDAV family and a `*` wildcard; CONNECT and TRACE MUST be refused whatever the grant says |
| R-TVM.21 (new) | progress: an unconditional host import; narration and not a result; the per-run-mode destinations of §5; per-call line and byte caps; `Continuation.Progress` as its call-boundary granularity |
| R-TVM.22 (new) | reactions: a third run mode; journal-event trigger; `on = []` refused; the two cursors and the ordering rule between them; R-TVM.20's reporting rules unchanged |
| R-TVM.20 | amended: `notify` names an operator-declared agent owner for a finding, redirecting rather than duplicating, with the human feed as the undeliverable fallback. Answers `adr/standing-tools.md` §12.3 |
| R-TVM.23 (new) | `reactions_get` and the pending-findings count: findings accrue in the agent-facing `notifications` table; the count is rendered by the context builder; the tool marks them delivered |
| R-SUB.5 | amended: a second pull-surfacing path that is unconditional rather than relevance-gated, since a pending count has no topic |
| R-TVM.5 | unchanged — no new capability, asserted explicitly because each facility would plausibly have added one |
| R-SUB.1 | amended: a subscriber handler MAY be a sandboxed tool; cursor semantics unchanged |
| R-SUB.3 | unchanged, and satisfied structurally rather than by discipline (§6) |
| R-SUB.7 | unchanged: no generative reaction, no session injection |
| I-TVM.9 (new) | an injected credential is never observable by a guest and never present in any record the host writes |
| `docs/sandboxed-tools.md` | §6.2 gains the `auth` parameter; §11 loses the unauthenticated-tool limit; reactions get a section beside §6.6 |
| `docs/writing-sandboxed-tools.md` | `nine:progress`; the auth rule for authors |
| `spec/conformance.md` | rows for R-TVM.21, R-TVM.22, I-TVM.9; R-SUB.1's row extended |

---

## 10. Phasing

| Phase | Work | Gate |
|---|---|---|
| M1 | The §2 redaction pass — exact-value removal across the audit record, the `tool_http` payload, the `slog` line, `nine.log` lines and the returned envelope. Built against a synthetic secret, with no injection path yet | a test asserting a planted value appears at none of §2's six sites |
| M2 | `auth` on the `net.http` grant: config shape, validation, host-side injection after both gates, `nine.caps` header names | an authenticated request to a test server; the value absent from the journal; a retargeted host refused |
| M3 | The §4 method allowlist: the WebDAV verbs, `methods = ["*"]`, CONNECT and TRACE refused. Independent of M1 and M2 | a PROPFIND against a test server; `*` accepted; CONNECT and TRACE refused under `*` |
| M4 | `nine:progress` and its host import, per-call caps, the turn and job destinations | a long call's lines reach the progress stream; `job_check` shows live progress; the cap drops rather than fails |
| M5 | Reactions: `[[reaction]]` config, a subscriber adapter reusing the shipped `StandingRunner`'s health and reporting halves but triggered by the subscription wake rather than `runDue`, the two-cursor ordering, the ring-buffer destination | a reaction fires on a real journal event within the wake latency, not a tick; a restart mid-event redelivers; empty output is silent |
| M6 | Delivery to an agent: `notify`, findings into the `notifications` table, the pending count in `BuildInput`, the `reactions_get` tool | a finding reaches its named agent's next turn as a count; `reactions_get` drains it; an undeliverable finding lands on the human feed |
| M7 | `allow_reactions` for the generated tier; `nine tools` and TUI surfacing | a generated reaction refused with the flag off |
| M8 | `skills/tool-authoring.md` extended, `skills/tool-reactions.md` written, `docs/` and `spec/` per §9 | `make ci`; an eval writing an authenticated tool without inventing an `Authorization` header |

M1 precedes M2 because building injection first ships a window in which the no-leak
property is untested, and a journalled secret cannot be un-journalled. M5 precedes M6
because a reaction with no delivery path is testable and a delivery path with no
reaction is not.

---

## 11. Open questions

1. **Secret sources beyond `env`.** `{ env = "GH_TOKEN" }` is the only source here.
   A file source is trivially addable and probably wanted, since Docker and
   Kubernetes secrets are files. An external secret manager is not, and is out of
   scope for this note.
2. **Rotation.** Grants are read at boot (`docs/sandboxed-tools.md` §11), so a
   rotated token needs a restart. `adr/capability-grants.md` moved the generated
   tier's ceiling into the store to avoid exactly this; whether `auth` follows
   affects the config shape.
3. **Whether one shared worker budget survives a third claimant.** Not open in
   principle — `adr/standing-tools.md` §12.1 is already settled in code: standing
   runs take `cfg.Tools.JobWorkers`, the job sweeper's budget, because
   `cmd/nine/daemon.go:297` records that what both bound is concurrent wasm
   instantiations. Reactions take the same budget for the same reason. What is open
   is whether one number still serves when an event-rate-driven claimant joins two
   clock-driven ones, since a reaction storm would starve jobs before any health
   limit noticed.
4. **Whether `progress` is journaled or only streamed.** Journaling gives `nine
   trace` a narrative of a long call; not journaling keeps a chatty tool from
   dominating the journal, the concern `adr/standing-tools.md` §7.1 raises for
   standing calls. Journaling the final line per call and streaming the rest is the
   current leaning.
5. **Reaction fan-out.** One event matching three reactions is three sandboxed
   calls. Whether that needs a per-reaction rate limit distinct from the health
   limits depends on how busy `tool_end` is in practice.
6. **Whether the pending count is aggregate or per-reaction.** `[System: 5 reaction
   findings are pending]` is one line; naming which reactions produced them is more
   useful and grows with the number configured. Aggregate is the current leaning,
   since `reactions_get` names them on read.
7. **Whether `*` admits nonstandard verbs.** As written it allows any token-valid
   method, so a typo reaches the upstream as `PSOT` and returns a 405 rather than a
   config error. The alternative is `*` meaning a known list, which then needs
   extending for each new verb — the cost the wildcard exists to remove.

---

## Limits

| Limit | Detail |
|-------|--------|
| Nothing here is built | The note is a design record. No phase in §10 has started. |
| Secret derivatives leak | §2's redaction removes values the host injected. A tool that returns a signed URL, a minted cookie or a re-encoding of a token leaks it, and the host cannot detect that. Source review before conferring the grant is the only control. |
| Query-parameter auth unsupported | Deliberate (§3). APIs that accept only `?api_key=` remain out of reach for the tier. |
| Progress does not reach the model mid-call | The LLM tool protocol delivers one result per call. Only the job destination reaches a model, by polling `job_check` across turns. |
| Reactions cannot wake anything | Deliberate, and the same line `adr/reactive-events.md` §1a draws. Work needing an immediate response to an event belongs to a condition trigger on a standing agent. |
| Tool→tool dispatch and MCP reach rejected | Deliberate (§7). Revisit when a concrete need appears that `net.http` plus §3 cannot meet, and bring the closure-printing `Summary()` with it. |
| Grants still read at boot | Unchanged by this note. `auth` inherits it, so rotation needs a restart until open question 2 is answered. |
| CONNECT and TRACE are never grantable | Deliberate (§4). No grant, including `methods = ["*"]`, confers them. |
| Agent-directed findings bypass the human feed | `notify` redirects rather than duplicates (§6), matching the condition-trigger rule. An operator watching only the feed does not see findings routed to an agent. |
| A finding is delivered once, to one agent | `reactions_get` marks findings delivered, as `queued_messages_get` does. Two agents cannot both read one finding, because `notify` names a single owner. |
| Reactions share the job worker budget | `cfg.Tools.JobWorkers` bounds concurrent wasm instantiations across jobs, standing runs and now reactions. An event-rate-driven claimant can starve two clock-driven ones before a health limit notices (open question 3). |
| A reaction cannot wake, though a condition trigger can | Deliberate (§6). The difference is cadence: a condition trigger's is operator-set, a reaction's is the journal's event rate, so a waking reaction would close the reaction→event→reaction loop. |
| Reactions inherit undocumented health behaviour | The thresholds a reaction would reuse — three consecutive failures to `failing`, ten to auto-disable a generated run, a 30-minute backoff cap — are in `internal/runtime/standing_tools.go` and in no `docs/` page. Reusing the machinery inherits that gap and widens it. |
| The pending count is not relevance-gated | Unlike the R-SUB.5 enrichment path it appears whenever findings are pending, costing a line of every turn's system prompt until the model drains them. |
