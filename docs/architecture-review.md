# Architecture review — findings and sequencing

- **Status:** **Review** (rev 2 — §7.1 corrected: the `goal`+`workflow` merge
  proposed in rev 1 is **withdrawn**, see
  [`concept-consolidation.md`](concept-consolidation.md) §4).
  Non-normative: this note records an assessment,
  not a contract. Nothing here changes behavior, so no `spec/` requirement moves
  on account of it. Findings are given stable IDs (`F1`…`F12`) so they can be
  lifted into a roadmap without being restated.
- **Date:** 2026-08-18.
- **Scope:** the whole tree as of `5ef3203` — all 20 `internal/` packages,
  `cmd/nine`, `docs/`, `spec/`, `tests/`.
- **Method:** read the code first and the docs second, then measured. Every claim
  below cites a file or a count; where the docs and the code disagree the code is
  reported and the disagreement is itself the finding.
- **Purpose:** separate the parts of the architecture that are genuinely
  distinctive and must not be eroded (§3) from the seams that need work (§5–§6),
  and put the latter in an order (§8).

---

## 1. TL;DR — the findings

| ID | Finding | Impact | Effort |
|---|---|---|---|
| **F1** | ~~Token budgeting is a `chars/4` estimate and is never reconciled against actual usage — the provider never returns a count~~ **Landed** — `Response.Usage` (R-LLM.8) is journaled and joins the estimate on span; calibration awaits data | **High** | S |
| **F2** | The wire protocol is a stringly-typed fat union: 29 optional `Msg` fields with validity in comments, dispatch on raw string literals | **High** | M |
| **F3** | Daemon assembly is duplicated between production and the eval harness; the refactor that removes it is still *Proposed* | **High** | M |
| **F4** | No schema migration path — `user_version = 1` plus one ad-hoc `ALTER` | **High** | S |
| **F5** | `agent.Loop` carries 12 post-construction observer setters; a Loop is never fully valid until N unordered calls have happened | Medium | M |
| **F6** | Test coverage is inverted at the boundary: `protocol` 0.24, `tui` 0.27, `cli` 0.33 against 0.87 elsewhere | Medium | M |
| **F7** | The four built-in plugins hold ambient authority that the capability model exists to remove | Medium | L |
| **F8** | ~20 first-class nouns; `stage` is a working multi-stage capability with no caller, no precedence rule, and a starvation bug (`goal`+`workflow` examined and **not** collapsible — §7.1) | Medium | L |
| **F9** | ~~One provider behind a generalized `Provider` + `ThinkingAware` abstraction — decide whether local-first is a goal or a stopgap~~ **Decided** — local-first is a commitment **and** multi-backend; the abstraction stays (G8/N5) | Medium | S |
| **F10** | `internal/selfmodel`: 84 LOC, zero tests, `/.dockerenv` probe, swallowed query error | Low | S |
| **F11** | `docs/` + `spec/` is 57% of production code size; implemented design notes are maintained rather than frozen | Low | M |
| **F12** | The `TestRegisterPlugin` flake is documented as inherent; a socket-timing flake usually means a missing readiness handshake | Low | S |

**F1–F4 are the seams. None of them is in the concepts — they are in wire types,
assembly wiring, schema evolution, and token accounting.** That is the summary
judgment of this review: the design reasoning in this repo is consistently better
than the plumbing that carries it.

---

## 2. What was measured

| Measure | Value |
|---|---|
| Production Go (`internal/` + `cmd/`, excluding `_test.go`) | ~31,400 LOC |
| Test Go (`internal/`, `cmd/`, `tests/`) | ~31,000 LOC |
| test:code ratio, `internal/` only | 0.87 |
| `docs/` + `spec/` Markdown | 17,904 lines (57% of production Go) |
| `TODO` / `FIXME` / `XXX` / `HACK` in production Go | **0** |
| Internal packages | 20 |
| Cross-cutting invariants (`spec/overview.md` §5) | 11 |
| Spec contracts | 20 |
| Wire message types dispatched in `daemon.go` | 20 |

The zero-TODO count across 31k LOC is the most informative single number in the
repo, and §9 returns to why.

---

## 3. What is distinctive — protect these

These are the parts that are unusual enough to be the project's actual identity.
Every proposal in §5–§7 is constrained by *not eroding them*.

**3.1 Spec-as-contract, embedded in the binary.** `spec/contracts/*.md` carries
stable requirement IDs (`R-TVM.14`, `R-PLUG.7`), 11 numbered invariants, and a
`conformance.md` checklist — compiled in via `spec/embed.go`, so `nine spec
<topic>` always matches the running version. Intent lives in a versioned contract
with a `Status:` header rather than in a code comment. This is the mechanism that
produces the zero-TODO count; it is not a coincidence.

**3.2 Language-neutral concurrency vocabulary** (`spec/overview.md` §5.1).
Contracts specify behavior — *single-slot inbox*, *wait-for-first-of*, *bounded
buffer with a stated overflow discipline* — and a table maps each to its Go
primitive. A reimplementation in another language stays conformance-checkable
against the same document. Very few projects at any scale do this.

**3.3 Pull-not-push, applied consistently.** I11's *enrich, don't interject* holds
across journal subscribers, plugin job completion, related-session links, and
new-tool visibility. The strongest instance is `docs/plugin-capabilities.md` §2:
an already-designed reverse channel (second socket, capability tokens, grant
model) was **deleted** and inverted to daemon-polls-plugin. Deleting a finished
design because it fits the system worse than the alternative is the single best
judgment call in the tree.

**3.4 Capabilities conferred, never claimed — checked both ways.**
`resolveGrant` (`internal/toolvm/capability.go:129`) refuses *granted-but-not-declared*
as well as *declared-but-not-granted*, so an over-broad `[tool.x]` table written
months ago cannot survive review unnoticed. Nearly everyone checks one direction.
`resolveCeiling:233` applies the same rigor to the harder case — a ceiling is a
maximum, never a default, and a tool cannot request its way past it.

**3.5 Roles subsuming `depth int`.** `docs/roles.md` §1 correctly identifies that
a threaded integer was a role in disguise, and — the part that matters — that a
*skill* cannot enforce a tool boundary, because skill context is advisory and
droppable under budget pressure. Enforcement at loop-build time, persona as data.

**3.6 Self-improvement as data, not code** (N1–N3, I10), with the runtime
container shipping no Go toolchain, no git, and no source tree. A principled
answer to a question most agent systems leave deliberately vague.

---

## 4. What is solidly good

- **I3 — one store, a writer pool of exactly one plus a concurrent read pool.**
  The correct SQLite model, stated as an invariant rather than discovered under
  load.
- **I1 — one session, one serial worker, single-slot inbox.** Eliminates the
  interleaved-turn class of bug by construction.
- **I2 — the LLM is reachable only through the queue.** One choke point for
  concurrency and three-level priority.
- **One dispatcher, two backends** (`R-DISP.4`): sandboxed tools are
  "indistinguishable downstream" from plugin tools — registered, role-filtered,
  gated, and capped identically.
- **Pure Go end to end** — `modernc.org/sqlite`, wazero, no CGO, one static
  binary. Operationally this buys more than it appears to.
- **Append-only journal with `span_id` / `parent_span_id`** — a real tracing
  shape, plus `internal/replay`, plus evals that assert on the *journal* rather
  than on daemon state.
- **`Provider` is one method.** Refreshing restraint (see F9 for the caveat).
- **Adversarial tests where they belong**: SSRF, DNS rebinding, redirect
  laundering, credential stripping (`internal/toolvm/ssrf_test.go`,
  `nethttp_test.go`).

---

## 5. Findings — needs work

### F1 — Token budgeting is an estimate that is never reconciled · **High** · S

`countTokens` is `(len(s)+3)/4` (`internal/context/builder.go:334`), and
`truncateTokens:399` uses the same divisor. G5 — *"every LLM turn is assembled to
a fixed token budget"* — is the headline goal of the system, and it is enforced
with an approximation that is off by 20–50% on exactly the content being
budgeted: JSON tool schemas, code, tool results, non-English text.

The compounding problem is that the actual number never comes back.
`llm.Response` (`internal/llm/provider.go:74`) carries `Text`, `ToolCalls`,
`StopReason`, and `ThinkingUsed` — **no usage field**. So the system cannot answer
"did we overflow, and by how much?", and the budget cannot be calibrated even in
principle.

**Proposal.** Add `Usage{InputTokens, OutputTokens}` to `llm.Response`; populate
it from the ollama response; record estimate-vs-actual per turn on the existing
journal; calibrate the divisor per content class once there is data. This is the
cheapest high-value change in the review — it converts the system's primary
constraint from dead reckoning to measurement.

**Status: landed**, as proposed except that no new event type was added. `Usage`
is normative as **R-LLM.8**; the Ollama adapter reads `prompt_eval_count` /
`eval_count` off the final stream chunk; `llm_response` carries
`input_tokens` / `output_tokens`, which join `llm_request.tokens_used` on the
shared LLM-call `span_id` rather than duplicating the estimate. Zero means *not
reported*, never *nothing consumed*, so an unreporting provider cannot be
mistaken for a total overestimate. **The divisor is unchanged** — calibration is
a separate change once real turns have accumulated, which is the point of
landing the measurement first.

### F2 — The wire protocol is a stringly-typed fat union · **High** · M

`protocol.Msg` carries 29 optional, `omitempty` fields whose validity is expressed
in comments (`// set when Type == "…"`), across 20 message types
(`internal/protocol/protocol.go`). `internal/runtime/daemon.go:422` dispatches on
raw string literals (`case "new_conversation":`) with no typed constants, and
`ProgressEvent` duplicates the whole pattern for the client side — including the
comments, which are the only record of which field means anything for which type.

The five-place-lockstep checklist in `CLAUDE.md` — `Msg` fields, constructor,
doc-comment block, client method, dispatch switch, handler, and the
`wire-protocol.md` tables — exists precisely to compensate for this. **The
checklist is a symptom, not a solution.** This is the one place where the
codebase's rigor is enforced by ritual rather than by the compiler, in the one
component that has a published contract and a compatibility story.

**Proposal, in increasing order of ambition:**

1. Typed `MsgType` constants and a switch over them (mechanical, removes the
   literal-typo class entirely).
2. A generated or hand-written table mapping each type to its required fields,
   validated on ingress — turns the comments into a check.
3. Per-message payload structs behind a `Type` discriminator, with `Msg` reduced
   to an envelope. Largest change; retires most of the checklist.

Even (1) is worth doing immediately and independently.

### F3 — Assembly is duplicated; the fix is written but not landed · **High** · M

`AgentBuilder.build` is ~280 lines (`internal/runtime/builder.go:368–649`) inside
a 1011-line file — the assembly god-function. `runDaemon`
(`cmd/nine/daemon.go`) and `Harness.Run` (`tests/evals/runner/harness.go`)
construct a fully-wired daemon from the same dependencies twice, and the Go
compiler only catches *signature* drift, never *additive* drift.

`docs/daemon-assembly-refactor.md` diagnoses this exactly right and is still
marked **Proposed**. Its own framing is the argument for landing it: the Stop
hook (`.claude/hooks/eval-harness-guard.sh`) and `/sync-evals` are **detection,
not prevention**.

**Proposal.** Land that note as written. It deletes a hook, deletes a slash
command's reason to exist, and converts "the harness reproduces production" from
a review-time reminder into a compile error. Highest-leverage cleanup available.

### F4 — No schema migration path · **High** · S

`internal/memory/db.go` applies the schema idempotently on every `Open`, sets
`PRAGMA user_version = 1` (`:452`), and carries exactly one ad-hoc
`addColumnIfMissing` for the `tools.lockfile` column added after v2.1.0
(`:461–465`). `CREATE TABLE IF NOT EXISTS` handles additive tables and indexes and
nothing else.

G2 promises sessions durable across restarts. The day a column type changes, a
backfill is needed, or — as the code comment itself predicts — the FTS tokenizer
changes and the index needs `INSERT INTO files_fts(files_fts) VALUES('rebuild')`,
there is no mechanism and no place to put one.

**Proposal.** A ~40-line versioned step runner: an ordered `[]func(db) error`
indexed by `user_version`, run inside a transaction, bumping the pragma as it
goes. The existing `addColumnIfMissing` becomes step 1. Write it **before** it is
needed; a migration runner authored under pressure against a user's live database
is the worst version of this code.

### F5 — `agent.Loop` is configured by 12 observer setters · Medium · M

`Loop` exposes 15 exported `Set*` methods (`internal/agent/loop.go:169–801`), of
which **12 are `SetOn*` observer callbacks** — `SetOnContextUpdate`,
`SetOnToolStart`, `SetOnToolEnd`, `SetOnLLMRequest`, `SetOnLLMResponse`,
`SetOnChunk`, `SetOnThinkingChunk`, `SetOnThinking`, `SetOnPlanStart`,
`SetOnPlanEnd`, `SetOnNotice`, `SetOnStage` — and 3 are genuinely dynamic state
(`SetQueue`, `SetPlanMode`, `SetForceThinkNextTurn`).

The 12 are the problem: a `Loop` returned by `NewLoop` is not fully wired until a
dozen further calls have happened, in an order nothing enforces and no type
records, and a caller that forgets one gets silence rather than an error. It is
also a significant part of why `build` (F3) is 280 lines.

**Proposal.** One `Hooks` struct with no-op defaults, passed to `NewLoop`. Keep
the three dynamic setters as methods; fold the 12 observers into the struct. F3
and F5 should be done together — F5 is a meaningful share of what makes F3 large,
and both edit the same call site.

### F6 — Coverage is inverted at the boundary · Medium · M

test:code by package: `protocol` 0.24, `tui` 0.27, `cli` 0.33, against 0.87 for
`internal/` as a whole and higher in `toolvm` and `agent`. The wire protocol is
the component with a published contract, a version story, and external
consumers — and it is the thinnest-tested thing in the tree. F2 and F6 are the
same problem seen from two directions; typed messages make the tests easy to
write.

### F7 — The built-ins hold ambient authority the capability model removes · Medium · L

`files` (`read_file` / `write_file`, whole disk), `http` (`http_get` /
`http_post`, any host, none of the `ssrf.go` checks), and `time` predate
`internal/toolvm`. They are subprocesses running with the daemon's full uid
authority. A strictly better mechanism — mounted `fs.read`/`fs.write`, allowlisted
`net.http` with the full §8 checklist — now ships in the same binary and is used
for a strictly less trusted class of code.

**Proposal.** Migrate `files`, `http`, and `time` to shipped wasm tools with
operator-visible default grants; leave `shell` (needs real `exec`), user plugins,
and the MCP bridge on the process transport. This collapses four built-in plugins
to one and puts the most-called tools under the capability model and the HTTP
audit trail.

**Cost, stated honestly:** `toolvm` has two source tiers today — developer
(file + manifest on disk) and generated (row in `tools`). Shipping tools inside
the binary needs a **third**, with its own default-grant story, and
`web_search` / `web_page_read` need HTML parsing bundled at build time. This is
real work, not a refactor. It is listed Medium/L for that reason.

---

## 6. Findings — decide, then change or document

### F9 — One provider behind a generalized abstraction · Medium · S

`internal/llm/ollama` is the only implementation of `Provider`, and there is a
second optional interface (`ThinkingAware`) generalizing over providers that do
not exist. An interface with one implementation is a guess about the second one.

This is a fork, and it is the owner's call:

- **If local-first is a commitment** — which G5 ("small-model friendly") strongly
  implies — then say so in `spec/overview.md` §1 as a goal, and stop paying for
  generality. `ThinkingAware` becomes a capability query on the one provider.
- **If a hosted provider is coming**, add it soon. The interface will be wrong in
  a specific way (usage reporting per F1, prompt caching, system-prompt handling,
  parallel tool calls) that only a second implementation reveals.

Doing neither is the only bad option.

**Resolved — and the fork above was a false one.** The roadmap (`README.md`)
carries *"Add different LLM backends — add llama.cpp and vllm"* and *"Model
routing — route different work to different models within one deployment"*. Both
of those backends are **self-hosted**, so local-first is a commitment **and** the
`Provider` interface is justified — by plurality *inside* local-first, not by an
anticipated hosted provider. This review assumed the two were mutually exclusive;
they are not. Recorded as **G8** with **N5** as its non-goal, and in R-LLM.1.
`ThinkingAware` stays: llama.cpp and vLLM differ from Ollama in reasoning
support, which is exactly the variation it exists to absorb.

**Two consequences this exposes, neither of which F9 asked about:**

1. **`max_concurrent` is a per-backend property, and the queue has one.**
   `Queue` holds a single `provider` and a single `maxConcurrent`
   (`internal/llm/queue.go:49`), and R-LLM.4 specifies one `[llm].max_concurrent`.
   A llama.cpp process on one GPU and a vLLM server with continuous batching have
   very different sane slot counts, and one global number cannot express both.
   Model routing is therefore a **queue** change, not merely a second adapter —
   and I2 ("the LLM is reachable only through the queue") means a router must sit
   *behind* the queue or *be* it, never beside it. Design not yet written.
2. **The unknown-provider fallback becomes a hazard.** `config/factory.go:218`
   logs *"unknown [llm].provider; using ollama"* and builds Ollama anyway. Harmless
   with one adapter; actively misleading the moment an operator writes
   `provider = "vllm"` and silently gets Ollama. Should fail rather than warn once
   a second adapter ships.

**F1 is validated by this, not complicated by it.** All three backends report
prompt/completion token counts, so `Usage{InputTokens, OutputTokens}` (R-LLM.8)
carries across unchanged — and *"zero means not reported"* earns more of its keep
with three adapters than it did with one.

### F10 — `internal/selfmodel` · Low · S

84 LOC, **zero tests** — the only package in the tree with none. It probes
`os.Stat("/.dockerenv")` to print `Runtime: Docker container`, which misreports
under podman and under Kubernetes runtimes that do not create that file, and it
discards the error from `VectorQuery` silently. Environment detection inside
prompt assembly is also the wrong home: the container fact belongs in config,
resolved once at boot.

### F11 — Doc mass and design-note maintenance · Low · M

17,904 lines of `docs/` + `spec/` against ~31,400 lines of production Go. For a
system meant to be reimplementable from its spec this is an asset, not a defect —
§3.1 and §3.2 depend on it. But the write amplification is real (every behavior
change is two or three edits, policed by `/sync-nine`), and one subsystem shows
the cost concretely:

| File | Lines | Role |
|---|---|---|
| `docs/sandboxed-tools.md` | 981 | design rationale, explicitly kept *after* implementation |
| `spec/contracts/toolvm.md` | 677 | normative |
| `docs/writing-sandboxed-tools.md` | 607 | authoring guide |
| **total** | **2,265** | for a 3,583-LOC subsystem |

**Proposal.** Keep normative spec plus authoring guide live. Move *implemented*
design notes — the ones whose own header says "kept as design rationale" — to
`docs/adr/`, frozen: dated, never updated, linked from the contract. This
preserves the reasoning (which is genuinely valuable) without paying to keep it
in sync forever. Candidates today: `sandboxed-tools.md`, `plugin-capabilities.md`,
`roles.md`, `predefined-agents.md`.

### F12 — The plugin-socket flake · Low · S

`CLAUDE.md` documents `TestRegisterPlugin` as a known timing flake that passes on
re-run. That is honest, and permanent amnesty is still the wrong resting state: a
socket-timing flake is usually a missing readiness handshake rather than an
inherent race. Worth one investigation before it becomes load-bearing folklore.

---

## 7. F8 — The conceptual surface · Medium · L

This is the largest architectural observation and it deserves its own section,
because it is the one finding where the right answer might be "no change".

Counting first-class nouns a contributor must hold simultaneously: session (in
four flavors), turn, tool call, goal (plus sub-goal and sub-work), workflow (plus
steps), session plan (plus stages), role, skill, plugin (built-in, user, MCP),
sandboxed tool (developer, generated), job, notification (plus user
notification), journal event (plus subscriber and cursor), checkpoint, memory
(KV, files, vectors, FTS), doc index, standing agent, HITL request, reflection,
supervisor. That is roughly **20**.

The evidence that this is over budget is not aesthetic — it is that the system
says so itself: a **549-line glossary**, and a `CLAUDE.md` that warns "terms like
agent / session / conversation / sub-agent / goal / workflow / role are
overloaded and the distinctions matter."

Two pairs looked collapsible on a first reading. **The merge sketch this section
originally called for has since been written**
([`concept-consolidation.md`](concept-consolidation.md)), and it moved the
finding — so what follows is the corrected version.

**7.1 `goal` + `workflow` — do not merge.** *(Rev 1 of this note proposed these
were "one structure with an `ordered` flag". That was wrong and is withdrawn.)*
The distinguishing axis is not ordering but **autonomy**: a goal owns a session
1:1 (`agentID == goalID`, `stage_pursue.go`), wakes itself on an interval or cron,
and is advanced by that session *between* turns. A workflow has no session, no
scheduler, and no driver — `internal/workflow.Service` is pure record-keeping —
and is advanced by the calling model *inside* a turn. A goal is a machine; a
workflow is a record. Merging them would put a scheduler behind something that
must not have one.

What is genuinely wrong is the *framing*: `spec/overview.md` §3.2 presents them as
siblings ("durable structures imposed over sessions"), and that false parallelism
is the whole reason they read as duplicates. The fix is docs-only — see
`concept-consolidation.md` `C7`.

**7.2 `session plan`/`stage` — an unreachable capability, not dead weight.** All
three plan constructors produce **exactly one stage**, and one of the three
registered kinds (`active`) is a no-op that exists only to give
`loadOrCreatePlan` something to seed. That reads at first like speculative
generality to delete.

It is not. The scheduler underneath is genuinely multi-stage: per-stage last-fire
tracking (`w.idleSince`), per-stage interval-or-cron wake computation
(`stageNextWake`), a timer armed to the **earliest** wake across active stages,
one turn at a time when several are due, and `OnTurnEnd` fanned out to all of
them. What is missing is a *caller* — no profile has two stages — plus a
precedence rule for the three functions that resolve role, delegation, and goal
ownership by first-match, and a fairness fix in `handleIdle`, which returns after
the first due stage in array order and can starve a longer-interval one.

**Multi-stage sessions have since been adopted as a direction**, so the work is to
finish the capability rather than remove it. `concept-consolidation.md` `C1`–`C3`
carries it.

**Recommendation.** Act on `concept-consolidation.md`, not on this section. Five of
its seven changes need **no schema change** and are unblocked today; only the two
deletions (`C5` the `reflections` table, `C6` `goal.subtree`) wait on **F4**.

---

## 8. Sequencing

Ordered by leverage-per-unit-risk, not by severity alone.

| # | Item | Why here |
|---|---|---|
| 1 | ~~**F1** — provider usage + estimate reconciliation~~ **done** | Small, self-contained, and it instruments the system's headline constraint. Everything else is easier to reason about once budget error is measurable. |
| 2 | **F4** — migration step runner | Small, and its value is entirely in being written *before* it is needed. Cheapest insurance in the list. |
| 3 | **F3 + F5** — assembly refactor and Loop hooks, together | F5 is most of what makes F3 large; done as one change they delete a hook, a slash command's rationale, and ~200 lines of wiring. |
| 4 | **F2 (step 1)** — typed `MsgType` constants | Mechanical, removes the literal-typo class, and makes F6 tractable. |
| 5 | ~~**F9** — decide local-first, then document or add a provider~~ **done** — decided: local-first *and* multi-backend (G8/N5) | A decision, not a build. Blocks nothing, unblocks F1's shape. |
| 6 | **F6** — protocol and client tests | Follows F2 naturally; typed messages make the tests worth writing. |
| 7 | **F10, F12** | Small hygiene; fold into whatever branch is nearby. |
| 8 | **F2 (steps 2–3)**, **F7**, **F11** | Larger, independent, and none is urgent. |
| 9 | **F8** — the seven changes in [`concept-consolidation.md`](concept-consolidation.md) | Five of the seven need no schema change and can start now; only the two deletions (`C5`, `C6`) wait on F4. The `stage`→`aspect` rename is deliberately last. |

---

## 9. Remarks

- **Zero `TODO`/`FIXME` across ~31,400 LOC of production Go** is the strongest
  quality signal here. Combined with `Status:` headers, "Decisions taken" lists,
  and "Open questions: **None outstanding**", this repo carries an unusually low
  load of undocumented intent. That is worth more than any individual design
  choice in it, and it is the thing most easily lost by moving fast on §8.
- The eval scenarios in `docs/plugin-capabilities.md` §8 assert that the model
  **chooses a posture** (wait vs. move on) rather than that the mechanism runs.
  That distinction is the genuinely hard part of agent systems and most projects
  never reach it.
- The recurring pattern in every finding above: the *concepts* are sound and the
  *seams* are where the rigor thins — wire types, assembly wiring, schema
  evolution, token accounting. That is a good failure mode to have, because seams
  are fixable without redesign. It is also a warning: seams are exactly what rots
  quietly, because nothing in the docs describes them.

---

## 10. Decisions taken

- **This note is non-normative and does not move any `R-*` requirement.** It is a
  review; the changes it proposes will each carry their own spec impact when they
  land.
- **Findings carry stable IDs** (`F1`…`F12`) so a roadmap entry can cite one
  rather than restating it, and so a closed finding stays traceable.
- **Severity and effort are separate axes.** F7 and F8 are Medium impact and
  Large effort; F1 and F4 are High impact and Small effort. The sequencing in §8
  follows the ratio, not the impact column.
- **§3 is a constraint on §5–§7, not decoration.** Any proposal that would erode
  the embedded spec, the pull-not-push discipline, the two-way capability check,
  or self-improvement-as-data is out of scope regardless of what it fixes.
- **F8's defined exit has been taken.** Rev 1 deferred it with the condition
  "write the merge sketch, then either act or record why not". The sketch is
  [`concept-consolidation.md`](concept-consolidation.md): it withdraws the
  `goal`+`workflow` merge with reasons (§7.1) and relocates the finding to
  `stage`, with four moves and a phase order.
- **Two owner decisions were taken against that sketch**, and both narrowed it:
  **multi-stage sessions are wanted** (so the capability is completed, not
  collapsed), and **reflection history is bounded** (no scrub exemption — the
  durable product of a reflection turn is the `self/*` KV write, which nothing
  scrubs). See `concept-consolidation.md` `C5` and §4.

---

## 11. Open questions

1. ~~**Is local-first a goal or a stopgap?**~~ **Answered: a goal** (F9) — and the
   question was mis-posed. Local-first and a plural `Provider` interface are not
   alternatives, because the planned second and third backends (llama.cpp, vLLM)
   are self-hosted. See **G8**/**N5**. What replaces it as open: **how model
   routing divides `max_concurrent` per backend without breaking I2** — the queue
   holds one provider and one slot count today.
2. ~~**Does the `goal` + `workflow` merge survive the `workflow_*` tool
   surface?**~~ **Answered (rev 2): no, and the merge is withdrawn** — the axis is
   autonomy, not ordering (§7.1). Of the three questions it raised in turn, two
   are now settled — multi-stage is wanted, reflection history is bounded — and
   the remainder are `concept-consolidation.md` §5: what the second aspect
   concretely is, whether the `stage`→`aspect` rename earns its churn, and whether
   an operator may delete the reflection aspect from the default profile.
3. **Should implemented design notes stay in `docs/` and stay embedded?** (F11.)
   Freezing them to `docs/adr/` trades `nine docs` discoverability for a smaller
   sync burden; which side that lands on depends on how often the rationale is
   actually read at runtime.
