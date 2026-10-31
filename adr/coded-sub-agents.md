# Design note — Coded sub-agents

**Status:** Proposed (2026-10-31) ·
**Related:** `adr/process-sessions.md`, `adr/tool-facilities.md`, `adr/roles-design.md`,
`adr/generated-tool-authoring-loop.md`, `adr/deferred-tool-schemas.md` ·
**Amends:** R-ORCH.1–2 (a delegation may run a program), `adr/tool-facilities.md` §7 (the
containable shape it describes, built for one host call), R-PROC (live processes gain `ask`),
the glossary's "there is no Task entity" (§5)

A **coded sub-agent** is a delegation whose child is a **program** instead of a ReAct loop.
A program is a sandboxed tool that decides the steps of a finite task. It asks the model only
for language and judgment, in small calls with fresh contexts. Before anything is kept, it
checks what the model said against **evidence**: what the run's tools actually returned.

Programs run two ways. `run_agent` and `run_agents` gain a `program` argument, for a parent
that waits for the result. **The board** is for work posted without waiting: a durable pool of
tasks that the daemon drains as slots free, that programs can post to themselves, and whose
record every session can read. Either way the run is the same: its own session, a timeout,
the depth guard and priority 3.

This note proposes a mechanism, not an application: the program shape, three host calls, the
board, a set of runtime checks, and a check library. Nothing in it is about a domain.

---

## 1. Problem

Delegated work in Nine is a sub-agent: a ReAct loop under a leaf role, given the task as its
first message. On a small model the loop fails in ways that do not depend on the task:

| Failure | What it looks like |
|---|---|
| Repeating a step | the same call made again and again with the same arguments |
| Running out of steps with nothing kept | the work is done, and the loop ends before the answer is written |
| Answering from memory | claims with nothing the run read behind them |
| A result that says nothing | `{"status": "ok"}` or `"done"`, taken as success by the parent |
| Malformed structure | JSON that does not parse, or does not match what was asked |
| Reaching past its job | a delegate that should only read edits a file another component depends on |

The loop leaves all of this to the model: what to do next, when to stop, what to keep, and
whether it is true. A program takes those decisions in code. The model gets single,
well-posed requests, and code checks each answer.

Nine has two homes for code that calls a model, and neither fits a finite delegated task:

| | Calls the model | Fits a delegated task? |
|---|---|---|
| A sandboxed tool | no | no: it cannot ask the model anything |
| A live process | `turn(text)`, in its own session | no: it runs until stopped and returns no result, its session history grows every turn, and a conversation reaches it only asynchronously |

Delegation is also synchronous only. A parent that wants ten things researched waits through
all ten. No delegate can hand a follow-up to another. And when the turn ends, nothing is left
of what was asked and what came back except that session's history. Workflows record a plan,
but they are passive by design: nothing runs a step unless a model calls a `workflow_*` tool.

Nothing in Nine checks a delegate's claims against what it read.

---

## 2. Where the pattern comes from

Five is an experiment that runs this pattern on qwen3.5:4b alone. Its workload was news
research, but nothing in the mechanism is about news. Its findings, from 30-minute runs:

| Change (generic) | Effect |
|---|---|
| A board: the delegator posts and moves on, programs post follow-ups, and every result stays readable | several runs at once on one small model, and the delegator chose its next tasks from the record |
| Delegates as code, the model for single steps | first results, but the delegating model wrote programs that did nothing |
| The result must be text; a written program's trial must have an effect; the delegator's tasks must point at something it read | the useless programs and tasks invented from memory stopped |
| Constrained decoding and a schema for structured asks | no more failed parses |
| A warning two calls before the step limit; when the agent leaves no answer, ask again from the evidence itself | runs that kept nothing went from 9 of 13 to 0 of 16, and kept results doubled (39 to 78) |
| Claims kept only when the evidence carries them | every kept claim traceable to a source it was checked against |

What did **not** help: prompt changes, or letting the model write more of its own programs.
The best run wrote none. The counts were made by hand, in one domain. Phase 0 (§10) turns
them into evals across several domains, so Nine measures the result rather than inheriting it.

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
| A delegation is already finite and returns a result. It has a timeout, the depth guard, a child session in the journal, and priority 3 | `run_agents`' 120 s default is short for a program that reads and checks |
| `run_agents` already runs several at once, bounded by the LLM queue | A program's model calls need a role, while a sub-agent has one role for its whole loop |
| Workflows already record delegated steps, and roles already gate who may delegate | |

**C. As processes.** Send work to a live process with `process_send`, and read its results
from what it writes.

| For | Against |
|---|---|
| Built, with budgets, health and versions | Asynchronous: the sender cannot wait for a result |
| | `turn()` appends to the session's history, so contexts grow |

**Decision: B, built on the process sandbox, with a board (§5) for the work posted without
waiting.** Processes stay the home for work driven by a
clock or by events. A is rejected for now (§11).

---

## 4. The mechanism

**A program** is a sandboxed tool with a `program` declaration. Any tier may write one:
shipped, developer or generated. Like a live tool, it is never in a tool list. A delegation
runs it, calling its default export with `{ task }`. Its return value is the delegation's
result.

```toml
[tool.<name>.program]
role    = "monitor"     # bounds its agent() calls: their tools are a subset of this role's
timeout = "6m"          # replaces the delegation's default
writes  = true          # its effect is files; the trial (§6) checks it has one
```

**Calling.** `run_agent({ program: "<name>", task: "…" })`. `role` and `program` are
mutually exclusive, and `run_agents` takes either form in each entry. The roles that may
delegate today may delegate to a program. This adds an argument, not a tool: about 40
tokens of schema.

**The host calls (`nine:llm`).** They block, like `next()` and `turn()`, because the guest
is synchronous.

| Call | Context | Reach |
|---|---|---|
| `ask(prompt, { system?, schema?, max_tokens? })` | fresh: a system line and the prompt, nothing else. With `schema`, it uses the provider's constrained decoding where it has one. The reply is parsed and validated against the schema (§6) | none: no tools |
| `agent({ prompt, system?, tools, max_steps? })` | fresh: a ReAct loop in the child session, with only `tools` | `tools`, a subset of the program's `role`. This is the shape `tool-facilities.md` §7 called containable: a named allowlist under its own role, depth 1, and the closure printed in the roster |
| `evidence()` | — | what this run's tools returned: from its `agent()` calls and its own `fs` and `net.http`. Each entry is `{ source, kind, text }`, where the source is a URL, a workspace path or a command, and the kind is page, search, file, http or command |

Every call is journaled in the child session, as a sub-agent's model and tool calls are
today. The parent sees only the program's result, spilled when long.

**Why fresh contexts.** A program's model calls see each other only through what its code
passes along. No prompt grows with the length of the run, which is what keeps a small model
inside the size of context it handles well. A sub-agent's history grows with every step, and
a process's with every turn.

**Three shapes, one mechanism.** Illustrations, not features:

| Program | `agent()` with | `ask()` for | Checked against `evidence()` |
|---|---|---|---|
| Answer a question from the web | `web_search`, `web_page_read` | claims, each with a source and a quote | the pages it read |
| Find where a behavior is implemented | `read_file`, `file_search_text` | locations, each with a path and the lines quoted | the files it read |
| Explain a failure from logs | `shell` under a read-only role | the cause, with the lines it rests on | the command output |

---

## 5. The board

`run_agent({ program })` is a parent waiting for a result. The board is the other half: work
posted without waiting, run when a slot frees, and recorded where anyone can read it.

**A task** is one program run on the board:

| Field | |
|---|---|
| `program`, `task` | what runs, and its input |
| `posted_by` | a session, or the task whose program posted it |
| `depth` | 0 when posted by a session; one more than its poster when posted by a program |
| `status` | `queued`, `running`, `done`, `failed`, `cancelled` |
| `result` | the program's text, or the failure's reason |
| `session` | the run's child session, for its journal |

**Posting.**

| From | With |
|---|---|
| A conversation or a process session | `task_post({ program, task })`, granted per role like `run_agent` |
| A program | `post(program, task)` from `nine:board` |
| The operator | `nine task post`, `POST /api/v1/tasks` |

A post is admitted or refused on the spot, with a reason. The poster is a model or a program
that can act on the refusal. The checks that apply are the runtime checks of §6: the program
exists, `accepts(task)`, alike tasks, and the poster's budget. One more applies to posting:
depth. A task at `[board] max_depth` (default 2) cannot post. A program may hand work on, and
that work may hand work on once, but chains end, as processes' lineage does.

**Running.** A driver in the daemon takes queued tasks oldest first, up to
`[board] max_running` at once (default: the LLM queue's `max_concurrent`). Each runs exactly
as `run_agent({ program })` runs it. `run_agent({ program })` also records its run on the
board, as a task that starts `running` at once. Waited-for and posted runs therefore share one
record and one alike check, and a waited-for run never queues behind posted ones.

**Results.** A finished task's result is delivered to its poster:

| Poster | Receives the result as |
|---|---|
| A conversation | a queued message (`docs/queued-messages.md`), so it gets a turn even if the model never opens the queue |
| A process session | a message, which is a trigger, as a pipe's report is |
| A program | nothing: its run has ended. The result is on the board for whoever reads it |
| Gone (deleted, archived) | the human feed |

A result is data a program produced from what it read, so it is an injection path, as piped
reports are. It is delivered framed with its task, as `[Result of task #12 (<program>): …]`,
and the turn it starts is restricted as a piped turn is (`adr/process-sessions.md`, Limits).

**Reading and control.**

| Tool | Does |
|---|---|
| `task_list({ status?, program?, posted_by? })` | the board, newest first, with each result's first line |
| `task_get(id)` | one task: its input, result, timings and session |
| `task_cancel(id)` | stops a queued or running task, allowed to its poster and to the operator |

The operator has `nine task list|show|cancel`, `/api/v1/tasks`, and a web UI page.

**Restarts.** At boot, a task left `running` is queued again once. If it is interrupted
again, it fails as "interrupted twice". A program's writes are its own concern: a program
that must not run twice says so in `accepts`.

**Retention.** Finished tasks stay for `[board] retention` (default 30 days), then go with
their sessions in the session-retention sweep.

**Why not workflows.** A workflow is an ordered plan a model keeps, with no driver.
`spec/overview.md` §3.1 makes the difference between workflows and goals a matter of
autonomy, and the board is autonomous. Giving workflow steps a driver would blur that line.
A board is a pool with no order, and a workflow step may still name the task that carries it
out.

---

## 6. Checks

Checks come in two kinds. **Runtime checks** hold for every program, whatever its code does.
**Content checks** are a library a program calls: the runtime cannot know what a claim is in
a given task, but it can make checking one a single line of code.

### Runtime checks

| Check | What it does | Failure it closes |
|---|---|---|
| **Result is text** | refuses `null`, `""`, an object, an array, `{ error }`, or fewer than 4 words. The run fails, and the reason reaches the parent | a result that says nothing |
| **Structured replies** | `ask` with `schema` validates the parsed reply. On a mismatch it asks once more, giving the validation error. Then it throws | malformed structure |
| **Step warning** | two calls before `max_steps`, `agent()` adds: "two model calls left: reply now with what you have, without a tool call" | running out of steps with nothing kept |
| **Repeated calls** | `agent()` refuses a tool call identical to an earlier one in the same call and returns the earlier result's location instead | repeating a step |
| **Alike delegations** | refuses a `program` and `task` alike to one running, or done within `[programs] alike_window` (default 6 h). The refusal names the earlier run and its result | the same work delegated again and again |
| **Task admission** | a program may export `accepts(task)`, which runs before anything else with no model access. A string it returns refuses the delegation with that reason | delegations built from memory: a program can require that the task point at something that exists, such as a file, a URL the parent read, or a record |
| **Trial** (written programs) | before a write is saved, the program runs on a sample task in a copy of the workspace. It must return text and, when `writes`, change a file | written programs that do nothing and report success |
| **Own state** | a program keeps its state in `nine:state` or the database, never in workspace files another delegate can edit | another component damaging a program's state |

Admission checks (alike, task admission, budget, and depth on the board) run when a task is
posted or delegated, before it queues, so the poster learns of the refusal at once.

Task admission is the generic form of five's grounding rule. Five's research program refused
any task that did not cite a link from the news it had read. Another program might require
an existing path or an open record. The runtime only runs the check; the program states it.

### Content checks (`nine:check`)

Pure functions over text, shipped and tested like the stdlib. They work on any evidence:
pages, files, command output.

| Function | Returns |
|---|---|
| `quoted(text, quote)` | whether the quote is in the text, normalized for case, punctuation and whitespace: whole, or `run` words in a row (default 8) |
| `says(text, claim)` | the sentence of the text (or two in a row) that has a share of the claim's content words (default 60 %) and **every number** in it. Otherwise `undefined` |
| `cited(claims, evidence)` | splits `[{ claim, source, quote }]` into `{ kept, dropped }`. A claim is kept when its quote is `quoted` in its source and carries the claim's numbers, or when any evidence `says` it, which then becomes its source. Each dropped claim has a reason. A named source the run never read may be fetched once, through the program's own grants |
| `novel(text, existing)` | whether the text is new: not already present, and not a substring of a longer entry or containing one |

The idea five's results rest on is "the model finds and words; code checks and writes". A
program asks for claims with their sources, keeps what `cited` keeps, and writes only that.

---

## 7. Writing programs

`tool_write` with a `program` block, gated by `[tools.agent] allow_programs` (off by
default). The write passes today's checks: parse, the declaration against the ceiling, and
approval. Then it passes the trial, which reuses the dry run of
`generated-tool-authoring-loop.md`. A written program's `role` must be in
`[tools.agent] program_roles` (default `["monitor"]`: read-only), as `process_roles` bounds
written processes. Versions and rollback are the generated tier's.

Writing is off by default because five's evidence points that way. Programs written by a 4b
model added nothing until the trial refused them, and the best run wrote none. The value is
the mechanism, used by programs the operator or a developer writes. Writing programs is an
option, not the feature.

---

## 8. What ships

| | Content |
|---|---|
| `program` declarations and `run_agent({ program })` | §4 |
| `nine:llm` (`ask`, `agent`, `evidence`) | §4 |
| The board | §5 |
| The runtime checks | §6 |
| `nine:check` | §6 |
| The `program-patterns` skill | the three shapes of §4 as complete programs, each run by a test, and the common mistakes, as `process-patterns` is for processes |

No domain program ships. The skill's examples are examples.

---

## 9. What this amends

| | Today | With coded sub-agents |
|---|---|---|
| R-ORCH.1–2 | A delegation runs a ReAct loop under a leaf role | …or a program, whose `agent()` calls run under its role |
| `tool-facilities.md` §7 | Tool→tool dispatch is rejected; a containable shape is recorded | That shape is built for `agent()` only |
| R-PROC | A live process asks the model only with `turn()` | Live processes may also `ask()`, so a process that needs no history stops growing its session |
| Glossary, "Work unit" | There is no Task entity: finite work is a sub-agent or a workflow step | A **task** is a program run on the board. It adds no new kind of work, only a record of one: a task runs as a coded sub-agent |
| `run_agents` timeout | 120 s for every child | A program's `timeout` overrides it for its entry |

---

## 10. Phases

| # | Content | Acceptance |
|---|---|---|
| 0 | Live cases in three domains: a web question, a code location and a log diagnosis. Each is delegated to `executor` on 4b and 9b and graded on side effects: claims whose quote is in their source, claims without one, results that say nothing, and runs that kept nothing | Recorded on `main`: the baseline |
| 1 | `program` declarations (developer and shipped tiers), `run_agent({ program })`, `nine:llm` | Replay snapshots of existing delegation unchanged; a program run journals as a child session |
| 2 | The runtime checks | Each refusal has a case that fails for its reason |
| 3 | The board: tasks, `task_post`, `post()`, the driver, delivery, `task_list`, `task_get`, `task_cancel`, the operator surface | Ten tasks posted at once all finish with at most `max_running` running; a program's follow-up runs; a post at `max_depth` is refused; a result reaches its conversation as a queued message, and its turn is restricted |
| 4 | `nine:check`, the `program-patterns` skill, and each phase-0 case rewritten as a program | On 4b, each domain keeps more checked claims than its baseline and writes **no** unchecked ones, in 3 of 3 runs |
| 5 | Written programs: `allow_programs`, `program_roles`, the trial | A written program with no effect is refused; one with an effect is saved and runs |
| 6 | `ask()` in live processes; docs, the glossary, the spec (R-PROG, R-BOARD) | Each phase's cases pass on a local model |

---

## 11. Open questions

| Question | Proposal |
|---|---|
| The name | **Program** for the code, and **coded sub-agent** for the delegation. Five's word, "worker", is taken: `AgentWorker` and worker roles |
| A new tool or a `run_agent` argument | **An argument**: it adds no tool schema (`deferred-tool-schemas.md`), and delegation is already where a model looks for it |
| `ask()` in ordinary tools (option A) | **Later**: `ask` has no reach, so it would not break §7, but a long tool call holding a turn is a separate question |
| How far alike delegations reach | **Daemon-wide**, within the window. Checking per session misses `pursue` delegating the same task at every tick |
| The board's order | **Oldest first.** Priorities wait for a case that needs them; five ran first come, first served |
| A flood of posts | A per-poster cap on queued tasks, `[board] max_queued` (default 20), beside the budgets |
| Results to a busy conversation | **A queued message**, so a result never interrupts a turn and is never lost |
| Parallel calls inside a program | **No**: the guest is synchronous. Parallelism comes from several programs through `run_agents` |
| `evidence()` and size | It keeps each entry's full text for the run, in the run's spill namespace, never in a prompt. The cap is open |

---

## Limits

- `cited` shows that the evidence carries a claim. It does not show the evidence is right or
  current, or that the claim matters.
- `says` and `novel` are lexical: a paraphrase in other words is dropped, and the same claim
  from two sources in different words is kept twice.
- The board's results are restricted when they start a turn, and that restriction has the
  same gaps as piped reports: a restricted turn can still create files or plant a memory.
- The trial shows that a written program has an effect, not that the effect is good.
- The evidence for the pattern is one experiment: one domain, runs counted by hand, a 4B model
  on one 16 GB machine. Phase 0 runs three domains for this reason.
- Concurrency gains depend on the model server. With four calls at once, five measured MLX
  about 3× faster than Ollama. Nine's providers are Ollama and Mistral, and
  `max_concurrent` alone does not buy parallel decoding.
