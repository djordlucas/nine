# Critique — feature set, limits, and positioning

- **Status:** Assessment, 2026-09-26. Non-normative: this note records a
  judgement, not a contract. Nothing here changes behavior, so no `spec/`
  requirement moves on account of it. Findings carry stable IDs (`C1`…`C12`) so
  they can be lifted into the roadmap without being restated.

  **Update, 2026-09-27:** `C5` and `C12` are closed and `C4`'s default-posture half
  is closed, by `capability-grants.md`. A struck-through row keeps its ID rather
  than being deleted — the record is what was found, not what is still open.
- **Date:** 2026-09-26.
- **Scope:** the shipped surface as of `c572c92` — `README.md`, all 38 documents
  under `docs/` including every `## Limits` section, `spec/contracts/`,
  `adr/README.md`, and the `internal/` package tree by line count.
- **Method:** read the documentation first and measured it against the tree
  second. Every claim below cites a document, a file, or a count. Where a
  document and the code disagree, the code is reported and the disagreement is
  itself a finding.
- **Purpose:** separate what is differentiated and must not be eroded (§2) from
  what is incomplete (§3), and state where Nine should be positioned (§4).

---

## 1. TL;DR

Nine's four differentiated assets — the journal, single-binary/single-file
deployment, the operator-owned capability ceiling, and the small-model baseline
— are all about **operating** an agent, not authoring one. The positioning
should follow them (§4). The largest defect is that the features most likely to
harm an adopter are the ones with no eval coverage (`C1`), and the second is
four subsystems whose concept is built and whose loop is not closed (`C2`).

| ID | Finding | Impact | Effort |
|---|---|---|---|
| **C1** | Eight feature rows have no eval case; the `hitl` and `safety` tiers have none at all | **High** | M |
| **C2** | Workflows, the supervisor, `gap_report` and subscriptions are each built with nothing consuming the result; a workflow step carries no link to the sub-agent running it | **High** | M |
| **C4** | ~~The capability boundary covers only the tier that is off by default~~ **half closed** — both tiers now default on with a workspace ceiling (`capability-grants.md`); `shell`, `files` and plugins still run with the daemon's full reach | **High** | L |
| **C5** | ~~The gap → tool → capability path dead-ends at an operator TOML edit plus a restart~~ **closed** — `capability_request` plus `nine grants`, applied to the running daemon (`capability-grants.md`) | Medium | M |
| **C6** | Autonomy is timer-driven; the journal subscription machinery that would make it event-driven drives one subscriber | Medium | M |
| **C7** | `internal/llm/openai/` is an empty placeholder, and the adapter it would hold is the highest-leverage roadmap row | Medium | S |
| **C11** | Workflow dependency gating is advertised in `README.md`, implemented, tested, and unreachable from any tool | Medium | S |
| **C12** | `TestEveryClientMsgTypeIsDispatched` reads a silent connection as "routed", so a field-less wire verb with no handler ships — **closed**, `TestEveryQueryKindIsAnswered` (`capability-grants.md` §5) | Medium | S |
| **C8** | `internal/api` is the least-tested large package (8443 source / 1876 test lines) and six of its endpoints return 501 | Medium | M |
| **C3** | `docs/daemon.md` misstates supervisor event coverage in both directions | Low | S |
| **C9** | `README.md` §Security contradicts §Limits on whether the API has authentication | Low | S |
| **C10** | The token-estimate ratio is documented two different ways | Low | S |

## 2. What is differentiated

These four are the assets a competing project cannot cheaply copy, and the ones
a roadmap should be measured against.

| Asset | What it is | Why it is rare |
|-------|-----------|----------------|
| The event journal | Append-only record of every step: turn boundaries, exact LLM request and response, tool I/O with latency and errors, context usage, sub-agent lifecycle. Written off the critical path, subscribable with durable per-subscriber cursors, read back by `nine trace` and `nine replay`. | No other local agent runtime can reconstruct what happened after the fact. It is one README bullet and should be a chapter. |
| One binary, one file | `dist/nine` is the CLI, the TUI, the daemon and the four built-in plugins; all durable state is one SQLite file; every turn is checkpointed, so a killed daemon resumes with the same history and plan. | "Deploying Nine is copying one file" is literally true. Framework competitors are libraries whose state dies with the process. |
| The operator-owned capability ceiling | A generated tool's code is the agent's; its capability grant is the operator's, written only in `nine.toml`. `tool_write` writes a declaration, never a grant. Duration (`allow_long_running`, `allow_standing`) is gated separately from reach, because the ceiling cannot express it. | The correct factoring for self-modifying agents, and the part comparable projects get wrong. |
| The small-model baseline | Developed against `qwen3.5:4b`-class models on a 16 GB M4, which forced context to be a budget with competing priorities and tool definitions to be ranked by embedding similarity under a top-N cap. | The discipline transfers upward to large models. The reverse does not. |

A fifth asset is process rather than product: every document ends with a
specific, honest `## Limits` section, and `/sync-nine` keeps the embedded copies
true per binary. That is why this critique took one pass. For a single-author
project it is a competitive advantage and should not be allowed to slip.

## 3. Findings

### C1 — Eight feature rows have no eval case

`docs/evals.md` §Limits records that the 28-case live corpus covers nothing for
HTTP/web fetching, HITL (`ask_human`), the approval gate, `gap_report`, stall
detection, safety (`rm -rf`, SSRF to loopback), context-budget pressure, or
related-session surfacing, and that "the `hitl` and `safety` tiers have no cases
at all, so those two tier names are aspirational."

The covered cases are the benign ones — `shell-echo`, `kv-roundtrip`,
`files-write-read`. The uncovered ones are where an adopter gets hurt: a shell
tool that should have been refused, an SSRF that should have been blocked, an
approval gate that silently passes. Every other claim Nine makes about safety is
untested until these exist. This gates `C4` and is the first thing to do.

### C2 — Four subsystems with no consumer

Each is built, documented, and reaches no conclusion. Quoted from the shipped
docs:

| Subsystem | The gap |
|-----------|---------|
| Workflows | "Nothing drives a workflow… It is a record of intent, not a scheduler." (`docs/workflows.md`) |
| Supervisor | `EventGoalStalls`, `EventGapReported` and `EventAgentCompletes` "are delivered and logged, and nothing acts on them." (`docs/architecture.md`) |
| `gap_report` | The tool exists and posts an event (`internal/runtime/builder.go:758`); nothing consumes the gap. |
| Subscriptions | "One subscriber ships" — session linking. (`docs/event-journal.md`) |

The result is a supervisor that supervises nothing and a workflow engine that
does not execute. Closing two of these adds more capability than any new
subsystem, and the machinery is already in place: `internal/runtime/supervisor.go:131`
is a switch arm with a comment where the reaction goes.

A workflow's passivity is deliberate and documented — "it has no session, no
scheduler, and no driver", the axis separating it from a goal being autonomy
rather than ordering (`docs/workflows.md`). The defect is not the missing
scheduler. It is that within the passive design the bookkeeping is unassisted in
two specific ways:

| Gap | Evidence |
|-----|----------|
| The ledger has no structural link to the work it tracks | `run_agent`'s schema is `task`, `context`, `role` (`internal/agent/register_subagents.go:53`). It carries no `workflow_id` and no `step_id`, so nothing connects a step to the sub-agent executing it, and nothing notices when that sub-agent finishes. |
| First attempts are hand-bookkept; retries are fully automated | `workflow_retry_step` performs reset → mark `running` → `spawnFn` → mark `done`/`failed` in one call, including the failure branch (`internal/agent/register_workflow.go:128-150`). The first run of the same step needs a separate `run_agent` plus two `workflow_update` calls, by convention. |

So a step opened by a model that then loses the thread — across turns, under a
context budget that trims history from the front — leaves a workflow `active`
with `pending` steps indefinitely. That is why `nine workflow fail` is
load-bearing: it is the cleanup path for unassisted bookkeeping.

The cheapest fix is already written. A `workflow_run_step` doing on the first
attempt what `workflow_retry_step` does on the second would reuse the same
`spawnFn` already passed into `RegisterWorkflowTools`, collapse four model-driven
calls into one, and leave the passive design untouched — the model still decides
when to advance, it just stops maintaining the ledger by hand.

### C3 — `docs/daemon.md` misstates supervisor coverage

`docs/daemon.md` §Limits states "All four events are delivered and logged;
nothing acts on them." The code splits three ways:

| Event | Emitted | Handled |
|-------|---------|---------|
| `EventGoalStalls` | `internal/runtime/daemon.go:751` | no — falls into the no-op arm at `supervisor.go:131` |
| `EventGapReported` | `internal/runtime/builder.go:758` | no — same arm |
| `EventAgentCompletes` | `internal/runtime/handlers.go:251` | no — same arm |
| `EventPluginCrashed` | never — "exists in the supervisor but nothing emits it yet" (`internal/builtins/mcp.go:502`) | has its own arm, which documents that recovery is the plugin manager's |

So one event is handled and never emitted, and the doc is wrong about all four.
`docs/architecture.md` has it right with three.

### C4 — The capability boundary covers the tier that is off by default

**Half closed 2026-09-27** ([`capability-grants.md`](capability-grants.md)): both tiers
now default on with a workspace ceiling, so the boundary guards the tier that runs.
The other half stands — `shell`, `files` and every plugin still run as the daemon's
process user.

Generated and sandboxed tools run behind a real boundary: default-deny
capabilities, one wazero instance per call, an SSRF-checked HTTP path. That tier
is gated by `[tools.agent] enabled`, which is off by default. The tools the
agent uses to do the work the README advertises — `shell`, `files`, every plugin,
every MCP server — run as the daemon's process user with its full filesystem and
network reach, and the whole mitigation is the container.

The asymmetry is worth naming plainly: the least-used tier is the only one with
a boundary. Extending the capability model to the `fs`/`shell` tier — a `shell`
that runs with granted paths rather than the process user's — would make Nine
the only agent runtime where the entire tool surface is capability-gated. That
is the strongest version of the hardening row on the roadmap, and `C1`'s safety
cases are its precondition.

### C5 — No path from a discovered gap to a granted capability

**Closed 2026-09-27** ([`capability-grants.md`](capability-grants.md)). What follows is
the finding as written. Three segments turned out to be wrong rather than one: the
refusal named an unadvertised tool, nothing persisted a request, and a grant needed a
restart.

The agent can detect a capability gap (`gap_report`), write a tool (`tool_write`),
and then stop: the tool declares a need, and nothing can grant it without an
operator editing `nine.toml` and restarting the daemon
(`docs/self-modification.md`, `docs/configuration.md`).

Operator-owns-the-ceiling is correct and should not change. What is missing is a
*request* primitive: a pending-grant row the agent can write and a
`nine grants` command that lists and approves them. That closes the loop without
weakening the invariant, because the operator still writes every grant.

### C6 — Autonomy is timer-driven

Goal sessions wake on a fixed five-minute interval that is not configurable per
goal (`docs/session-plans.md`). Standing agents are cron at minute granularity,
with no backfill across restarts, one clock trigger per routine, local timezone
only, and wakes that drop when one is already queued (`docs/scheduling.md`).
"Autonomous" therefore means polling.

The journal plus subscriptions is the machinery for event-driven wake, and it
serves one subscriber. The "no generative subscribers" constraint is deliberate
and worth keeping — but *scheduling a wake* is not a generative call, so a
subscriber that wakes a goal session on a matching journal entry fits inside the
constraint exactly as written.

### C7 — The OpenAI-compatible adapter is an empty directory

`internal/llm/openai/` contains a single `.gitkeep` dated 2026-05-19. The
provider factory (`internal/config/factory.go:228`) registers `ollama` and
`mistral` and warns-then-defaults on anything else.

The roadmap notes that "llama.cpp and vLLM both speak an OpenAI-compatible API,
so one adapter covers them." That adapter is the cheapest high-leverage item on
the table: it unlocks llama.cpp, vLLM and LM Studio at once, and it is the only
way to get the large-model measurement that `docs/model-compatibility.md`
§Limits admits is missing.

### C8 — `internal/api` is the least-covered large package

Source and test line counts across the tree, non-test versus test:

| Package | Source | Test | Ratio |
|---------|-------:|-----:|------:|
| `internal/runtime` | 10758 | 11113 | 1.03 |
| `internal/toolvm` | 5257 | 6840 | 1.30 |
| `internal/agent` | 4020 | 4803 | 1.20 |
| `internal/memory` | 4890 | 3647 | 0.75 |
| **`internal/api`** | **8443** | **1876** | **0.22** |

`internal/api` is the second-largest package and the thinnest-tested by a factor
of three, and six of its endpoints — history, trace, replay, goal create, goal
delete, skills — return 501 because the wire protocol does not back them
(`docs/api.md`). It is simultaneously the biggest surface and the least
verified. Its OpenAPI document is also generated from annotations, so an
annotation that drifts produces a spec wrong in the same way as the handler.

### C9 — `README.md` contradicts itself on API authentication

`README.md:715` (§Security) states "The API on port 8080 has no authentication."
`README.md:682` (§Limits) correctly states that a bearer token
(`[api] auth_token`, `--auth-token`, `NINE_API_AUTH_TOKEN`) and TLS are
supported, are off by default, and that Nine warns at startup when it binds a
non-loopback address without a token. `docs/docker-image.md` agrees with §Limits.

`12c167e` added the exposure warning and updated one site. The stale sentence is
in the section a security-conscious reader reads hardest.

### C10 — Two documented token ratios

| Document | Claim |
|----------|-------|
| `docs/architecture.md:1128` | "a 4-characters-per-token approximation calibrated against one tokenizer" |
| `docs/context-builder.md:163` | "a 3.45-bytes-per-token approximation measured against one model family" |

Both cannot be the calibration in force. The number is the basis of every
budgeting and trimming decision, and `adr/architecture-review.md` F1 records
that `Response.Usage` is already journaled alongside the estimate — so the
reconciliation data to settle it exists.

### C11 — Workflow dependency gating cannot be reached from any tool

`README.md` §Architecture states that "a workflow is a finite, in-session
multi-step plan with **dependency gating** and auto-close that agents generate
and follow." Auto-close is real: `terminalStatus` derives the workflow status
from its steps and is exercised through `Update`. Dependency gating is
unreachable in production.

| Site | State of `Step.DependsOn` |
|------|---------------------------|
| `internal/workflow/workflow.go:24` | Declared on `Step` as `depends_on,omitempty` |
| `internal/workflow/workflow.go:136` | `Create` hardcodes `DependsOn: []string{}` for every step |
| `internal/agent/register_workflow.go:17` | `workflow_create`'s schema takes `steps` as a flat `array` of `string` — no per-step object, so no caller can express a dependency |
| `internal/workflow/workflow.go:178` | `Update`'s gate reads the field, resolving `failed`/`skipped` dependencies to `skipped` and refusing to start a step whose dependency is still `pending`/`running` |
| `internal/workflow/workflow_test.go:327,346,376` | The only writers, assigning `w.Steps[1].DependsOn` directly and bypassing the tool layer |

The gating logic is correct and covered by tests. Nothing can populate the field
it reads, so the branch never executes outside those tests. Adding a
`depends_on` array to `workflow_create`'s per-step schema lights up code that
already exists.

This is where `C1` and `C2` meet: the tests pass because they write the struct
directly rather than calling the tool. An eval case at the tool boundary — the
coverage `C1` is about — is what distinguishes tested code from reachable code.

## 4. Positioning

**Stop describing Nine as a runtime for building agents. Describe it as what you
run when an agent must work unattended and be accountable afterward, on hardware
you control.**

"An AI agent runtime" places Nine in the general-framework category against
LangGraph, Letta, and the vendor agent SDKs. In that category Nine loses on
model access, ecosystem and newcomer documentation, and it cannot win: it is
single-author and accepts no external pull requests by deliberate choice
(`README.md` §Contributing). None of the four assets in §2 are about authoring
an agent. All four are about running one accountably.

Three framings carry that, ordered by how much of the code already exists:

| # | Framing | Existing basis | What it needs |
|---|---------|----------------|---------------|
| **a** | **The auditable agent runtime** — the only one that can prove what it did | The journal: exact LLM request/response, tool I/O with latency, context usage, sub-agent lifecycle, replayable | Journal export (OTel spans or JSONL), a `nine diff` between two replays, optionally signed journal segments. Audience: anyone who must explain an agent's actions after the fact. GPL-3 helps here — it repels the embed-in-a-SaaS use Nine does not want. |
| **b** | **The self-hosted standing-agent host** — "watch this and tell me", on one box | Standing agents, cron, notifications, one container, one file, a 4B model on 16 GB | Notification sinks beyond `nine notifications` (webhook, ntfy, email), a handful of shipped `nine.toml` recipes, and `C6`'s event-driven wake. This is the cheapest route to the user mileage `README.md` §Project status names as the one thing missing. |
| **c** | **Capability-governed host for the whole tool surface** | The ceiling model, correctly factored, over generated tools | `C4`. Hardest and most defensible; a multi-quarter bet that retires the hardening row properly. |

Run **b** to buy mileage, state **a** as the identity, treat **c** as the long
bet. **a** and **b** are one story told to two audiences: an agent that runs
unattended and can prove what it did.

### Two roadmap rows to defer

| Row | Why defer |
|-----|-----------|
| Model routing, and any multi-host story | The generic-framework path. It multiplies surface area while eight feature rows have no tests (`C1`). Single host and one file are features for both **a** and **b**, not limitations. |
| "More built-in plugins" | The catalog already outgrew the context budget, which is why ranking under a fixed top-20 cut exists at all (`docs/tool-selection.md`), and why `docs/browser.md` advises declaring its 24-tool server only on instances that browse. More tools makes the hardest problem harder. |

## 5. Sequencing

1. **`C1`** — eval cases for the eight uncovered rows, `safety` and `hitl`
   first. Everything else is a claim until these exist.
2. **`C7`** — the OpenAI-compatible adapter. Fills the empty directory, unlocks
   three runtimes, and enables the large-model measurement.
3. **`C2`, `C11`** — close the workflow loop and one supervisor arm. A
   `workflow_run_step` reuses the driver `workflow_retry_step` already contains,
   and a `depends_on` field on `workflow_create` reaches gating that is written
   and tested. Then take the supervisor's `EventGoalStalls` and
   `EventGapReported` arms: they turn the supervisor from a monitor into a reason
   Nine exists.
4. **Framing b** — journal export and notification sinks. Cheap, and they are
   the two edges where Nine cannot currently reach the outside world.
5. **`C3`, `C9`, `C10`** — the three documentation defects. Small, and two of
   them are in the places a reader trusts most.
6. **`C5`** — the pending-grant path.
7. **`C4`** — the capability boundary over `shell` and `files`, once `C1`'s
   safety cases can measure it.

## Limits

| Limit | Detail |
|-------|--------|
| Documentation-led, not execution-led | The assessment read `docs/`, `spec/` and package line counts. It did not run `make ci`, `make eval-live`, or the container contract tests, so it inherits any error the documents carry that the code does not. |
| Not a code review | No package was read end to end. Findings that cite a file cite it as evidence for a documented claim, not as the result of reviewing that file. `C8` is a line count, not a coverage measurement. |
| No user data | Nine has no user mileage (`README.md` §Project status), so the positioning in §4 rests on what the assets are, not on observed demand. |
| Line counts are unweighted | Counts are raw lines of `*.go` under `internal/`, excluding `vendor/`. Comments, blank lines and any generated code count as source, so the `C8` ratio is an indicator rather than a measurement. |
| Effort columns are estimates | S/M/L follow the convention in `adr/architecture-review.md` and are unvalidated guesses. |
