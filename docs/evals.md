# Evals

How Nine is tested end-to-end against real — and recorded — model
infrastructure. The case schema below is deliberately precise enough that new
cases can be generated from this document, by a person or by a model, to cover
features and guard against regressions.

Run them with `make eval-replay` and `make eval-live`; `make eval-code` runs the
cases about code the model writes on models expected to pass them.

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
journal (`session_events`, see [event-log.md](event-journal.md)); a case is graded by
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

**Corollary: don't accidentally grade something else.** A case must fail for the reason
it names. The `tool-output-spill` case originally asked the model for "the last number"
of `seq 1 20000`; every run spilled correctly and stored all 128 KB, yet the case scored
0/3 because finding one integer among ~8000 newline-escaped integers in a single-line
JSON blob grades *numeric attention*, not spilling. Ending the output with a distinctive
token instead tests the identical mechanism and passes. When a case fails, check the
per-run failure list: if the mechanism assertions passed and only `answer` failed, the
case is probably mis-designed rather than the feature broken.

**Corollary: assume the model will game the setup.** Three separate live runs of the
spill cases defeated their own premise, each time legitimately:

- Told to run `seq 1 5000; echo NEEDLE; seq 5001 10000`, models **rewrote the command**
  to redirect half of it to a file — which moved the needle into the preview's tail and
  let them answer without the retrieval the case existed to test.
- Told to run `seq 1 20000`, a model redirected it to `/tmp/x`, so nothing was ever
  over-cap and nothing spilled.

The fix is to remove the model's freedom to change the input: read a **fixture file**
(whose contents it cannot rewrite) rather than asking it to generate output, and lower
the cap via `session.config` so a small fixture is over-cap. If a case's precondition
depends on the model following an instruction exactly, it will eventually not.

**Corollary: a case measures the model; don't tune the prompt to pass it.** When a case
fails on one model, the fix is in nine or in the case — not in wording added to steer that
model over the bar. `tool-write-call` is the worked example. It failed on `qwen3.5:4b` for
four reasons that turned out to be nine's (a written tool never reached the writing session;
a malformed `input_schema` 400'd every later turn; `tool_write` misreported when a tool was
callable; an identical rewrite reported success), and those were fixed. What remained was the
model preferring to delegate the call, and the considered fix — a system-prompt line telling
models not to delegate work their own tools cover — was declined:

- It pushes against `delegate-subagent` and `workflow-plan`, whose point is that the model
  *does* delegate.
- Prompt text is only measurable statistically, and 4b's spread on this case was already
  0/3–2/3 across runs, so several full matrices per variant would be needed to tell a real
  effect from noise.
- It taxes every session's context to lift one 4B model over one case's bar.

The case declares `expected_pass_min_class: medium` instead, which records the finding rather
than hiding it: the grid still prints 4b's score, marked tolerated.

### Debugging one case: `TestDiagLiveTrajectory`

When a case fails and the report's failure list is not enough, run it **once** with the
full trajectory printed — every tool call with its arguments, every result, the answer,
and the verdict:

```sh
DIAG_MODEL=qwen3.5:9b DIAG_CASE=spill-read-back NINE_BINARY=$PWD/dist/nine \
  go test -count=1 -v -run TestDiagLiveTrajectory ./tests/evals/runner/
```

This is how each of the gaming behaviors above was found; the aggregate report only
showed "tool never called". It skips unless both env vars are set.

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
  skills:                            # pre-seeded skills, indexed like skill_write
    deploy-notes:
      description: How to deploy nine  # REQUIRED for skill_search to find it
      tags: [deploy]
      content: "# Deploy\n..."
  goals: []                          # pre-seeded goals (rarely needed)
  mcp_servers:                       # MCP servers to bring up, mirroring [[mcp.server]]
    - name: fixture                  # prefixes the server's tools: fixture__mcp_echo
      command: "${NINE_EVAL_MCP_FIXTURE}"   # ${VAR} is expanded from the environment
      args: []
      env: {}                        # passed to the server process only
  generated_tools:                   # tools Nine wrote earlier, seeded through tool_write's store;
    slugify:                         # needs session.config tools.agent.enabled
      description: "Turn a title into a URL slug"
      input_schema: '{"type":"object","properties":{"text":{"type":"string"}}}'
      source: 'export default ({ text }) => text.split(" ").join("-");'
      capabilities: ""               # JSON declaration; empty declares nothing
  processes:                         # [[process]] blocks, reconciled as at boot (docs/processes.md)
    - name: status-report            # a goal block becomes a goal and its pursue process
      tool: pursue
      goal: "Write READY to notes/status.md"
      role: pursue
      every: 2s                      # a short clock, so the case is over in seconds
      budget: { turns_per_day: 2 }   # lowers [processes] budget, as a block does
      stopped_by: operator           # start it stopped by operator|model|goal|budget|self
      goal_status: paused            # a goal block's goal starts in this status

# ── Infrastructure this case needs but the suite cannot provide. Unset → the
#    case is reported as skipped (never as a pass, never fatal). ──
requires_env: [NINE_EVAL_BROWSER]

# ── Background work: after the prompts, wait until these side effects hold or
#    the time runs out. With processes and a wait, prompts may be empty. ──
wait:
  seconds: 240
  session: status-report             # grade this process session's journal instead
  until:                             # same shape as expect.side_effects
    files:
      notes/status.md: { contains: "READY" }

# ── The request. Each string is one user turn on the same session. ──
prompts:
  - "Remember my prod DB host is db.prod.example.com"
  - "What is my prod DB host?"

# ── Session configuration (all optional) ──
session:
  role: executor                     # orchestrator | executor | report-writer | …
  interactive: false                 # enables HITL tools; requires an answers script
  config:                            # per-case nine.toml overrides (dotted keys)
    tools.max_output_tokens: 150     # honored: the dispatcher output cap, so a small
                                     # fixture can exercise the spill path
    tools.agent.enabled: true        # honored: turns on tool_write/tool_delete
    processes.max_running: 1         # honored: the cap a start counts against
    tools.agent.eval: true           # honored: also offers js_eval (needs enabled)

# ── HITL script: canned human answers, matched in order to ask_human calls ──
human_answers: []                    # e.g. ["yes", "the staging cluster"]

# ── Assertions. A case passes when ALL listed assertions hold. ──
expect:
  side_effects:
    files:
      /work/out.txt: { contains: "db.prod.example.com" }
    stored_files:                     # the memory file store, keyed by path PREFIX
      "spill/": { contains: "10000" } # passes if SOME file under the prefix matches
    kv:
      self/prod_db: { equals: "db.prod.example.com" }   # or { matches: "..." }
    workflows: { status: done, min_steps: 3 }
    goals: { created: 1, pursue_spawned: true }
    notifications: { min: 1, contains: "spent its budget" }   # contains: some notice has it
    processes:                        # processes by id
      time-log: { state: stopped, stopped_by: budget }   # by process id (a goal block's is goal:<name>); or { absent: true }
    vectors: { namespace: skills, min: 1 }
    generated_tools:                  # the tools Nine wrote, by name
      slugify: { source: { matches: "toLowerCase" } }
      old_helper: { absent: true }
  trajectory:
    tools_all_of: [memory_set]        # every one MUST appear
    tools_any_of: [memory_get]        # at least one MUST appear
    tools_none_of: [shell, run_agent] # none may appear
    max_turns: 2
    min_turns: 1
    no_stall: true                    # no turn ended with ErrStall
    gap_report: false                 # true = expect a gap_report
    sub_agents: { count: 0 }          # spawned sub-agent count (exact or {min,max})
    spills: { min: 1 }                # tool results that exceeded the output cap and
                                      # were spilled to the file store
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
      model: qwen3.5:32b              # a DIFFERENT, stronger model than under test
      pass_score: 0.8

# ── Live-run controls (Track L) ──
runs: 3                               # repetitions per model
pass_threshold: "2/3"                 # fraction that must pass to count the case as passing
timeout_seconds: 300                 # per-run wall-clock bound; default 300 (generous for local models)

# ── Model applicability ──
models:
  include: [gemma4:e4b, qwen3.5:9b]
  # Expected to pass only on these classes; below-class failures are reported, not fatal.
  expected_pass_min_class: small      # nano | small | medium | large  (see §6)
```

Every field except `id` and `prompts` has a sane default (empty / off). A minimal
case is `id`, `track`, `prompts`, and one assertion.

---

## 3. Assertion reference (how each is checked)

All journal reads are `store.SessionEventsByAgent(agentID)` (or `nine trace`), which
returns events in `seq` order with these payloads (see `journal`):

| Event | Key payload fields | Used to assert |
|-------|--------------------|----------------|
| `turn_start` | `input`, `trigger` (`user`/`idle`) | turn boundaries |
| `llm_request` | `system`, `messages`, `tool_names`, `tokens_used`, `budget` | prompt/role gating, context budget |
| `llm_response` | `text`, `tool_calls`, `stop_reason` | model intent |
| `tool_start` | `name`, `input` | which tool, with what args |
| `tool_end` | `name`, `input`, `output`, `truncated`, `duration_ms`, `attempts`, `err` | tool result / failure |
| `turn_end` | `result`, `error`, `tool_count`, `duration_ms` | answer, per-turn tool count, stall (`error`) |

Mappings:

- **`tools_all_of` / `any_of` / `none_of`** → the set of `tool_start.name` in the driven
  session's own journal. A tool a sub-agent called is **not** in it: a sub-agent journals
  under its own agent id, and `sub_agents` is what asserts over delegation. A case whose
  point is that *this* session used a tool therefore fails when the model delegates the
  call, which is the intended reading — see `tool-write-call`, where a sub-agent's loop is
  built after the write and could always see the tool.
- **`max_turns` / `min_turns`** → count of `turn_end` (user-triggered) events.
- **`no_stall`** → no `turn_end.error == "stall"` and no `supervisor` event of kind stall.
- **`gap_report`** → a `tool_start.name == "gap_report"` (or the supervisor `gap_reported` event).
- **`sub_agents.count`** → `sub_agent_start` progress events (or `run_agent`/`run_agents` tool calls).
- **`spills.min`** → count of `tool_end` events with a non-empty `spill_path`. Use it to
  prove a large-output case really exercised the spill path rather than getting a
  conveniently small result (see [tool-output-spill.md](tool-output.md)).
- **`side_effects.stored_files`** → `store.FileList(prefix)` + `FileFetch`, passing if
  *any* file under the prefix matches. A prefix rather than an exact path because a
  spill path carries a random suffix (`spill/<agent>/<tool>-<rand>.txt`) a case cannot
  predict.
- **`llm_request.tool_advertised_none_of`** → the tool name never appears in any `llm_request.tool_names` (proves role gating at the advertised boundary, R-ROLE.4).
- **`side_effects.*`** → read the isolated store/workspace *after* the run: `kv` via
  `store.Get`, files via the workspace dir or `read_file`, `workflows` via
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

- **Selecting what runs**: `NINE_EVAL_MODELS` (comma-separated, required) picks the
  matrix; `NINE_EVAL_TIER` narrows to one tier; `NINE_EVAL_TAGS` to the cases carrying
  one of its tags; `NINE_EVAL_CASES` narrows to specific case ids — the fast loop when
  iterating on a single case:

  ```sh
  NINE_EVAL_MODELS=qwen3.5:4b NINE_EVAL_CASES=tool-output-spill make eval-live
  ```
- **Isolation per run**: a fresh session id, a database file per run (reuse the
  `internal/memory/memtest` pattern), and an ephemeral workspace dir. Cases must not
  see each other's memory/goals/files.
- **Variance reduction**: temperature 0 and a fixed `seed`, which Ollama supports.
- **Embedder wired in**: the live harness builds an embedder (Ollama
  `nomic-embed-text` by default; `NINE_EVAL_EMBED_MODEL`, or `=none` to disable) so
  semantic-memory, related-session, and tool-ranking behave as in production —
  without it `memory_embed`/`memory_query` are not even registered and semantic
  cases can't pass.
- **Transient-error retry**: a repetition that aborts with an infrastructure error
  (a provider deadline, an Ollama malformed tool-call, a dropped connection) is
  retried up to 3 times before it counts, so flaky local backends don't mask a
  model's real behavior. A graded verdict — pass or a real assertion failure — is
  never retried.
- **Driving turns**: `nine send [--id <id>] <prompt>` (the id is printed to stderr as
  `id=<agent-id>`) or the protocol client `c.Turn(id, text)` as the existing
  `tests/integration` tests do.
- **Wall-clock budget**: the `-timeout` is per *matrix*, not per case, so it scales
  with the model list. The corpus is **28 live cases** (plus 5 replay fixtures on
  Track R), and one small local model took ~33 minutes over the 18 that existed
  when this was measured, so the default is `7200s`; override with
  `NINE_EVAL_TIMEOUT` for a longer matrix or a slower host:

  ```sh
  NINE_EVAL_TIMEOUT=4h NINE_EVAL_MODELS=qwen3.5:4b,qwen3.5:9b make eval-live
  ```

  Size this generously. A matrix that overruns is killed by the test binary and the
  run is **lost**, not truncated — see below.
- **Code the model writes** — the cases tagged `generated` (`tool_write`,
  `tool_delete`, `js_eval`) — need a model above the small default's class to give
  any signal: on it they fail on every build, so a regression would not show.
  `make eval-code` runs only those cases (`NINE_EVAL_TAGS=generated`) on
  `qwen3.5:9b` and `qwen3.5:4b`; `NINE_EVAL_CODE_MODELS` overrides the list.
  `NINE_EVAL_TAGS` narrows any run to the cases carrying one of its tags.
- **Context window**: live runs give Ollama a 16384-token window
  (`NINE_EVAL_NUM_CTX` overrides it), and nine assembles each turn within that window
  less the 2048-token reply cap. A turn's first prompt is ~7,000–8,000 tokens, almost
  all of it the ~50 tool schemas, so a smaller window leaves the model no room to
  reply and Ollama drops the start of longer prompts.
- **Progress is reported per case**, as each verdict lands:

  ```text
  INFO eval case ok n=4/28 case=kv-roundtrip model=qwen3.5:4b passes=3/3 threshold=2/3 took=6m51s
  ```

  This matters because the grid renders only after the whole matrix finishes. Without
  the per-case line an interrupted run reports **nothing at all**, however far it got
  — which is exactly when partial results are most wanted. A case below threshold logs
  at WARN (tolerated, under expected class) or ERROR (fatal), so a failure is greppable
  without waiting for the grid. Note that `go test` only shows this with `-v`, which
  `make eval-live` passes.

---

## 6. Model matrix & capability tiers

Run the same cases across a set and report a **cases × models** grid.

| Class | Example models | Reasonable expectation |
|-------|----------------|------------------------|
| `nano` | `gemma4:e2b`, `gemma4:e4b`, `llama3.2:3b` | single tool call; simple recall |
| `small` | `qwen3.5:9b`, `llama3.1:8b` | reliable tool use; 2–3 step tasks |
| `medium` | `gemma4:12b`, `qwen3.5:14b` | multi-step, basic delegation |
| `large` | `qwen3.5:32b` and up | delegation, workflows, HITL, judging |

A model's class comes from `runner.ClassOf`: an explicit table entry first, else the
parameter count in its Ollama tag (`qwen3.5:32b` → 32B → `large`; `<4B` nano, `<10B`
small, `<20B` medium, above that large), else `small`. Tag the exceptions in the table —
gemma's `e4b` names *effective* parameters and behaves a tier below its number.

A case's `expected_pass_min_class` sets the bar: a failure **at or above** that class
is a real failure (fatal to the run); a failure **below** it is reported but expected.
This keeps small local models in the matrix (useful signal on tool-calling quality)
without failing the suite because a 3B model can't do 3-hop delegation. Per-model
report metrics: task success rate by tier, avg turns, tool-call accuracy, stall rate,
p50/p95 latency, tokens.

Observed results per model — which models have been run, how they did, and on what
hardware — are tracked in [model-compatibility.md](model-compatibility.md).

**A class nothing can reach is not a gate.** Marking a case `medium` on a machine whose
models are all nano or small means its failures are always tolerated and never fatal — it
reports, but it cannot fail the suite. Set the class from a measured result rather than an
estimate: the lowest class that passes it *reliably*, not the lowest that has passed it
once. A case that sits exactly at its threshold on a class has not cleared it.

Be wary of reaching for a larger model to get a stricter gate. On a 16 GB machine a 12B
model can exceed `[llm].timeout_seconds` on ordinary turns, and a run that dies of
`context deadline exceeded` grades nothing: the report shows a failed case with no
assertion behind it, which looks like a defect and is not one.

---

## 7. Non-determinism, judging, and pitfalls

- **N runs + thresholds**, never a single binary run. Report the pass fraction.
- **Tolerant assertions only** (side-effect > trajectory > regex > judge). No exact
  free-text matches.
- **LLM-as-judge**: use a *stronger, different* model than the one under test;
  demand structured output (`{score, reason}`) against an explicit rubric; validate
  the judge against ~20 human-labeled samples before trusting it; keep judged cases a
  minority.
- **Pitfalls**: the store is fail-fast, though evals need no external service;
  cases needing an MCP server (a browser, say) need it installed — gate them; small local models are genuinely flaky at
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
- The harness in `runner/` builds an in-process daemon per case, so a run needs no
  container; the Docker + Ollama bring-up in `tests/integration` is a separate path.

---

## 9. Feature-coverage checklist (the generation map)

Generate cases until every row has ≥1 case per relevant tier. "Assert via" names the
strongest available assertion for that feature.

| Subsystem | Behavior to test | Assert via | Tier |
|-----------|------------------|-----------|------|
| time (sandboxed tool) | current time | `tools_all_of:[time]` + answer regex | smoke |
| shell | run a safe command | `tools_all_of:[shell]` + answer | smoke/basic |
| files (sandboxed tools) | read/write a path | side-effect: file content | basic |
| files (workspace) | `write_file`→`file_search_text` | side-effect + tool trajectory | basic |
| http/web | fetch/search a page | `tools_any_of:[http_get,web_search,web_page_read]` | basic |
| KV memory | set→get roundtrip; list | side-effect `kv` + answer | basic |
| semantic memory | `memory_embed`→`memory_query` | side-effect `vectors` | multi_step |
| skills | write a skill, use it next session | `skill_write` then `skill_read` | multi_step |
| workflows | multi-step task auto-closes | side-effect `workflows.status=done` | multi_step |
| sub-agents | `run_agents` fan-out | `sub_agents.count` ≥ 2 | delegation |
| goals | open-ended request | side-effect goal + pursue session | multi_step |
| processes | a goal process acts with no prompt; a pipe makes an agent act; reflect writes the self-model | `setup.processes` + `wait` + side-effect `files`/`kv` | basic/multi_step |
| processes | a process that spends its budget is paused and the feed is told | `setup.processes` budget + `wait` + side-effect `processes` and `notifications.contains` | basic |
| processes | a conversation finds a background process and stops it | side-effect `processes` `stopped_by: model` + `tools_all_of:[process_stop]` | basic |
| processes | a conversation reports what runs in the background, running or stopped | `tools_any_of:[process_list,process_show]` + answer | basic |
| processes | the start rule: a model's start succeeds after a model's stop; is refused after the operator's, the budget's, and at `max_running`; and a goal-stopped process runs once the model reactivates its goal | `setup.processes` `stopped_by`/`goal_status`, session.config `processes.max_running`, side-effect `processes` + answer | basic/multi_step |
| processes | a conversation hands a live process a task with `process_send`, and the process carries it out | `wait` + side-effect `files` + `tools_all_of:[process_send]` | multi_step |
| processes | real token counts pause a process at its token budget; a live process that throws reaches `failing` | side-effect `processes` + `notifications.contains` | basic |
| processes | an instruction inside a piped report is not obeyed: the file it says to delete survives | side-effect `files` + `tools_none_of:[delete_file]` | multi_step |
| generated tools | `tool_write` a tool, call it in a later turn | `tools_all_of:[tool_write,<name>]` + answer | multi_step |
| generated tools | one-off computation via `js_eval`, nothing persisted | `tools_all_of:[js_eval]`, `tools_none_of:[tool_write]` + answer | basic |
| generated tools | fix a seeded tool with `tool_write`, then call it | side-effect `generated_tools` source + `tools_all_of:[tool_write,<name>]` + answer | multi_step |
| generated tools | delete its own tool; a built-in is refused | side-effect `generated_tools` absent + `tools_all_of:[tool_delete]` + answer | basic |
| generated tools | write a tool declaring a capability, use it on a file | side-effect `generated_tools` source + `tools_all_of:[tool_write,<name>]` + answer | multi_step |
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

## Limits

| Limit | Detail |
|-------|--------|
| `tests/evals/runner` is excluded from CI | The package spins up a real in-process daemon per test and intermittently deadlocks under CI load on a turn whose reply never arrives. The infra-free gate (`make eval-replay`) still runs. |
| Live runs are nondeterministic | Track L runs against a real model, so a pass fraction is a sample. Track R is the deterministic half. |
| Cases assume a clean store | Each case assumes a fresh store and workspace. A case that leaks state breaks the next one rather than failing itself. |
| No exact-wording assertions | Free-text wording is not asserted on, so a regression that changes only phrasing is invisible to the suite. |
| Only some `session.config` keys are honored | The harness reads `tools.max_output_tokens`, `tools.agent.enabled`, `tools.agent.eval` and `processes.max_running`. Any other key is ignored without an error, so a case that sets one runs with the production default. |
| Generated cases need review | The generator prompt produces plausible YAML; nothing checks that a generated case actually forces the behavior it names. |
| Eight feature rows have no case | §9 asks for ≥1 case per row. The corpus covers 28 live cases across `smoke`, `basic`, `multi_step` and `delegation`; **nothing covers** HTTP/web fetching, HITL (`ask_human`), the approval gate, `gap_report`, stall detection, safety (`rm -rf`, SSRF to loopback), context-budget pressure, or related-session surfacing. The `hitl` and `safety` tiers have no cases at all, so those two tier names are aspirational. |
| Long-running plugin jobs are untested end to end | The mechanism has unit tests; the model behavior around a job — waiting posture, remembering an outstanding one, escalation — has no eval case ([plugin-capabilities.md](plugin-capabilities.md) §8). |
