# Design note — Coded sub-agents

**Status:** Proposed (2026-10-30) ·
**Related:** `adr/process-sessions.md`, `adr/tool-facilities.md`, `adr/roles-design.md`,
`adr/generated-tool-authoring-loop.md`, `adr/deferred-tool-schemas.md` ·
**Amends:** R-ORCH.1–2 (a delegation may run a program), `adr/tool-facilities.md` §7 (the
containable shape it describes, built for one host call), R-PROC (live processes gain `ask`)

A **coded sub-agent** is a delegation whose child is a **program** instead of a ReAct loop:
a sandboxed tool that decides the steps of a finite task and asks the model only for
language and judgment, in small calls with fresh contexts. The program also **checks** what
the model says before anything is kept. `run_agent` and `run_agents` gain a `program`
argument. Everything else about a delegation stays the same: the parent waits for the
result, and the child gets its own session, a timeout, the depth guard and priority 3.

The pattern comes from five, an experiment that runs a news desk on qwen3.5:4b with nothing
larger. Five's results went up when code took over the structure around the model, and
never when the prompts changed. Nine's processes already let code ask the model for a turn,
but only for long-running background work. This note covers the other half: finite work
that a model delegates and gets a result back from.

---

## 1. Problem

Delegated work in Nine is a sub-agent: a ReAct loop under a leaf role, with the task as its
first message. On a small model, that loop is where five failed before it changed shape.
Each of these was observed in a 30-minute run:

| Failure | Observed in five |
|---|---|
| Repeating a step | the same page fetched 12 times in one run |
| Running out of steps with nothing written | 9 of 13 research runs kept nothing: the agent read pages and never wrote its notes |
| Reporting from memory | facts with no page behind them, and research tasks for stories nobody had read |
| A result that says nothing | `{"status": "ok", "factsFound": 0}` taken as done |
| Writing what it should only read | the delegating model emptied another worker's state file, and 64 lines were duplicated after it |

Nine has two homes for code that calls a model, and neither fits a finite delegated task:

| | Can call the model | Fits a delegated task? |
|---|---|---|
| A sandboxed tool | no | no: it cannot ask the model anything |
| A live process | `turn(text)`, in its own session | no: it runs until stopped and returns no result. Its session history grows with every turn, and a conversation reaches it only asynchronously |

Nothing in Nine checks that what a sub-agent reports was in what it read.

---

## 2. The pattern, as five runs it

| Part | In five |
|---|---|
| Lead | a fixed agent that reads the news files and posts tasks to a board |
| Board | the tasks, their inputs, who posted them and their results. An alike task is refused |
| Task worker | an ES module, `export default async ({ task }) => string`, run in a child process |
| `five.llm.ask(prompt, { system?, schema? })` | one model call with a fresh context and no tools. With a schema, decoding is constrained (greedy on MLX) and the reply comes back parsed |
| `five.llm.agent({ prompt, system?, tools, maxSteps? })` | a small agent with only the tools named and a fresh context. It is warned two calls before its last step |
| `five.web.read()` | every page and search result this run's agents read: the evidence to check their reports against |
| Checks | in the runtime: the result must be text, a written worker's trial must write a file, an alike task is refused, a research task must cite the news. In the worker: each fact's quote is checked against a page that was read |

The shipped research worker is about 250 lines. It picks the story with `ask` and a schema.
It lets an agent with only `search_web` and `fetch_url` read. It asks for facts as JSON,
and if the agent left no notes it asks again from the pages themselves. Then it keeps only
the facts whose quote, or a sentence carrying the same claim, is on a page the run read. The
model finds and words; code checks and writes.

What the changes did, in five's 30-minute runs on 4b:

| Run | Change | Facts kept |
|---|---|---|
| 11 | workers as code; the lead free to post and write workers | 40. The lead wrote useless workers and posted stories from memory |
| 13 | grounding, the trial that must write a file, results that must be text | 32. The rules held; the agent looped under greedy sampling |
| 15 | constrained JSON for `ask` | 39, with no JSON failures. 9 of 13 runs were empty |
| 16 | the step warning, the facts asked again from the pages read | **78 in 13 stories. All 16 research runs kept facts, and no new workers were written** |

These counts were made by hand, and all the runs are about one domain. Phase 0 (§9) turns
them into an eval.

---

## 3. Options

**A. As tools.** Give a sandboxed tool the `llm` capability, and let a model call it in a
turn like any other tool.

| For | Against |
|---|---|
| The smallest surface: a name in the tool list | A call that runs for minutes holds the caller's turn and a sandbox slot |
| | `agent()` with tools is the tool→tool dispatch `tool-facilities.md` §7 rejects: the tool's reach becomes the closure of what its agent can call |
| | No parallelism beyond what one reply's tool calls give |
| | Tool ranking and deferred schemas decide whether the model sees it at all |

**B. As sub-agents.** `run_agent({ program, task })`: the delegation is unchanged, but its
child is code.

| For | Against |
|---|---|
| A delegation is already finite, returns a result, has a timeout, a depth guard, a child session in the journal, and priority 3 | `run_agents`' default timeout (120 s) is too short: five's research runs take 2 to 5 minutes |
| `run_agents` already runs several at once, bounded by the LLM queue | A program's own model calls need a role, while a sub-agent has one role for its whole loop |
| Workflows already record delegated steps | |
| Roles already gate who may delegate | |

**C. As processes.** Send work to a live process with `process_send`, and read its results
from files.

| For | Against |
|---|---|
| Built, with budgets, health and versions | Asynchronous: the sender cannot wait for a result |
| | `turn()` appends to the session's history, so contexts grow: the opposite of five's fresh calls |

**Decision: B, built on the process sandbox.** A coded sub-agent is a delegation (B), and its
program is a sandboxed tool with the host calls of §4. C stays the home for the clock-driven
half of five: its `feeds` worker is a process today. A is rejected for now (§10).

---

## 4. Decision

**A program** is a sandboxed tool with a `program` declaration. Any tier may write one:
shipped, developer or generated. Like a live tool, it is never in a tool list. A delegation
runs it.

```toml
[tool.research.program]
role        = "monitor"          # the role of its agent() calls: their tools are a subset of it
timeout     = "6m"               # replaces the delegation's default
cite_from   = ["news/*.md"]      # §5: the task must cite one of these files, or omit
```

```js
import { ask, agent, read } from "nine:llm";
import { sourced, novel } from "nine:check";

export default ({ task }) => {
  const notes = agent({ system: RESEARCH, prompt: task, tools: ["web_search", "web_page_read"] });
  const { facts } = ask(`Notes:\n${notes}\n\nThe facts, each with its URL and a sentence from it.`,
                        { schema: FACTS });
  const { kept, dropped } = sourced(facts, read());
  appendFile(`stories/${slug}.md`, kept.filter((f) => novel(f.quote, before)).map(line).join(""));
  return `${kept.length} checked facts added to stories/${slug}.md; ${dropped.length} dropped.`;
};
```

**Calling.** `run_agent({ program: "research", task: "…" })`. `role` and `program` are
mutually exclusive. `run_agents` takes either form in each entry. A program is delegable by
the roles that may delegate today: no new tool, and about 40 tokens of schema.

**The host calls, `nine:llm`.** They block like `next()` and `turn()`, since the guest is
synchronous.

| Call | Context | Reach |
|---|---|---|
| `ask(prompt, { system?, schema?, max_tokens? })` | fresh: a system line and the prompt, nothing else. With `schema`, the provider's constrained decoding when it has one, otherwise the schema in the system prompt and the JSON parsed out of the reply | none: no tools |
| `agent({ prompt, system?, tools, max_steps? })` | fresh: a ReAct loop in the child session with only `tools` | `tools`, which must be a subset of the program's `role`. This is the containable shape of `tool-facilities.md` §7: a named allowlist under its own role, depth 1, and the closure printed in the roster |
| `read()` | — | the pages and search results this run's `agent()` calls and `net.http` requests returned, as `[{ url, text }]` |

Every call is journaled in the child session as a sub-agent's model and tool calls are
today. The parent sees only the program's result, which is spilled when it is long.

**Why fresh contexts.** A program's model calls never see each other except through what the
code passes along, so no call's prompt grows with the length of the run. A sub-agent's
history grows with every step. A process's grows with every turn.

---

## 5. Checks

The user-visible half of the pattern. They fall into two kinds. **Runtime checks** hold for
every program, whatever its code does. **Content checks** are a shipped library that a
program calls, because what counts as checked depends on the work.

### Runtime checks

| Check | Refuses | Why (five) |
|---|---|---|
| **The result is text** | `null`, `""`, an object, an array, `{ error }`, or fewer than 4 words. The run fails, and the reason goes to the parent | `{"status": "ok", "factsFound": 0}` read as a result to a 4b parent |
| **The step warning** | nothing. `agent()` adds "two model calls left: reply now with what you have, without a tool call" two calls before `max_steps` | 9 of 13 runs ran out of steps without notes (run 15) |
| **An alike delegation** | a `program` and `task` alike to one running, or done in the last `[programs] alike_window` (default 6 h), naming the earlier run and its result | the lead posted the same research again and again |
| **Grounding** (`cite_from`) | a task that cites no link or path from the declared files, saying which files it may cite | the lead posted stories from memory (run 11) |
| **The trial** (a written program) | a program whose trial run on a sample task, in a copy of the workspace, writes no file, or returns no text | the lead wrote workers that did nothing and reported success |
| **A program's own state** | nothing new: a program keeps its state in `nine:state` or the database, never in workspace files | five kept a worker's state in a dot file by convention, and the delegating model emptied it (run 16) |

Grounding and the trial apply to whatever delegates or writes, whether a person's
conversation or `pursue`. An operator who wants unrestricted delegation omits `cite_from`.

### Content checks, `nine:check`

Pure functions, shipped and tested like the stdlib. They port five's research worker:

| Function | True when |
|---|---|
| `quoted(page, quote)` | the quote is on the page, normalized for case, quotes and whitespace: whole, or 8 words of it in a row |
| `says(page, claim)` | a sentence of the page (or two in a row) has 60 % of the claim's content words and **every number** in it. Returns that sentence |
| `sourced(facts, pages)` | per fact: its quote is `quoted` on its page and has the fact's numbers, or a page `says` the fact. A named page the run never read is fetched once. Returns `{ kept, dropped }`, each with a reason, and a kept fact carries the page's sentence |
| `novel(quote, existing)` | the quote is not already there, **including as a substring of a longer quote, or containing one**. Five checked only identical quotes and kept near-duplicates |

The shipped `research` program uses all four, so delegating research is checked by default.
`sourced` shows that a page says something, not that the page is right (Limits).

---

## 6. Writing programs

`tool_write` with a `program` block, gated by `[tools.agent] allow_programs` (off by
default). The write passes today's checks: parse, the declaration against the ceiling, and
approval. It also passes the trial (§5), which reuses the dry run of
`generated-tool-authoring-loop.md`. The `role` of a program Nine writes must be in
`[tools.agent] program_roles` (default `["monitor"]`: reading only), as `process_roles`
bounds written processes. Versions and rollback are the generated tier's.

It is off by default for a reason five found: its best run wrote no programs. Before the
trial check, the programs a 4b model wrote added nothing, and one rewrote the shipped
research worker for the worse. The value is in shipped programs and the checks. Writing
them is an option, not the feature.

---

## 7. What ships

| | Content |
|---|---|
| `research` | five's research worker, ported. Its agent gets `web_search` and `web_page_read`, and it appends checked facts to a story file |
| `program-patterns` skill | complete programs with tests, and the common mistakes, as `process-patterns` is for processes |
| `nine:llm`, `nine:check` | §4, §5 |
| An example process | five's `feeds`: on a clock, fetch the feeds, keep what is new in `nine:state`, and write the day's news file that `research` tasks cite |

---

## 8. What this amends

| | Today | With coded sub-agents |
|---|---|---|
| R-ORCH.1–2 | a delegation runs a ReAct loop under a leaf role | …or a program, under the program's role for its `agent()` calls |
| `tool-facilities.md` §7 | tool→tool dispatch rejected; a containable shape recorded | that shape, built for `agent()` only: a named allowlist, its own role, depth 1, the closure printed |
| R-PROC | a live process asks the model only with `turn()` | live processes may also `ask()`: a summarizer that needs no history stops growing its session |
| `run_agents` timeout | 120 s for every child | a program's `timeout` overrides it for its entry |

---

## 9. Phases

| # | Content | Acceptance |
|---|---|---|
| 0 | A `research-story` live case: the same story delegated to `executor` on 4b and 9b, graded on side effects. It counts facts with a quote `quoted` on the cited page, facts without one, and duplicates | Recorded on `main`: the baseline the other phases are measured against |
| 1 | `program` declarations (developer and shipped tiers), `run_agent({ program })`, and `nine:llm` (`ask`, `agent`, `read`) | Replay snapshots of existing delegation unchanged; a program run journals as a child session |
| 2 | The runtime checks: text results, the step warning, alike delegations, `cite_from` | Each refusal has a case that fails for its reason |
| 3 | `nine:check` and the shipped `research` program | On 4b, `research-story` through `research` keeps more checked facts than the phase-0 baseline and writes **no** unchecked ones, 3 of 3 runs |
| 4 | Written programs: `allow_programs`, `program_roles`, the trial | A written program that writes no file is refused; one that does is saved and runs |
| 5 | `ask()` in live processes; docs, the glossary, the spec (R-PROG) | Each phase's cases pass on a local model |

---

## 10. Open questions

| Question | Proposal |
|---|---|
| The name | **program**, for the code, and **coded sub-agent**, for the delegation. "Worker", five's word, is taken: `AgentWorker` and worker roles |
| A new tool or a `run_agent` argument | **An argument**: it adds no tool schema (`deferred-tool-schemas.md`), and delegation is already where a model looks for it |
| `ask()` in ordinary tools (option A) | **Later**: `ask` has no reach, so it would not break §7, but a long tool call holding a turn is its own question |
| The alike-delegation scope | **Daemon-wide** for a window. Per session misses `pursue` posting the same task at each tick |
| Parallel calls inside a program | **No**: the guest is synchronous, and five never needed it. Parallelism is several programs through `run_agents` |

---

## Limits

- `sourced` proves that a page read in the run carries the quote or the claim. It does not
  prove the page is right, current, or that the fact matters.
- `says` and `novel` are lexical. A paraphrase with different words is dropped, and the same
  fact from two pages in different words is kept twice.
- The trial proves a written program writes something and says so, not that what it writes
  is good. Five's lead wrote programs that passed and added nothing.
- The evidence is one experiment: one domain (news), 30-minute runs counted by hand, and a 4B
  model on one 16 GB machine. Phase 0 exists so Nine measures it instead of inheriting it.
- Concurrency gains depend on the model server. With four calls at once, five measured
  MLX about 3× faster than Ollama (22 s against 66 s). Nine's providers are Ollama and
  Mistral, and `max_concurrent` alone does not buy parallel decoding.
