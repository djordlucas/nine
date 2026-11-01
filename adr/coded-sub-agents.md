# Design note — Coded sub-agents

**Status:** Proposed (2026-10-31, revised 2026-11-01 against five's source and run data and
Nine's code) ·
**Related:** `adr/process-sessions.md`, `adr/tool-facilities.md`, `adr/roles-design.md`,
`adr/generated-tool-authoring-loop.md`, `adr/deferred-tool-schemas.md`, `adr/node-compat.md`
(on `origin/adr/node-compat`, not merged) ·
**Amends:** R-ORCH.1–2 (a delegation may run a program), `adr/tool-facilities.md` §7 (the
containable shape it describes, built for one host call), R-PROC.8 and R-PROC.14 (task
results and task events), R-TVM (a third pool, `nine:llm`, `nine:board`), `llm.Request`
(structured replies), `[llm]` (a third provider), the glossary's "there is no Task entity"
(§11 lists each)

A **coded sub-agent** is a delegation whose child is a **program** instead of a ReAct loop.
A program is a sandboxed `js` tool that decides the steps of a finite task. It asks the model
only for language and judgment, in small calls with fresh contexts. Before anything is kept,
it checks what the model said against **evidence**: what the run's tools actually returned.

Programs run two ways. `run_agent` and `run_agents` gain a `program` argument, for a parent
that waits for the result. **The board** is for work posted without waiting: a durable pool of
tasks that the daemon drains as slots free, that programs can post to themselves, and whose
record every session can read. Either way the run is the same: its own session, a timeout,
a budget, the depth guard and priority 3.

**Fit for Nine.** The mechanism fits: it extends delegation, the process sandbox, roles, the
journal and restricted turns, and adds no new kind of session. Five's Node runtime, npm
installs, live settings and second model are not adopted (§2.2, §9). Five's results come from
one domain and from a single agent loop, not from Nine's sub-agents, so phase 0 must show
that Nine fails the same way before phase 1 starts (§12).

This note proposes a mechanism, not an application: the program shape, three host calls, the
board, a set of runtime checks, a check library, and the changes to the model layer they need.
Nothing in it is about a domain.

---

## 1. Problem

Delegated work in Nine is a sub-agent: a ReAct loop under a leaf role, given the task as its
first message. Five recorded these failures on a small model, and none depends on the task.
The last column is what Nine does about each today.

| Failure | What it looks like | In Nine today |
|---|---|---|
| Running out of steps with nothing kept | the work is read, and the loop ends before the answer is written | The loop has no step limit. A child ends when it answers, on a repeated call, or at `task_timeout_seconds` (1800), and a child that times out returns nothing |
| Answering from memory | claims with nothing the run read behind them | Nothing checks |
| A result that says nothing | `{"status": "ok"}` or `"done"`, taken as success by the parent | An empty reply is retried (R-LOOP.5). Any text passes |
| Malformed structure | JSON that does not parse, or does not match what was asked | `llm.Request` has no schema field. Tool arguments are the only structured output |
| Reaching past its job | a delegate that should only read edits a file another component depends on | Roles bound the tools. Nothing bounds what a write does, and a `nine:fs` write leaves no earlier version |
| Repeating a step | the same call made again and again with the same arguments | `repeatGuard` (`internal/agent/repeats.go`): a notice at 3 identical calls with identical results, the turn ends at 5 |

The loop leaves what to do next, when to stop, what to keep, and whether it is true to the
model. A program takes those decisions in code. The model gets single, well-posed requests,
and code checks each answer.

Nine has two homes for code that calls a model, and neither fits a finite delegated task:

| | Calls the model | Fits a delegated task? |
|---|---|---|
| A sandboxed tool | no | no: it cannot ask the model anything |
| A live process | `turn(text)`, in its own session | no: it runs until stopped and returns no result, its session history grows every turn, and a conversation reaches it only asynchronously |

Delegation is also synchronous only. A parent that wants ten things researched waits through
all ten. No delegate can hand a follow-up to another. And when the turn ends, nothing is left
of what was asked and what came back except that session's history. Workflows record a plan,
but they are passive by design: nothing runs a step unless a model calls a `workflow_*` tool.

---

## 2. What five found

Five (`../five`) is an experiment on a 16 GB Apple M4 with qwen3.5. Its workload was news
research; the mechanism is not about news.

### 2.1 Measurements

Each test ran from a clean start for 30 minutes and counted facts that passed the checks.

| Test | Server | Planner | Checked facts | What it showed |
|---|---|---|---:|---|
| 8 | Ollama | 4b | 30 | Code picked the topics from the news, with no duplicates. An Ollama stall failed 3 runs |
| 9 | Ollama | 9b | 41 | The two models swapped in and out of memory |
| 10 | Ollama | 9b | 59 | A larger planner planned better, and did not fit beside the workers |
| 11 | Ollama | 4b | 40 | First run requiring every number of a fact in its quote |
| 12 | MLX | 9b | 9 | Out of GPU memory: MLX loads both models without checking free memory |
| 13 | MLX | 4b | 32 | MLX samples greedily unless told, and the model fetched one page twelve times |
| 14 | MLX | 4b | 10 | The presence penalty broke JSON; the planner then rewrote the working program |
| 15 | MLX | 4b | 39 | No parse failures, no programs written. 9 of 13 research runs kept nothing |
| 16 | MLX | 4b | 78 | Step warning and evidence fallback. All 16 finished runs kept facts |

| Four research calls at once, 4B 4-bit, thinking off | Ollama | MLX |
|---|---:|---:|
| Seconds to the last reply | 66 | 22 |
| One call alone, seconds | 20 | 18 |
| Tokens per second | 26 | 69 |

How far these numbers go:

- Tests 8–10 ran under a weaker check than tests 11–16: a fact could say more than its quote.
  Test 10's 59 is not comparable with what follows it.
- Each test is one run, counted by hand, in one domain.
- Test 16 changed two things. The evidence fallback ran in 3 of its 16 runs; the rest of the
  gain cannot be split between the step warning and run-to-run variation. Seven more runs were
  cut off when the test stopped, and are recorded as failed.
- Test 15 on MLX kept as much as test 11 on Ollama (39 and 40). Three times the throughput did
  not raise the count while most runs kept nothing. The two runs differ in more than the
  server, so this is a reading, not a measurement.
- No test measured a prompt change alone. What the record does show is that prompts did not
  hold: the lead was told to pick from the results and still posted stories it remembered,
  until code refused them. Test 16 wrote no programs, and both seed programs stayed at
  revision 1.

### 2.2 Each finding, and what Nine does with it

| Finding in five | Evidence | Nine today | Decision |
|---|---|---|---|
| Workers as code; the model for single steps | the mechanism behind all of §2.1 | no home for it (§1) | **Adopt**: programs (§4) |
| `llm.ask`, `llm.agent`, `web.read` | same | `turn()` in live processes only | **Adopt** as `nine:llm` (§4) |
| A board, and a code driver | several runs on one model; the planner chose from the record | none | **Adopt** (§5) |
| Result must be text | programs returning `{"factsFound": 0}` | any text passes | **Adopt** (§6) |
| Constrained decoding with a schema | no failed parses on Ollama | no `format` in `llm.Request` | **Adopt**, with a model-layer change (§8) |
| Warning two calls before the last | part of test 16 | the loop has no step limit | **Adapt**: a step limit and warning in `agent()` only (§4) |
| Ask again from the evidence | ran 3 times in test 16 | none | **Adopt** as a pattern, using `agent()`'s `outcome` (§4) |
| Claims checked against what was read, numbers included | a claim of 23 million births backed by a fertility-rate quote in test 10 | none | **Adopt** as `nine:check`, Unicode-aware (§6) |
| Alike tasks refused | 5 refused in test 16 | none | **Adapt**: five's matcher names news words and `story:`, so Nine needs a generic one (§6) |
| Tasks must point at something real | stories posted from memory, years old | none | **Adopt** as `accepts(task)` (§6) |
| Trial on a copy of the workspace | a program without one wrote raw HTML over the day's digest | the W2 dry run is unbuilt | **Adapt**: the copy must also reach the tools `agent()` calls (§7) |
| A working program cannot be revised | small models rewrite working code | versions, probation, rollback (R-PROC.16) | **Adopt** beside those (§7) |
| At most `maxWorkers` programs | same | `[processes] max_running` covers processes only | **Adopt** (§7) |
| A failed run's writes put back | the same digest | `.nine/trash` covers `write_file`, `edit_file`, `delete_file`, not `nine:fs` | **Adopt** (§6) |
| Private state in dot files | the planner emptied `news/seen.json`, and 64 lines were added twice | `nine:state`, `nine:sql` | **Exists**: a rule for programs (§6) |
| Fresh contexts, append-only, 12 000-token budget | "contexts stay small" | the builder ranks tools and trims per call; goal-session ticks start from an empty history | **Adapt** in `agent()` (§4) |
| Tool results cut at 2 000 characters, the rest in a file | same | spill at 2 048 tokens | **Exists**, with a lower cap in `agent()` (§4) |
| Thinking off | all tests | on by default; `Request.Think` overrides | **Adopt** in `ask` and `agent` (§4) |
| The date first in every system prompt | small models guess the date | `ambientCore` has it; a bare `ask` would not | **Adopt** in `ask` (§4) |
| A tool call written as text is recovered | carried over from seven | none; eval retries absorb it | **Adopt** in the model layer (§8) |
| A failed connection is retried once | Ollama stalled for five minutes and three runs failed | the queue retries only rate limits | **Adopt** (§8) |
| First come, first served within a priority | five's pool | `itemHeap.Less` compares priority alone, so equal priorities have no order | **Adopt** (§8) |
| Slots matched to the server, 16 k tokens each | four slots on 16 GB | `max_concurrent = 1`, `num_ctx = 32768` in `nine.toml` | **Adapt**: a documented profile (§8) |
| MLX, with its sampling and JSON rules | the bench; tests 13–15 | Ollama and Mistral | **Adopt** as an optional provider (§8) |
| A larger model for planning | tests 9, 10, 12 | one `[llm]` model | **Not adopted**: it does not fit in 16 GB. Revisit on a larger host |
| The main loop as code; timers post work | "nothing depends on a small model remembering to" | processes run on clocks and events | **Exists**: a live process calls `post()`, and `task_end` is a trigger (§5) |
| The lead: fresh prompt, notes | planning from results | goal sessions start each tick empty, and a piped report starts a turn | **Exists**: `pursue` is the lead. A note-keeping reminder is not adopted |
| The chat beside the workers, first in line | | conversations are priority 2 | **Exists** |
| Inbox | | `notify_user`, the human feed | **Exists** |
| Live settings | | `nine.toml`, which Nine never rewrites at runtime | **Not adopted**: it contradicts that rule |
| Node runner, npm install, `run_js` with all of Node | five's stated limit: no permission model | QuickJS in wazero, with capabilities | **Not adopted** (§9) |
| `five.web.text/feed/search` host calls | | `fetch`, `nine:html`, `web_search` through `agent()` | **Not adopted**: a program uses `fetch` and its own parsing (§9) |
| Libraries (`lib:name`) | | `[tools.agent.deps]` | **Not adopted** |

---

## 3. Options

**A. As tools.** Give a sandboxed tool the `llm` capability and let a model call it inside a
turn.

| For | Against |
|---|---|
| The smallest surface | A call running for minutes holds the caller's turn and a sandbox slot |
| | `agent()` with tools is the tool→tool dispatch `tool-facilities.md` §7 rejects: the tool's reach becomes the closure of what its agent can call |
| | Ranking and deferred schemas decide whether the model sees it at all |

**B. As sub-agents.** `run_agent({ program, task })`: the delegation is unchanged, and its
child is code.

| For | Against |
|---|---|
| A delegation is already finite and returns a result. It has the depth guard, a child session in the journal, approval gates that follow the owner, and priority 3 | A program's model calls need a role, while a sub-agent has one role for its whole loop |
| `run_agents` already runs several at once, bounded by the LLM queue | `run_agent` itself has no timeout of its own: it runs under the parent's turn |
| Workflows already record delegated steps, and roles already gate who may delegate | |

**C. As processes.** Send work to a live process with `process_send`, and read its results
from what it writes.

| For | Against |
|---|---|
| Built, with budgets, health and versions | Asynchronous: the sender cannot wait for a result |
| | `turn()` appends to the session's history, so contexts grow |

**Decision: B, built on the process sandbox, with a board (§5) for the work posted without
waiting.** Processes stay the home for work driven by a clock or by events. A is rejected for
now (§13).

---

## 4. The mechanism

**A program** is a `js` sandboxed tool with a `program` declaration. Any tier may write one:
shipped, developer or generated. Like a live tool, it is never in a tool list. A delegation
runs it, calling its default export with `{ task }`. Its return value is the delegation's
result.

```toml
[tool.<name>.program]
role    = "monitor"     # bounds its agent() calls: their tools are a subset of this role's
timeout = "10m"         # the run's deadline; the default is 10m, five's taskTimeoutSeconds
writes  = true          # its effect is files; the trial (§7) checks it has one, a failed run is undone (§6)
memory  = "64MB"        # the default; ordinary tools get 16 MiB
```

A `role` that does not exist fails the load. `ResolveLeaf`'s fallback to `executor` is for
models naming a role, and does not apply here.

**Calling.** `run_agent({ program: "<name>", task: "…" })`. `role` and `program` are
mutually exclusive, and `run_agents` takes either form in each entry. The roles that may
delegate today (`executor`, `orchestrator`, `pursue`) may delegate to a program. This adds an
argument, not a tool: about 40 tokens of schema.

**Bounds.** A program runs in the live path, with the bounds set for a finite task:

| Bound | Ordinary tool | Live process | Program |
|---|---|---|---|
| Deadline | 5 s | none | `timeout` |
| Linear memory | 16 MiB | 16 MiB | `memory`, 64 MiB by default: five's seed works on whole pages in a 1 GiB heap |
| Work budget | one per call | refilled by `next()` | refilled by each `nine:llm` call's return |
| Pool | `[tools] max_concurrent` | `[processes] max_running` | `[board] pool` (default 8), so programs starve neither |
| Model calls | none | `turn()`, under a daily budget | `[board] run_budget` (default 40 model calls), then `E_BUDGET` |

The run budget closes a gap five left open: code that loops on `ask` has no other bound below
its timeout. Tokens a task spends are charged to its poster when the poster is a process
(R-PROC.11). A conversation has no budget, so its bound is `run_budget` and `max_queued`.

**The host calls (`nine:llm`).** They block, like `next()` and `turn()`, because the guest
is synchronous. They reuse the `process` host import with new ops, so neither the QuickJS blob
nor the harness changes. Outside a program run they throw `E_NOT_PROGRAM`, as `nine:process`
throws `E_NOT_LIVE`.

| Call | Context | Reach |
|---|---|---|
| `ask(prompt, { system?, schema?, max_tokens? })` | fresh: the date, a system line and the prompt, nothing else. It goes straight to the queue at priority 3 with thinking off, and skips the context builder. With `schema`, it uses the provider's constrained decoding where it has one (§8). The reply is parsed and validated against the schema (§6) | none: no tools |
| `agent({ prompt, system?, tools, max_steps? })` | fresh: a ReAct loop in the child session, with only `tools`, thinking off. Returns `{ text, outcome }`, where `outcome` is `done`, `max_steps`, `budget` or `repeated` | `tools`, a subset of the program's `role`. This is the shape `tool-facilities.md` §7 called containable: a named allowlist under its own role, depth 1, and the closure printed in the roster |
| `evidence({ source? })` | — | what this run's tools returned: from its `agent()` calls and its own `fs` and `fetch`. Each entry is `{ source, kind, text }`, where the source is a URL, a workspace path or a command, and the kind is page, search, file, http or command |

Every call is journaled in the child session, as a sub-agent's model and tool calls are
today. The parent sees only the program's result, spilled when long.

**`agent()` is a loop with a smaller context than a sub-agent's.**

| Setting | Value | Why it differs from a sub-agent |
|---|---|---|
| `max_steps` | 12 by default, five's value | Applies to `agent()` only. Sub-agents and turns keep having no step limit, so replay snapshots do not change |
| Context budget | 12 000 tokens. At 80 % it says to finish, past 100 % it ends with `budget` | The builder trims the oldest history instead. Trimming a context the model is still reading is what five avoids |
| Tool list | exactly `tools`, unchanging for the whole call | Ranking and hybrid picks change the list between calls, which changes the prompt's prefix. A prefix that only grows is one the server has already read |
| System prompt | the date and `system` | No self-model, enrichment or memory surfacing |
| Tool output | 2 000 characters before the spill, against 2 048 tokens | Five's `resultChars`: a 12 000-token budget does not hold many results at 8 000 characters each |

`evidence()` is taken in the dispatcher, before the spill cap, so a spilled result is still
whole in the evidence. Each entry's text is cut at 64 KiB and marked as cut. `source` filters
to one source, which keeps a program from copying every page into guest memory to check one.

**Why fresh contexts.** A program's model calls see each other only through what its code
passes along. No prompt grows with the length of the run, which is what keeps a small model
inside the size of context it handles well. A sub-agent's history grows with every step, and
a process's with every turn.

**The evidence fallback.** When `agent()` ends without a usable answer, a program asks again
from the evidence: a few entries, each cut short, through `ask`. Five's `research` keyed this
on "no facts came back", which also fires on an honest "the pages do not say". `outcome` lets
a program key it on `max_steps`. It stays in the program, and `program-patterns` shows it.

**Three shapes, one mechanism.** Illustrations, not features:

| Program | `agent()` with | `ask()` for | Checked against `evidence()` |
|---|---|---|---|
| Answer a question from the web | `web_search`, `web_page_read` | claims, each with a source and a quote | the pages it read |
| Find where a behavior is implemented | `read_file`, `file_search_text` | locations, each with a path and the lines quoted | the files it read |
| Explain a failure from logs | `read_file`, `file_search_text` over the log files | the cause, with the lines it rests on | the files it read |

No read-only role has `shell`: `monitor` and `code-reviewer` both exclude it. A program that
needs command output runs under `sysadmin` or `software-dev`, and its reach is theirs.

---

## 5. The board

`run_agent({ program })` is a parent waiting for a result. The board is the other half: work
posted without waiting, run when a slot frees, and recorded where anyone can read it.

**A task** is one program run on the board:

| Field | |
|---|---|
| `program`, `task` | what runs, and its input |
| `posted_by` | a session, or the task whose program posted it |
| `depth` | the poster's lineage depth (`adr/process-sessions.md` §7) for a session; one more than its poster for a program |
| `status` | `queued`, `running`, `done`, `failed`, `cancelled` |
| `result` | the program's text, or the failure's reason |
| `delivered` | whether the poster received it |
| `session` | the run's child session, for its journal |

`depth` must not restart at 0 for a process session. A process whose result is a trigger could
otherwise post, receive and post again forever, past `[processes] max_depth` (R-PROC.14).

**Posting.**

| From | With |
|---|---|
| A conversation or a process session | `task_post({ program, task })`, granted per role like `run_agent` |
| A program, or a live process's program | `post(program, task)` from `nine:board` |
| The operator | `nine task post`, `POST /api/v1/tasks` |

A post is admitted or refused on the spot, with a reason. The poster is a model or a program
that can act on the refusal. The checks that apply are the runtime checks of §6: the program
exists, `accepts(task)`, alike tasks, `max_queued`, and a process poster's budget. One more
applies to posting: a task at `[processes] max_depth` (default 2) cannot post. A program may
hand work on, and that work may hand work on once, but chains end, as processes' lineage does.

**Running.** A driver in the daemon takes queued tasks oldest first, up to
`[board] max_running` at once (default: the LLM queue's `max_concurrent`). Each runs exactly
as `run_agent({ program })` runs it. `run_agent({ program })` also records its run on the
board, as a task that starts `running` at once. Waited-for and posted runs therefore share one
record and one alike check, and a waited-for run never queues behind posted ones.

**Results.** A finished task's result is delivered to its poster:

| Poster | Receives the result as |
|---|---|
| A parent that waited | the return value only |
| A conversation | a restricted turn, through the wake path pipes use (`Daemon.WakeAgent`) |
| A process session | a message, which is a trigger, under R-PROC.8: taken only while it waits for work, and otherwise the human feed |
| A program | nothing: its run has ended. The result is on the board for whoever reads it |
| Gone, or a task at `max_depth` | the human feed, journaled as `process_skipped` for a process |

`AgentWorker.Wake` is lossy: it returns false while a turn runs. A result that is not taken
stays `delivered = false`, and the driver tries again at the poster's next `turn_end`.

Results do not go through queued messages (`docs/queued-messages.md`). `queued_messages_get`
would put the result in an unrestricted turn, and a consumed message joins the history as a
user message. The queue also drains only after a turn, so it would not reach an idle
conversation.

**Untrusted results.** A result is data a program produced from what it read, so it is an
injection path, as piped reports are. It is delivered framed with its task, as
`[Result of task #12 (<program>): …]`, and the turn it starts is restricted as a piped turn is
(`adr/process-sessions.md`, Limits).

Pulling a result must not be a way around that. `task_get` and `task_list` count as reading a
tainted file: an unrestricted turn that reads a result is restricted from that read on. Both
tools join `PipedTurnTools`, so a restricted turn can still read the board.

A restricted turn cannot post, which would stop what five showed: a planner choosing its next
tasks from the results. So `task_post` joins `PipedTurnTools` with a guard: from a restricted
turn it may post only to a program declared with `restricted_post = true`. A post's reach is
the program's declaration, and its input cannot widen that. It can still steer the program
inside that reach, which is why this is an opt-in that written programs cannot set (§13).

**Approval gates.** A gate follows its owning session (`docs/hitl.md`), and `task_post` must
not be a way around `require_approval`. A task posted from a conversation keeps that
conversation as its gate owner, so a gated tool in `agent()` raises a question in the Inbox and
holds its slot until it is answered or times out. A task posted by a session with no
interactive owner has no gate, as a goal session's children have none today.

**Events.** The journal records `task_posted`, `task_start` and `task_end`. `task_end` joins
the types R-PROC.14 allows, so a live process can react to finished work: this is how a
process keeps work moving without a model remembering to. Metadata only, unless the process
sets `event_content`.

**Reading and control.**

| Tool | Does |
|---|---|
| `task_list({ status?, program?, posted_by? })` | the board, newest first, with each result's first line |
| `task_get(id)` | one task: its input, result, timings and session |
| `task_cancel(id)` | stops a queued or running task, allowed to its poster and to the operator |

The operator has `nine task list|show|cancel`, `/api/v1/tasks`, and a web UI page.

**Restarts.** At boot, a posted task left `running` is queued again once. If it is interrupted
again, it fails as "interrupted twice". A waited-for task fails as "interrupted" at once: the
turn that waited for it is gone, and a late result would have nowhere to go. A program's writes
are its own concern: a program that must not run twice says so in `accepts`.

**Retention.** Finished tasks stay for `[board] retention` (default 30 days), then go with
their sessions in the session-retention sweep.

**Configuration.**

```toml
[board]
max_running  = 1                        # default: [llm] max_concurrent
pool         = 8                        # sandbox instances for programs, posted and waited-for
max_queued   = 20                       # per poster
alike_window = "30m"
retention    = "30d"
run_budget   = { model_calls = 40 }
```

**Why not workflows.** A workflow is an ordered plan a model keeps, with no driver.
`spec/overview.md` §3.1 makes the difference between workflows and goals a matter of
autonomy, and the board is autonomous. Giving workflow steps a driver would blur that line.
A board is a pool with no order, and a workflow step may still name the task that carries it
out.

---

## 6. Checks

Checks come in two kinds. **Runtime checks** are enforced by the host, whatever a program's
code does; some apply only when the program declares what they test. **Content checks** are a
library a program calls: the runtime cannot know what a claim is in a given task, but it can
make checking one a single line of code.

### Runtime checks

| Check | What it does | Failure it closes |
|---|---|---|
| **Result is text** | refuses `null`, `""`, an object, an array, `{ error }`, or fewer than 4 words of letters in any script. The run fails, and the reason reaches the parent | a result that says nothing |
| **Structured replies** | `ask` with `schema` validates the parsed reply. On a mismatch it asks once more, giving the validation error. Then it throws | malformed structure |
| **Step warning** | two calls before `max_steps`, when `max_steps` is at least 4, `agent()` adds: "two model calls left: reply now with what you have, without a tool call" | running out of steps with nothing kept |
| **Repeated calls** | `agent()` uses the loop's `repeatGuard`, and ends with `outcome: "repeated"` | repeating a step. Five has no such check; its looping was a sampling fault (§8) |
| **Alike delegations** | refuses a `program` and `task` alike to one queued, running, or done within `[board] alike_window`. The refusal names the earlier run and its result. A failed task does not count, so a failure can be retried | the same work delegated again and again |
| **Task admission** | a program may export `accepts(task)`, which runs before anything else with no model access. A string it returns refuses the delegation with that reason | delegations built from memory: a program can require that the task point at something that exists, such as a file, a URL the parent read, or a record |
| **Failed runs undone** | for a program with `writes`, the host keeps each file before the run first changes it, and puts them back when the run fails or times out. The task's reason lists them | a failing delegate leaving broken files for the others |
| **Trial** (written programs) | §7 | written programs that do nothing and report success |

Admission checks (alike, task admission, `max_queued`, budget, and depth on the board) run
when a task is posted or delegated, before it queues, so the poster learns of the refusal at
once.

**Alike.** Two tasks are alike when they name the same program and either a program-exported
`key(task)` returns the same string for both, or, without one, their texts are equal after
normalizing, or they share at least 2 content words and 60 % of the shorter one's. Five used
those thresholds with a list of news words and its `story:` marker; `key` is the generic form
of that marker. The check is daemon-wide, because checking per session misses `pursue`
delegating the same task at every tick.

**Task admission** is the generic form of five's grounding rule. Five's research program
refused any task that did not cite a link from the news it had read. Another program might
require an existing path or an open record. The runtime only runs the check; the program
states it.

**Undo.** Copies go to `.nine/history/<task>/`, the way `.nine/trash` keeps replaced files, and
go with the task at retention. The host takes them through a new op that `nine:fs` calls before
each write, and through the loop's call guard for `agent()`'s write tools. Five does the same
by patching `node:fs` in the child.

**State.** A program keeps its state in `nine:state` or the database, never in workspace files
another delegate can edit. This is a rule the `program-patterns` skill teaches, not a check:
nothing can tell a state file from a result. Five's planner emptied a state file it took for
its own, and five answered by hiding dot files from agents' file tools. Nine does not need that
while state has its own stores.

### Content checks (`nine:check`)

Pure functions over text, shipped and tested like the stdlib. They work on any evidence:
pages, files, command output.

| Function | Returns |
|---|---|
| `quoted(text, quote)` | whether the quote is in the text, normalized for case, punctuation and whitespace: whole, or `run` words in a row (default 8). A quote under 20 characters never matches |
| `says(text, claim)` | the sentence of the text (or two in a row) that has a share of the claim's content words (default 60 %) and **every number** in it. Otherwise `undefined`. A claim under 3 content words never matches |
| `cited(claims, evidence)` | splits `[{ claim, source, quote }]` into `{ kept, dropped }`. A claim is kept when its quote is `quoted` in its source and carries the claim's numbers, or when any evidence `says` it, which then becomes its source. Each dropped claim has a reason. A named source the run never read may be fetched once, through the program's own grants |
| `novel(text, existing)` | whether the text is new: not already present, and not a substring of a longer entry or containing one |

Five's versions match `[a-z]` and an English word list. Nine's use `\p{L}`, and the word list is
an argument, so the checks are not limited to English.

The idea five's results rest on is "the model finds and words; code checks and writes". A
program asks for claims with their sources, keeps what `cited` keeps, and writes only that.

---

## 7. Writing programs

`tool_write` with a `program` block, gated by `[tools.agent] allow_programs` (off by
default). The write passes today's checks: parse, the declaration against the ceiling, and
approval. Then it passes the trial. A written program's `role` must be in
`[tools.agent] program_roles` (default `["monitor"]`), as `process_roles` bounds written
processes. `monitor` writes no files but does have `memory_set`. Versions and rollback are the
generated tier's.

**The trial.** The block must carry a `sample_task`. Before saving, the program runs on it, and
must return text and, when `writes`, change a file. The W2 dry run of
`generated-tool-authoring-loop.md` is unbuilt, so this builds the trial itself.

| Rule | Why |
|---|---|
| It runs on a copy of the workspace, leaving out `.nine/` and keeping timestamps | five's first copies made every file look written |
| `/work` is mounted on the copy | a program's own `nine:fs` writes land there |
| `agent()` in a trial offers only the file and web tools of the shipped set | `shell`, plugins and memory tools resolve against the real workspace and store. The file tools are pointed at the copy |
| `post()` is a no-op, and alike is off | a trial is not work |
| It has a 3-minute deadline, and a run budget | five's `TRIAL_MS` |
| A workspace over `[tools.agent] trial_max_mb` (default 256) is refused | a copy per write is otherwise unbounded |

**Limits on rewriting.** A revision is refused while none of the program's latest five tasks
failed, with its counts in the refusal. A new program is refused at
`[tools.agent] max_programs` (default 4) until one is deleted. Both come from five, where small
models rewrote working code and wrote programs for work one already did.

Writing is off by default because five's evidence points that way. Programs written by a 4b
model added nothing until the trial refused them, and the best run wrote none. The value is
the mechanism, used by programs the operator or a developer writes. Writing programs is an
option, not the feature.

---

## 8. Model serving

The mechanism needs five changes to the model layer. Each is useful alone, so they ship first.

| Change | Today | With this note |
|---|---|---|
| `Request.Format`, a JSON Schema | absent | Ollama's `format`; Mistral's `response_format`; MLX below |
| Reply cap on Ollama | the adapter ignores `Request.MaxTokens`, so `[llm] max_tokens` has no effect there | sent as `num_predict`, which `ask`'s `max_tokens` needs. This fixes a bug in its own right |
| Tool calls written as text | not recovered | recovered in the adapter when the text is only calls to tools offered, ported from five's `textcalls.ts` |
| Connection retry | the queue retries only `RateLimitError` | one retry on a failed connection |
| Order within a priority | unordered: `itemHeap.Less` compares priority alone | a sequence number breaks ties, so "oldest first" holds down to the model call |

**Concurrency is a setting across three places.** `[board] max_running` follows
`max_concurrent`, and `nine.toml` ships `max_concurrent = 1`, so the board runs one task at a
time by default. That is the right default: with one server slot, several agents would take
turns in it, and each call would read its whole context again. Running four needs all of:

```toml
[llm]
max_concurrent = 4
num_ctx        = 16384   # per slot: 32768 × 4 was never measured on 16 GB
```

```bash
OLLAMA_NUM_PARALLEL=4 OLLAMA_KEEP_ALIVE=30m ollama serve   # the macOS app does not pass the variable
```

`num_ctx` also sets the context budget, so this halves a conversation's context unless
`context_budget` is set apart. On Ollama the gain is small: four calls took 66 s against 80 s
for four in a row.

**MLX.** A third provider, `provider = "mlx"`, for `mlx_lm.server` on Apple silicon. It decodes
concurrent calls together. Its OpenAI-style API looks reachable through `provider = "mistral"`
with an endpoint override, and that would reproduce test 13 and test 14: Mistral's adapter sends
no sampling parameters, and it requires an API key.

| Requirement | Reason |
|---|---|
| Send `[llm.sampling]` on every call: for qwen3.5, `temperature = 1`, `top_p = 0.95`, `top_k = 20`, `presence_penalty = 1.5` | the server is greedy unless told, and the 4B fetched one page twelve times (test 13). Ollama takes these from the Modelfile, so they are per model |
| A reply with a schema uses `temperature = 0`, no penalty, the schema in the system prompt, and JSON cut out of the reply | MLX cannot constrain a reply, and the penalty breaks JSON within a few lines (test 14) |
| Always send `max_tokens` | the server's default is 512 |
| Stream, with `include_usage` | usage feeds the budgets; a long call otherwise waits on headers |
| A stream with no choices is an error | a generation that died mid-reply otherwise reads as an empty answer |
| `chat_template_kwargs.enable_thinking` carries `Think` | thinking is a template setting here |
| Implement `ThinkingAware` from `[llm] thinking` | MLX has no capability endpoint. Without it, plan mode's fallback never runs, because unknown is not unsupported |
| No `num_ctx`; the budget comes from `context_budget` | the server grows its cache per request, and checks no free memory (test 12) |

One server runs one model. The keyword embedder is the default and needs no server; a deployment
that embeds through Ollama runs both, and memory is the limit. Nine's matrix
(`docs/model-compatibility.md`) has no MLX column, so a host profile row and a Track-L run belong
to the same phase.

---

## 9. Code runtime

Five runs each program as a Node child process with no permission model. Nine does not adopt
that. A program is a `js` tool in QuickJS under wazero.

| | Five | Nine |
|---|---|---|
| Runtime | Node 24 child process, killed with what it spawned | QuickJS in wazero, no process to spawn |
| Reach | all of Node: files, processes, network, any package | capabilities, granted by the operator |
| Memory | 1 GiB V8 heap | 64 MiB for a program (§4) |
| Files | `node:fs` in the workspace | `nine:fs`, on granted mounts |
| Web | `fetch`, `five.web.*` | `fetch` under `net.http`, `nine:html`; `web_search` and `web_page_read` only through `agent()` |
| Packages | any bare import, installed by `npm` at run time | bundled at write time by `[tools.agent.deps]`, off by default |
| Written code's blast radius | whatever Node can do, softened by the trial and undo | its declaration |

Five's own limit states the reason: "`run_js` and task workers run with all of Node". In Nine,
reach outside a grant is structurally absent (R-TVM.5), and the runtime image has no package
manager (`docs/sandboxed-tools.md`). Option E of `adr/node-compat.md` rejected a Node tool kind
because it would give up that confinement.

**What programs lose.** Nothing the seeds need. `research` uses files, regular expressions and
host calls, all of which QuickJS has. `feeds` needs an XML parser, which a program bundles or
writes, and it would use `fetch` directly, which `evidence()` records.

**`adr/node-compat.md` is independent of this note.** It would let bundled npm packages that
touch `Buffer`, `path` or `events` load, which widens what a program can import. No phase here
depends on it, and it does not change a program's reach.

---

## 10. What ships

| | Content |
|---|---|
| Model-layer changes, and the `mlx` provider | §8 |
| `program` declarations and `run_agent({ program })` | §4 |
| `nine:llm` (`ask`, `agent`, `evidence`) | §4 |
| The board, `nine:board`, and its tools and events | §5 |
| The runtime checks, with undo | §6 |
| `nine:check` | §6 |
| The `program-patterns` skill | the three shapes of §4 as complete programs, each run by a test, the evidence fallback, the common mistakes, as `process-patterns` is for processes |

No domain program ships. The skill's examples are examples.

---

## 11. What this amends

| | Today | With coded sub-agents |
|---|---|---|
| R-ORCH.1–2 | A delegation runs a ReAct loop under a leaf role | …or a program, whose `agent()` calls run under its role |
| R-ORCH.3 | `run_agents` has one group timeout, `task_timeout_seconds` by default and at least 300 s | Unchanged. A program's `timeout` applies inside it, and to `run_agent`, which has none of its own |
| `tool-facilities.md` §7 | Tool→tool dispatch is rejected; a containable shape is recorded | That shape is built for `agent()` only |
| R-PROC | A live process asks the model only with `turn()` | Live processes may also `ask()`, so a process that needs no history stops growing its session, and `post()` |
| R-PROC.8 | Pipes deliver reports | Task results are delivered the same way, framed as results |
| R-PROC.14 | Event types | Adds `task_end` |
| `PipedTurnTools` | No board | Adds `task_list`, `task_get`, and `task_post` under its guard |
| R-TVM | Two pools; `nine:*` as listed | A third pool; `nine:llm`, `nine:board`, `nine:check` |
| `llm.Request` | No structured replies | `Format` |
| `[llm]` | `ollama`, `mistral` | `mlx`, `[llm.sampling]` |
| Glossary, "Work unit" | There is no Task entity: finite work is a sub-agent or a workflow step | A **task** is a program run on the board. It adds no new kind of work, only a record of one: a task runs as a coded sub-agent |

---

## 12. Phases

| # | Content | Acceptance |
|---|---|---|
| 0 | Live cases in three domains: a web question, a code location and a log diagnosis. Each is delegated to `executor` on 4b and 9b and graded on side effects: claims whose quote is in their source, claims without one, results that say nothing, runs that kept nothing, and timeouts | Recorded on `main`: the baseline. If Nine's sub-agents do not show §1's failures, the rest of this note stops here for review |
| 1 | §8's five model-layer changes | `max_tokens` is honoured on Ollama; a schema reply parses on Ollama and Mistral; replay snapshots of existing turns unchanged |
| 2 | `program` declarations (developer and shipped tiers), `run_agent({ program })`, `nine:llm`, the program pool | Replay snapshots of existing delegation unchanged; a program run journals as a child session; `E_NOT_PROGRAM` outside a run |
| 3 | The runtime checks | Each refusal has a case that fails for its reason; a failed run's writes are put back, from `nine:fs` and from `agent()`'s tools |
| 4 | The board: tasks, `task_post`, `post()`, the driver, delivery, `task_list`, `task_get`, `task_cancel`, events, the operator surface | Ten tasks posted at once all finish with at most `max_running` running; a program's follow-up runs; a post at `max_depth` is refused; a process cannot exceed `max_depth` by posting; a result reaches an idle and a busy conversation, restricted; reading a result restricts the turn; a gated tool in a posted task raises a question |
| 5 | `nine:check`, the `program-patterns` skill, and each phase-0 case rewritten as a program | On 4b, each domain keeps more checked claims than its baseline and writes **no** unchecked ones, in 3 of 3 runs |
| 6 | Written programs: `allow_programs`, `program_roles`, `max_programs`, the trial, the revision rule | A written program with no effect is refused; one with an effect is saved and runs; a revision of a program with no failures is refused |
| 7 | `mlx` provider, `[llm.sampling]`, the compatibility row | The Track-L matrix on MLX; the phase-5 cases run on MLX at `max_concurrent = 4`, and the result is recorded against Ollama's |
| 8 | `ask()` and `post()` in live processes; docs, the glossary, the spec (R-PROG, R-BOARD) | Each phase's cases pass on a local model |

Phases 1 and 7 do not depend on the rest. Phase 7 can move earlier.

---

## 13. Open questions

| Question | Proposal |
|---|---|
| `restricted_post` | **Default off, not settable by written programs.** With it off, a planner that reads a result cannot post in that turn, and what it records is tainted, so a later turn that reads those notes cannot post either: five's planning from results does not work. With it on, text from a page can steer a program inside its declared reach. This is the one decision here that trades safety for autonomy, and it needs an operator's answer |
| The name | **Program** for the code, and **coded sub-agent** for the delegation. Five's word, "worker", is taken: `AgentWorker` and the worker roles. "Program" is not free either: the docs say "a process's program" and "process programs". The two are the same kind of thing, a sandboxed module that drives the model, so the glossary defines **program**, and "delegated program" where the two must be told apart |
| A new tool or a `run_agent` argument | **An argument**: it adds no tool schema (`deferred-tool-schemas.md`), and delegation is already where a model looks for it |
| `ask()` in ordinary tools (option A) | **Later**: `ask` has no reach, so it would not break `tool-facilities.md` §7, but a long tool call holding a turn is a separate question |
| The alike window | **30 minutes**, the only value measured. A longer one would refuse legitimate repeats on a recurring goal |
| The board's order | **Oldest first.** Priorities wait for a case that needs them; five ran first come, first served |
| Parallel calls inside a program | **No**: the guest is synchronous. Parallelism comes from several programs through `run_agents` |
| A step limit for sub-agents | **Not here.** Five's evidence is about `agent()`-sized loops. Phase 0's timeouts will show whether Nine's sub-agents need one |
| Fair sharing between posters | **Later.** `max_queued` bounds a flood per poster, and a busy poster can still hold every slot |

---

## Limits

- Whether Nine's sub-agents fail as five's single agent did is unmeasured. Nine's
  `delegate-subagent` case passes 3 of 3 on 4b, and that case does not grade claims.
- `cited` shows that the evidence carries a claim. It does not show the evidence is right or
  current, or that the claim matters.
- `says` and `novel` are lexical: a paraphrase in other words is dropped, and the same claim
  from two sources in different words is kept twice.
- The board's results are restricted when they start a turn, and that restriction has the
  same gaps as piped reports: a restricted turn can still create files or plant a memory.
- A waited-for program's result reaches its parent unrestricted, as a sub-agent's does today.
  Its text can carry what the program read.
- Taint is not tracked through programs, as it is not through process programs: a program that
  reads a tainted file through `nine:fs` is not restricted, and may pass the text to `agent()`.
- A program's own `nine:fs` writes are bounded by its grants, not by `[hitl] require_approval`.
- Undo restores files as they were before the run. If another task changed a file after the
  failed run did, that change is lost. Five has the same limit, and nothing locks a file
  between concurrent programs.
- A task that waits on an approval holds its slot, and with `max_running = 1` it holds the board.
- The trial shows that a written program has an effect, not that the effect is good. It cannot
  run a program whose `agent()` needs `shell`, a plugin or memory tools.
- The evidence for the pattern is one experiment: one domain, runs counted by hand, a 4B model
  on one 16 GB machine. Phase 0 runs three domains for this reason.
- Concurrency gains depend on the model server. With four calls at once, five measured MLX
  3× faster than Ollama, and `max_concurrent` alone does not buy parallel decoding. Ollama's
  gain over running in sequence was about 18 %.
- MLX is Apple silicon only, checks no free memory, and has not been run with Nine.
- `Format` on Mistral has not been tested in Nine.
