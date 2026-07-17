# Evals — testing Nine with replays and live models

**Status:** Design note / working spec · **Purpose:** define how we test Nine's
behavior end-to-end against real (and recorded) LLM infrastructure, and specify a
**machine-usable case schema** precise enough that new test cases can be generated
from this document — by a person or by an LLM — to cover features and guard
against regressions.

There are **two tracks**, and every case declares which it belongs to:

- **Track R — Replay (deterministic, no live model).** A session recorded once is
  re-executed from its journal against a *recorded* provider/dispatcher — no live
  LLM or tool calls. Reproduces the exact recorded behavior. Cheap, fast,
  deterministic; runs on every PR. Guards the loop / dispatcher / context-builder
  code, **not** the model.
- **Track L — Live model (behavioral).** The same request run against a real model
  (or a matrix of models). Non-deterministic; graded with tolerant, trajectory- and
  side-effect-based assertions over N runs. Runs nightly / on demand. This is where
  feature coverage and cross-model behavior are tested.

A single case may target `both`: its Track-L recording becomes the Track-R fixture.

---

## 1. Principle: grade the trajectory, not the prose

Two models phrase the same correct outcome completely differently, so **never grade
free text with exact matches.** Nine records every session to a durable event
journal (`session_events`, see [event-log.md](event-log.md)); a case is graded by
reading that journal plus real side-effects. Prefer assertions in this order (most
robust first):

1. **Side-effects** — a file exists with expected content, a `kv` key holds a value,
   a `workflows` row reached `done`, a pursue session was spawned, a notification
   was posted. Model-independent.
2. **Trajectory** — the right tool was called (and the wrong one wasn't), turn count
   within bounds, no stall, `gap_report` fired only when expected, an allowlist role
   never advertised a forbidden tool.
3. **Answer (tolerant)** — the final text contains a required number/substring or
   matches a regex.
4. **Judge** — an LLM-as-judge rubric, only for open-ended answers where 1–3 can't
   apply. Least trusted (see §7).

---

## 2. Case schema (the generation contract)

Cases are **data** (`tests/evals/cases/*.yaml`), not code, so the suite scales
without recompiling and cases can be generated mechanically. One YAML document = one
case. All fields:

```yaml
id: memory-roundtrip                 # REQUIRED, unique, kebab-case
description: "Store a fact, then recall it in a later turn."
track: both                          # replay | live | both
tier: basic                          # smoke | basic | multi_step | delegation | safety | hitl
tags: [memory, kv]                   # free-form, for filtering/reporting

# ── Fixtures applied to an isolated store + workspace before the run ──
setup:
  files:                             # written into the session workspace
    /work/data.txt: "alpha\nbeta\n"
  kv:                                # pre-seeded key/value memory
    self/region: "us-west-2"
  skills:                            # pre-seeded agent skills (name: body)
    deploy-notes: "# Deploy\n..."
  goals: []                          # pre-seeded goals (rarely needed)

# ── The request. Each string is one user turn on the same session. ──
prompts:
  - "Remember my prod DB host is db.prod.example.com"
  - "What is my prod DB host?"

# ── Session configuration (all optional) ──
session:
  role: executor                     # orchestrator | executor | report-writer | …
  interactive: false                 # enables HITL tools; requires an answers script
  config:                            # per-case nine.toml overrides
    related_sessions_index: false

# ── HITL script: canned human answers, matched in order to ask_human calls ──
human_answers: []                    # e.g. ["yes", "the staging cluster"]

# ── Assertions. A case passes when ALL listed assertions hold. ──
expect:
  side_effects:
    files:
      /work/out.txt: { contains: "db.prod.example.com" }
    kv:
      self/prod_db: { equals: "db.prod.example.com" }   # or { matches: "..." }
    workflows: { status: done, min_steps: 3 }
    goals: { created: 1, pursue_spawned: true }
    notifications: { min: 1 }
    vectors: { namespace: skills, min: 1 }
  trajectory:
    tools_all_of: [memory_set]        # every one MUST appear
    tools_any_of: [memory_get]        # at least one MUST appear
    tools_none_of: [shell, run_agent] # none may appear
    max_turns: 2
    min_turns: 1
    no_stall: true                    # no turn ended with ErrStall
    gap_report: false                 # true = expect a gap_report
    sub_agents: { count: 0 }          # spawned sub-agent count (exact or {min,max})
    llm_request:                      # assert over the assembled prompt sent to the model
      system_contains: []             # substrings that MUST be present
      system_not_contains: []
      tool_advertised_none_of: [shell]  # tool never appears in tool_names (role gating)
  answer:
    contains: ["db.prod.example.com"] # all substrings present (case-insensitive)
    matches: "db\\.prod\\.example\\.com"
    not_contains: ["error", "cannot"]
    judge:                            # optional LLM-as-judge
      rubric: "The answer correctly states the prod DB host."
      model: claude-sonnet-5          # a DIFFERENT, stronger model than under test
      pass_score: 0.8

# ── Live-run controls (Track L) ──
runs: 3                               # repetitions per model
pass_threshold: "2/3"                 # fraction that must pass to count the case as passing
timeout_seconds: 120

# ── Model applicability ──
models:
  include: [claude-haiku, claude-sonnet, gemma4:e4b, qwen3.5:9b]
  # Expected to pass only on these classes; below-class failures are reported, not fatal.
  expected_pass_min_class: small      # nano | small | medium | large  (see §6)
```

Every field except `id` and `prompts` has a sane default (empty / off). A minimal
case is `id`, `track`, `prompts`, and one assertion.

---

## 3. Assertion reference (how each is checked)

All journal reads are `store.SessionEventsByAgent(agentID)` (or `nine trace`), which
returns events in `seq` order with these payloads (see `internal/runtime/journal.go`):

| Event | Key payload fields | Used to assert |
|-------|--------------------|----------------|
| `turn_start` | `input`, `trigger` (`user`/`idle`) | turn boundaries |
| `llm_request` | `system`, `messages`, `tool_names`, `tokens_used`, `budget` | prompt/role gating, context budget |
| `llm_response` | `text`, `tool_calls`, `stop_reason` | model intent |
| `tool_start` | `name`, `input` | which tool, with what args |
| `tool_end` | `name`, `input`, `output`, `truncated`, `duration_ms`, `attempts`, `err` | tool result / failure |
| `turn_end` | `result`, `error`, `tool_count`, `duration_ms` | answer, per-turn tool count, stall (`error`) |

Mappings:

- **`tools_all_of` / `any_of` / `none_of`** → the set of `tool_start.name` across the session.
- **`max_turns` / `min_turns`** → count of `turn_end` (user-triggered) events.
- **`no_stall`** → no `turn_end.error == "stall"` and no `supervisor` event of kind stall.
- **`gap_report`** → a `tool_start.name == "gap_report"` (or the supervisor `gap_reported` event).
- **`sub_agents.count`** → `sub_agent_start` progress events (or `run_agent`/`run_agents` tool calls).
- **`llm_request.tool_advertised_none_of`** → the tool name never appears in any `llm_request.tool_names` (proves role gating at the advertised boundary, R-ROLE.4).
- **`side_effects.*`** → read the isolated store/workspace *after* the run: `kv` via
  `store.Get`, files via the workspace dir or `file_fetch`, `workflows` via
  `store.WorkflowList`, `goals` via `store.GoalList` + a pursue session in `nine status`,
  `notifications` via `store.UserNotificationList`, `vectors` via `store.VectorQuery`.

---

## 4. Track R — deterministic replay

**Record once, replay forever.** For any case tagged `track: replay` or `both`:

1. Run it live once against a chosen model; capture the journal
   (`SessionEventsByAgent`) into a fixture: `tests/evals/replay/<id>/journal.json`.
2. In CI, load it with `internal/replay.FromEvents(events) → Recorded`, build a loop
   wired to `rec.Provider()` (returns the recorded `llm_response`s in order) and
   `rec.Dispatcher()` (returns the recorded `tool_end` outputs), and run
   `replay.Session(ctx, rec)` (equivalently `nine replay <id> --turn N`).
3. Assert the **reproduced** answer/trajectory equals the recorded one.

This makes **zero** API/tool calls, is fully deterministic, and runs on every PR. It
catches regressions in the ReAct loop, dispatcher routing, context assembly, and
journal round-trip — the parts *we* own. It does **not** catch model regressions
(that's Track L). Re-record a fixture deliberately (a reviewed change) when the
recorded behavior should change.

---

## 5. Track L — live models

Run the case against each applicable model, `runs` times, and mark it passing if the
pass fraction ≥ `pass_threshold`. Everything in §1–§3 applies. Requirements:

- **Isolation per run**: a fresh session id, a schema-per-run Postgres (reuse the
  `internal/memory/memtest` pattern), and an ephemeral workspace dir. Cases must not
  see each other's memory/goals/files.
- **Variance reduction**: temperature 0 and a fixed `seed` where the provider
  supports it (Ollama does; Anthropic is best-effort).
- **Driving turns**: `nine send [--id <id>] <prompt>` (the id is printed to stderr as
  `id=<agent-id>`) or the protocol client `c.Turn(id, text)` as the existing
  `tests/integration` tests do.

---

## 6. Model matrix & capability tiers

Run the same cases across a set and report a **cases × models** grid.

| Class | Example models | Reasonable expectation |
|-------|----------------|------------------------|
| `nano` | `gemma4:e4b`, `llama3.2:3b` | single tool call; simple recall |
| `small` | `qwen3.5:9b`, `llama3.1:8b` | reliable tool use; 2–3 step tasks |
| `medium` | `claude-haiku`, `gemma4:12b` | multi-step, basic delegation |
| `large` | `claude-sonnet-5`, `claude-opus` | delegation, workflows, HITL, judging |

A case's `expected_pass_min_class` sets the bar: a failure **at or above** that class
is a real failure (fatal to the run); a failure **below** it is reported but expected.
This keeps small local models in the matrix (useful signal on tool-calling quality)
without failing the suite because a 3B model can't do 3-hop delegation. Per-model
report metrics: task success rate by tier, avg turns, tool-call accuracy, stall rate,
p50/p95 latency, tokens.

---

## 7. Non-determinism, judging, and pitfalls

- **N runs + thresholds**, never a single binary run. Report the pass fraction.
- **Tolerant assertions only** (side-effect > trajectory > regex > judge). No exact
  free-text matches.
- **LLM-as-judge**: use a *stronger, different* model than the one under test;
  demand structured output (`{score, reason}`) against an explicit rubric; validate
  the judge against ~20 human-labeled samples before trusting it; keep judged cases a
  minority.
- **Pitfalls**: Postgres is fail-fast (evals need it up — compose provides it);
  browser cases need Chromium (gate them); small local models are genuinely flaky at
  multi-step (that's what `expected_pass_min_class` is for); the `nine send` id line
  goes to **stderr**.

---

## 8. Layout, running, reporting

```
tests/evals/
  cases/            # *.yaml — the generated case corpus (§2)
  replay/<id>/      # recorded journal fixtures for Track R (§4)
  runner/           # Go harness: loads cases, isolates state, drives turns, asserts
  reports/          # JSON + rendered matrix per run
```

- **CI (every PR)**: Track R (all replay fixtures) + the `smoke` tier on one cheap
  model. Free/fast, deterministic gate.
- **Nightly / on-demand**: the full Track-L matrix; write `reports/<date>.json` and a
  rendered cases × models grid; track **pass-rate trend per model/tier** — a drop is
  the regression signal regardless of cause (model swap, prompt edit, code change).
- Build on `tests/integration/setup_test.go`'s existing harness (Docker + Ollama
  bring-up, `NINE_LLM_MODEL` selection) rather than starting fresh.

---

## 9. Feature-coverage checklist (the generation map)

Generate cases until every row has ≥1 case per relevant tier. "Assert via" names the
strongest available assertion for that feature.

| Subsystem | Behavior to test | Assert via | Tier |
|-----------|------------------|-----------|------|
| time plugin | current time | `tools_all_of:[time]` + answer regex | smoke |
| shell | run a safe command | `tools_all_of:[shell]` + answer | smoke/basic |
| files (plugin) | read/write a path | side-effect: file content | basic |
| files (store) | `file_store`→`file_search_text` | side-effect + tool trajectory | basic |
| http/web | fetch/search a page | `tools_any_of:[http_get,web_search,web_page_read]` | basic |
| KV memory | set→get roundtrip; list | side-effect `kv` + answer | basic |
| semantic memory | `memory_embed`→`memory_query` | side-effect `vectors` | multi_step |
| skills | write a skill, use it next session | `skill_write` then `skill_read` | multi_step |
| workflows | multi-step task auto-closes | side-effect `workflows.status=done` | multi_step |
| sub-agents | `run_agents` fan-out | `sub_agents.count` ≥ 2 | delegation |
| goals | open-ended request | side-effect goal + pursue session | multi_step |
| roles | report-writer denied `shell` | `llm_request.tool_advertised_none_of:[shell]` | basic |
| HITL | needs clarification | `ask_human` fired; resumes on `human_answers` | hitl |
| approval gate | gated `shell` needs approval | approval prompt; blocked on "no" | hitl |
| supervisor/gap | request no tool can satisfy | `gap_report: true` | basic |
| stall | contrived no-progress loop | supervisor stall event | basic |
| safety | `rm -rf /`, SSRF to loopback | tool blocked / errored | safety |
| context budget | very long input | `no_stall` + answer; memory-offload | multi_step |
| related sessions | two linked topics | later turn surfaces prior (see reactive-events) | multi_step |
| replay | any recorded case | reproduced answer == recorded (Track R) | all |

---

## 10. Generating cases from this document

To add coverage, or to have an LLM expand the corpus:

1. Pick a **feature row** (§9) and a **tier**.
2. Instantiate the **schema** (§2): write `prompts` that force the behavior, seed only
   the `setup` you need, and choose the **strongest** assertion the feature allows
   (§1 order) — prefer a `side_effects` or `trajectory` check over `answer`.
3. Set `track` (`replay` once a fixture is recorded, else `live`), `runs`/
   `pass_threshold`, and `expected_pass_min_class`.
4. Keep each case **single-purpose** and **isolated** (assume a clean store/workspace).
5. Drop it in `tests/evals/cases/<id>.yaml`. For Track R, record the fixture once.

**Prompt for an LLM generator** (feed it this document):

> Using the case schema in §2 and the feature-coverage map in §9 of docs/evals.md,
> generate N new eval cases for `<subsystem>` at tier `<tier>`. Each case MUST: force
> the target behavior via `prompts`; assert with the strongest available check
> (side-effect > trajectory > answer); be self-contained given a clean store and
> workspace; set realistic `expected_pass_min_class`. Output valid YAML, one document
> per case, ids kebab-case and unique. Do not assert on exact free-text wording.

---

## Reference symbols

- Journal + read: `internal/memory/events.go` (`SessionEventsByAgent`, `SessionEvent`),
  `internal/runtime/journal.go` (payload structs), `nine trace`.
- Replay: `internal/replay/replay.go` (`FromEvents`, `Recorded`, `Provider`,
  `Dispatcher`, `Session`), `nine replay`.
- Driving turns: `nine send` (`internal/cli`), `protocol.Client.Turn`.
- Isolation: `internal/memory/memtest` (schema-per-test Postgres).
- Existing harness: `tests/integration/setup_test.go` (Docker + Ollama bring-up),
  `make integration-test`.
- Models/config: `NINE_LLM_PROVIDER`/`NINE_LLM_MODEL`/`NINE_LLM_ENDPOINT`,
  `internal/llm/{anthropic,ollama}`.
