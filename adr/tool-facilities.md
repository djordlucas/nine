# Sandboxed tool credentials, progress and reactions

- **Status:** Proposed (design note). Nothing here is built.
- **Date:** 2026-09-29.
- **Scope:** `[tool.<name>.capabilities.net.http.auth]`, a `nine:progress` stdlib
  module and its host import, `[[reaction]]`, `[tools.agent] allow_reactions`,
  `skills/tool-authoring.md` and a new `skills/tool-reactions.md`.
- **Depends on:** the toolvm host, `net.http` and its audit, the session journal
  and its subscriptions, the standing-run driver.
- **Follows:** `adr/rich-js-tools.md`, `adr/durable-and-long-running-tools.md`,
  `adr/standing-tools.md`.
- **Rejects:** tool→tool dispatch, and with it MCP reach — §6.

Three facilities, none of which widens what a sandboxed tool can reach. Credentials
give a tool less than the `env` capability would, because it authenticates without
holding the secret. Progress gives a new destination for text the tool already
produces. Reactions give a new trigger for a run mode that exists. No row is added
to the R-TVM.5 capability table and the sandbox boundary is unchanged in all three
cases.

| | Gap today | Shape | New capability |
|---|---|---|---|
| **Credentials** (§3) | a granted tool reaches an API and cannot authenticate to it | a parameter on the existing `net.http` grant; the host injects, the guest never sees the value | no — a grant parameter |
| **Progress** (§4) | a tool is silent for the whole of a call, however long | a host import writing to the rail `EmitProgress` already provides | no — unconditional, like `log` |
| **Reactions** (§5) | reacting to the journal requires writing Go and rebuilding | a third run mode of a resumable tool, triggered by a journal event | no — a run mode |

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

## 4. Progress

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
| Reaction (§5) | the same ring buffer |

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

## 5. Reactions

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

The driver, capability resolution, the per-call deadline, the `continue` envelope,
the ring buffer and the health state machine are shared with a standing run
unchanged. So is the reporting rule: a reaction's output goes to the human feed and
nowhere else, empty output is silent, and it cannot address an agent or wake
anything of its own accord (R-TVM.20).

That constraint is what keeps R-SUB.7 true. A reaction that could wake a turn is
the autonomous session injection `adr/reactive-events.md` §1a defers, reached
through a side door.

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
name = "tag-large-diffs"
tool = "diff-tagger"
on   = ["tool_end"]        # journal event types
args = { threshold = 500 }
```

`on = []` is a config error rather than a subscription to every event type. A
subscriber to everything is one sandboxed call per event on the daemon's busiest
stream.

The generated tier is gated by `[tools.agent] allow_reactions`, off by default, for
the reason `allow_standing` exists: Nine writing itself a reaction to its own
journal is a different decision from Nine writing itself a date formatter, and the
capability ceiling cannot express it, since a trigger is not reach.

---

## 6. Why tool→tool dispatch is rejected

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

## 7. The authoring skill

`skills/tool-authoring.md` is what an agent reads before calling `tool_write`, and
all three facilities change the correct answer:

- a tool needing an authenticated API declares `net: ["http"]` and must not
  construct an `Authorization` header, because the operator's grant supplies it;
- a tool doing slow work narrates with `nine:progress`, and progress is not how it
  returns data — the likely misuse, so the skill carries a worked example of the
  wrong version;
- a tool can be the thing that reacts, a shape no existing skill describes, and one
  arriving with a constraint an agent will otherwise fight: it leaves a note and
  cannot wake anyone.

The first two extend `tool-authoring.md`. Reactions become a separate
`skills/tool-reactions.md`, because the audience differs — `tool-authoring` is read
when an agent wants a helper for itself, while a reaction is a standing arrangement
an operator enables through `allow_reactions`, and its body is mostly what a
reaction must not attempt.

Both are built-in skills seeded from `skills/` at boot (`docs/skills.md`), so the
`description` line is what the vector search matches. It needs the words an agent
uses — `api key`, `authenticate`, `token`; `react`, `watch`, `when something
happens` — not this note's vocabulary.

---

## 8. Spec impact

| Rule | Change |
|---|---|
| R-TVM.12 | amended: `auth` as a grant parameter, host-injected, headers only; the guest never receives the value; `nine.caps` reports header names only |
| R-TVM.12 | amended: the audit record, the `tool_http` payload, the `slog` line and the tool's returned envelope MUST have injected secret values removed by exact match before leaving the host |
| R-TVM.21 (new) | progress: an unconditional host import; narration and not a result; the per-run-mode destinations of §4; per-call line and byte caps; `Continuation.Progress` as its call-boundary granularity |
| R-TVM.22 (new) | reactions: a third run mode; journal-event trigger; `on = []` refused; the two cursors and the ordering rule between them; R-TVM.20's reporting rules unchanged |
| R-TVM.5 | unchanged — no new capability, asserted explicitly because each facility would plausibly have added one |
| R-SUB.1 | amended: a subscriber handler MAY be a sandboxed tool; cursor semantics unchanged |
| R-SUB.3 | unchanged, and satisfied structurally rather than by discipline (§5) |
| R-SUB.7 | unchanged: no generative reaction, no session injection |
| I-TVM.9 (new) | an injected credential is never observable by a guest and never present in any record the host writes |
| `docs/sandboxed-tools.md` | §6.2 gains the `auth` parameter; §11 loses the unauthenticated-tool limit; reactions get a section beside §6.6 |
| `docs/writing-sandboxed-tools.md` | `nine:progress`; the auth rule for authors |
| `spec/conformance.md` | rows for R-TVM.21, R-TVM.22, I-TVM.9; R-SUB.1's row extended |

---

## 9. Phasing

| Phase | Work | Gate |
|---|---|---|
| M1 | The §2 redaction pass — exact-value removal across the audit record, the `tool_http` payload, the `slog` line, `nine.log` lines and the returned envelope. Built against a synthetic secret, with no injection path yet | a test asserting a planted value appears at none of §2's six sites |
| M2 | `auth` on the `net.http` grant: config shape, validation, host-side injection after both gates, `nine.caps` header names | an authenticated request to a test server; the value absent from the journal; a retargeted host refused |
| M3 | `nine:progress` and its host import, per-call caps, the turn and job destinations | a long call's lines reach the progress stream; `job_check` shows live progress; the cap drops rather than fails |
| M4 | Reactions: `[[reaction]]` config, the subscriber adapter over the standing-run driver, the two-cursor ordering, the ring-buffer destination | a reaction fires on a real journal event; a restart mid-event redelivers; empty output is silent |
| M5 | `allow_reactions` for the generated tier; `nine tools` and TUI surfacing | a generated reaction refused with the flag off |
| M6 | `skills/tool-authoring.md` extended, `skills/tool-reactions.md` written, `docs/` and `spec/` per §8 | `make ci`; an eval writing an authenticated tool without inventing an `Authorization` header |

M1 precedes M2 because building injection first ships a window in which the no-leak
property is untested, and a journalled secret cannot be un-journalled.

---

## 10. Open questions

1. **Secret sources beyond `env`.** `{ env = "GH_TOKEN" }` is the only source here.
   A file source is trivially addable and probably wanted, since Docker and
   Kubernetes secrets are files. An external secret manager is not, and is out of
   scope for this note.
2. **Rotation.** Grants are read at boot (`docs/sandboxed-tools.md` §11), so a
   rotated token needs a restart. `adr/capability-grants.md` moved the generated
   tier's ceiling into the store to avoid exactly this; whether `auth` follows
   affects the config shape.
3. **Reaction concurrency budget.** Its own, or the daemon-wide standing-run cap
   `adr/standing-tools.md` §12.1 leaves open. One shared budget is the likely
   answer, since a busy event stream and a tight interval are the same load — which
   makes §12.1 blocking for this note.
4. **Whether `progress` is journaled or only streamed.** Journaling gives `nine
   trace` a narrative of a long call; not journaling keeps a chatty tool from
   dominating the journal, the concern `adr/standing-tools.md` §7.1 raises for
   standing calls. Journaling the final line per call and streaming the rest is the
   current leaning.
5. **Reaction fan-out.** One event matching three reactions is three sandboxed
   calls. Whether that needs a per-reaction rate limit distinct from the health
   limits depends on how busy `tool_end` is in practice.

---

## Limits

| Limit | Detail |
|-------|--------|
| Nothing here is built | The note is a design record. No phase in §9 has started. |
| Secret derivatives leak | §2's redaction removes values the host injected. A tool that returns a signed URL, a minted cookie or a re-encoding of a token leaks it, and the host cannot detect that. Source review before conferring the grant is the only control. |
| Query-parameter auth unsupported | Deliberate (§3). APIs that accept only `?api_key=` remain out of reach for the tier. |
| Progress does not reach the model mid-call | The LLM tool protocol delivers one result per call. Only the job destination reaches a model, by polling `job_check` across turns. |
| Reactions cannot wake anything | Deliberate, and the same line `adr/reactive-events.md` §1a draws. Work needing an immediate response to an event belongs to a condition trigger on a standing agent. |
| Tool→tool dispatch and MCP reach rejected | Deliberate (§6). Revisit when a concrete need appears that `net.http` plus §3 cannot meet, and bring the closure-printing `Summary()` with it. |
| Grants still read at boot | Unchanged by this note. `auth` inherits it, so rotation needs a restart until open question 2 is answered. |
