# Design note — Coded sub-agents

**Status:** Proposed (2026-10-31) ·
**Related:** `adr/process-sessions.md`, `adr/tool-facilities.md`, `adr/roles-design.md`,
`adr/generated-tool-authoring-loop.md`, `adr/deferred-tool-schemas.md` ·
**Amends:** R-ORCH.1–2 (a delegation may run a program), `adr/tool-facilities.md` §7 (the
containable shape it describes, built for one host call), R-PROC (live processes gain `ask`)

A **coded sub-agent** is a delegation whose child is a **program** instead of a ReAct loop.
A program is a sandboxed tool that decides the steps of a finite task. It asks the model only
for language and judgment, in small calls with fresh contexts. Before anything is kept, it
checks what the model said against **evidence**: what the run's tools actually returned.

`run_agent` and `run_agents` gain a `program` argument. Everything else about a delegation
stays as it is: the parent waits for the result, and the child has its own session, a
timeout, the depth guard and priority 3.

This note proposes a mechanism, not an application: the program shape, three host calls, a
set of runtime checks, and a check library. Nothing in it is about a domain.

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

Nothing in Nine checks a delegate's claims against what it read.

---

## 2. Where the pattern comes from

Five is an experiment that runs this pattern on qwen3.5:4b alone. Its workload was news
research, but nothing in the mechanism is about news. Its findings, from 30-minute runs:

| Change (generic) | Effect |
|---|---|
| Delegates as code, the model for single steps | first results, but the delegating model wrote programs that did nothing |
| The result must be text; a written program's trial must have an effect; the delegator's tasks must point at something it read | the useless programs and tasks invented from memory stopped |
| Constrained decoding and a schema for structured asks | no more failed parses |
| A warning two calls before the step limit; when the agent leaves no answer, ask again from the evidence itself | runs that kept nothing went from 9 of 13 to 0 of 16, and kept results doubled (39 to 78) |
| Claims kept only when the evidence carries them | every kept claim traceable to a source it was checked against |

What did **not** help: prompt changes, or letting the model write more of its own programs.
The best run wrote none. The counts were made by hand, in one domain. Phase 0 (§9) turns
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

**Decision: B, built on the process sandbox.** Processes stay the home for work driven by a
clock or by events. A is rejected for now (§10).

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
writes  = true          # its effect is files; the trial (§5) checks it has one
```

**Calling.** `run_agent({ program: "<name>", task: "…" })`. `role` and `program` are
mutually exclusive, and `run_agents` takes either form in each entry. The roles that may
delegate today may delegate to a program. This adds an argument, not a tool: about 40
tokens of schema.

**The host calls (`nine:llm`).** They block, like `next()` and `turn()`, because the guest
is synchronous.

| Call | Context | Reach |
|---|---|---|
| `ask(prompt, { system?, schema?, max_tokens? })` | fresh: a system line and the prompt, nothing else. With `schema`, it uses the provider's constrained decoding where it has one. The reply is parsed and validated against the schema (§5) | none: no tools |
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

## 5. Checks

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

## 6. Writing programs

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

## 7. What ships

| | Content |
|---|---|
| `program` declarations and `run_agent({ program })` | §4 |
| `nine:llm` (`ask`, `agent`, `evidence`) | §4 |
| The runtime checks | §5 |
| `nine:check` | §5 |
| The `program-patterns` skill | the three shapes of §4 as complete programs, each run by a test, and the common mistakes, as `process-patterns` is for processes |

No domain program ships. The skill's examples are examples.

---

## 8. What this amends

| | Today | With coded sub-agents |
|---|---|---|
| R-ORCH.1–2 | A delegation runs a ReAct loop under a leaf role | …or a program, whose `agent()` calls run under its role |
| `tool-facilities.md` §7 | Tool→tool dispatch is rejected; a containable shape is recorded | That shape is built for `agent()` only |
| R-PROC | A live process asks the model only with `turn()` | Live processes may also `ask()`, so a process that needs no history stops growing its session |
| `run_agents` timeout | 120 s for every child | A program's `timeout` overrides it for its entry |

---

## 9. Phases

| # | Content | Acceptance |
|---|---|---|
| 0 | Live cases in three domains: a web question, a code location and a log diagnosis. Each is delegated to `executor` on 4b and 9b and graded on side effects: claims whose quote is in their source, claims without one, results that say nothing, and runs that kept nothing | Recorded on `main`: the baseline |
| 1 | `program` declarations (developer and shipped tiers), `run_agent({ program })`, `nine:llm` | Replay snapshots of existing delegation unchanged; a program run journals as a child session |
| 2 | The runtime checks | Each refusal has a case that fails for its reason |
| 3 | `nine:check`, the `program-patterns` skill, and each phase-0 case rewritten as a program | On 4b, each domain keeps more checked claims than its baseline and writes **no** unchecked ones, in 3 of 3 runs |
| 4 | Written programs: `allow_programs`, `program_roles`, the trial | A written program with no effect is refused; one with an effect is saved and runs |
| 5 | `ask()` in live processes; docs, the glossary, the spec (R-PROG) | Each phase's cases pass on a local model |

---

## 10. Open questions

| Question | Proposal |
|---|---|
| The name | **Program** for the code, and **coded sub-agent** for the delegation. Five's word, "worker", is taken: `AgentWorker` and worker roles |
| A new tool or a `run_agent` argument | **An argument**: it adds no tool schema (`deferred-tool-schemas.md`), and delegation is already where a model looks for it |
| `ask()` in ordinary tools (option A) | **Later**: `ask` has no reach, so it would not break §7, but a long tool call holding a turn is a separate question |
| How far alike delegations reach | **Daemon-wide**, within the window. Checking per session misses `pursue` delegating the same task at every tick |
| Parallel calls inside a program | **No**: the guest is synchronous. Parallelism comes from several programs through `run_agents` |
| `evidence()` and size | It keeps each entry's full text for the run, in the run's spill namespace, never in a prompt. The cap is open |

---

## Limits

- `cited` shows that the evidence carries a claim. It does not show the evidence is right or
  current, or that the claim matters.
- `says` and `novel` are lexical: a paraphrase in other words is dropped, and the same claim
  from two sources in different words is kept twice.
- The trial shows that a written program has an effect, not that the effect is good.
- The evidence for the pattern is one experiment: one domain, runs counted by hand, a 4B model
  on one 16 GB machine. Phase 0 runs three domains for this reason.
- Concurrency gains depend on the model server. With four calls at once, five measured MLX
  about 3× faster than Ollama. Nine's providers are Ollama and Mistral, and
  `max_concurrent` alone does not buy parallel decoding.
