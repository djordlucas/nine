# Design note — Process sessions

**Status:** **Proposed** (revised 2026-10-06: one concept for all background work) ·
**Related:** `adr/standing-tools.md`, `adr/tool-facilities.md`, `adr/reactive-events.md`,
`adr/predefined-agents-design.md`, `adr/roles-design.md`, `adr/personality-pattern.md`,
`adr/agent-boundary.md` · **Amends:** R-SUB.7, the stance of `docs/self-modification.md`, and
the configuration of standing agents and standing tools — see §12

Nine has **two kinds of session**. A **conversation** is driven by a person. A **process
session** is driven by a **process**: a sandboxed tool that runs until stopped, receives its
triggers — a clock, a journal event, a message — and can call the model in its own session. Everything Nine does
between a person's turns becomes a process session: goal pursuit, standing agents,
self-reflection, condition triggers, standing tools, and the processes Nine writes for itself.

| | Driven by | Started by |
|---|---|---|
| Conversation | a person, through the TUI or the API | a message |
| Process session | a process | its triggers |

Processes come from three places: **shipped** with the binary (`pursue`, `reflect`),
**declared** by the operator in `nine.toml`, or **written** by Nine. One scheduler, one health
machine, one budget system and one concurrency limit govern all of them.

The design keeps a **rigid core and a flexible runtime.** The core — the binary, the
configuration, the grants and budgets, the roles, the shipped processes — does not change at
runtime and is never written by Nine. The runtime — the processes Nine writes and its private
database — is Nine's to write, revise and replace, inside the bounds the core sets.

This note comes out of nine-will, an experiment that ran "instances made of code they write"
outside Nine, with Nine as its model runtime. Its capable parts were rebuilds of what Nine
already has; it moves here, and §13 records what carries over.

---

## 1. Problem

Work between a person's turns runs through six mechanisms, each with its own configuration,
trigger handling, limits and failure behavior:

| Mechanism | Declared by | Trigger | Runs | Limit |
|---|---|---|---|---|
| Goal session | an agent (`goal_create`) or `POST /goals` | every 5 min | a `pursue` routine's prompt | `max_goal_sessions` |
| Standing agent | `[[agent]]` | `interval` / `schedule` | a goal session with a `monitor` role | `max_goal_sessions` |
| Self-reflection session | `[daemon] self_reflection` | interval | an `idle-reflection` routine's prompt | none |
| Condition trigger | `[[agent]] when = {…}` | interval | a standing run that may wake an agent | none of its own |
| Standing tool | `[[standing_tool]]` | `interval` / `schedule` | a resumable tool, no model | `max_standing` |
| Reaction | designed (`tool-facilities.md` §6), unbuilt | a journal event | a tool, no model | — |

A user learns six concepts to understand what runs in the background. A budget, a health rule
or a loop limit has to be built six times, and most of them have none: a standing agent has no
turn or token budget at all.

Meanwhile a seventh shape has no home: **code that decides when and what, and asks the model
only for language or judgment.** A digest that fetches feeds and needs one summary; a check that
runs every ten seconds and needs the model once a week. Today that is either a standing agent,
which spends a full turn deciding what a program could, or a standing tool, which cannot ask the
model anything.

---

## 2. Decision

**A process** is a sandboxed tool with a `process` declaration: its triggers, its role, its
budget. Source, schema and capabilities are those of any sandboxed tool. It runs in one of two
modes, fixed by its declaration:

| | Live | Slice |
|---|---|---|
| Instance | started with the process, alive until it is stopped, paused or fails | created per trigger, destroyed after, as standing runs are today |
| Receiving work | `nine.next()` blocks until the next trigger and returns it | the trigger is the call's input |
| Calling the model | `nine.llm.turn(text)` blocks and returns the reply (§6) | not possible |
| State across triggers | its own memory, plus `state` and the database; memory is lost on a restart | `state` and the database |
| Output to a pipe | `nine.report(text)` (§7) | the call's non-empty result |
| Used for | shipped and Nine-written processes | today's standing tools and condition-trigger predicates |

Live mode is how a process is written: a program that waits for work and drives its session.
Slice mode keeps existing standing tools running unchanged; it has no model access, so nothing
in it waits on a model. A live tool is never callable as an ordinary tool (§9).

**A process session** is the session a process drives. Its history is the process's continuity
with the model; its journal is the process's activity log. A session has one **owning** process,
which sets its role; other processes may be **attached** to it (§4), and their turns run in it
under that role, one at a time.

| Trigger | Declared as | Delivers |
|---|---|---|
| Clock | `every = "5m"` xor `schedule = "0 7 * * *"` | the time |
| Event | `on = ["tool_end"]`, optionally filtered | the journal event (§7) |
| Message | always on | a message sent to the session by the operator, a conversation, the API, or a process piping into it (§7) |

```toml
[[process]]
name     = "digest"
tool     = "digest"                     # the sandboxed tool that drives it
schedule = "0 7 * * *"
role     = "writer"                     # the role of its llm turns
budget   = { turns_per_day = 20, tokens_per_day = 60000 }
args     = { feeds = ["https://feeds.bbci.co.uk/news/rss.xml"] }
```

```js
export default ({ args }, nine) => {
  for (;;) {
    const trigger = nine.next();                        // blocks until 07:00
    const stories = fetchAll(args.feeds);               // net.http
    const summary = nine.llm.turn(`Summarize in three sentences:\n${stories}`);
    writeFile("digest/today.md", summary);              // fs.write
  }
};
```

---

## 3. What each mechanism becomes

| Today | As a process session |
|---|---|
| Goal session | The shipped `pursue` process, bound to its goal (§4), every 5 min |
| Standing agent (`[[agent]]`) | `[[process]]` running `pursue` on a config-owned goal, with the declared role and trigger |
| Self-reflection session | The shipped `reflect` process, at `[daemon] self_reflection` |
| Condition trigger (`when = {…}`) | A pipe (§7): the predicate tool as a process, with `report_to` naming the standing agent's session |
| Standing tool | A slice process |
| Reaction | A process with an `on` trigger |
| Processes Nine writes | `tool_write` with a `process` block (§9) |

What stays as it is:

| | Why |
|---|---|
| Goals | A goal is data — an intention, a status, sub-goals. `pursue` acts on it; it is not a session |
| Go subscribers (related-session indexer, supervisor) | Internal machinery with no configuration and no user-visible identity |
| Conversations | A person is not a process |
| Sub-agents (`run_agent`) | A delegation within a turn, owned by that turn |

---

## 4. Shipped processes

Session-plan routines become shipped processes: live JS tools compiled into the binary, as shipped
tools are. They are core: Nine cannot rewrite them, and a process it writes cannot take their
names. Being sandboxed, they hold to their grants and role structurally, and they are working
examples Nine can read (`nine tools show pursue`) before writing its own.

| Process | Replaces | Does |
|---|---|---|
| `pursue` | the `pursue` routine | one `llm` turn under the session's role: assess the goal and its sub-goals, act, update the status |
| `reflect` | the `idle-reflection` routine | one `llm` turn under the `reflection` role |

**Goal binding.** A process session may be bound to a goal (`goal = "<id>"`). The binding carries
over the rules goal sessions have today: one session per top-level goal, the goal's status
decides whether the session runs (active runs, paused pauses, done or archived retires), and the
agent owns that status — re-declaring a finished standing agent does not resurrect it.

**Sharing a session.** A standing agent can run extra routines in its own session today
(`[[agent.routine]]`, e.g. reflection beside pursuit, with the agent's history and role). That
carries over as attachment: `reflect` attached to the agent's session, its turns running there
under the owner's role, serialized with the owner's. When several attached processes are due at
once, the one overdue longest runs first, as routines are chosen today.

**Stall.** Five consecutive `llm` turns that call no tool pause a goal-bound session's goal, as
today. It is a rule of goal binding, not of every process: a summarizer's turns call no tool by
design.

The prompts, roles and turn shapes of `pursue` and `reflect` are today's, moved, not rewritten.
Phase 1 is accepted on that (§14).

---

## 5. One set of limits

| Setting | Replaces | Meaning |
|---|---|---|
| `[processes] max_running` | `max_goal_sessions`, `max_standing` | process sessions active at once, default 14 (today's 10 and 4); at the cap a new one is recorded and not started, as goal sessions are today |
| `[processes] budget` | — | the default budget of every process; a declaration may lower it, never raise it |
| `[processes] max_depth` | — | the lineage limit (§7), default 2 |
| `[processes] priority` | — | `background`, the queue priority of every process turn |
| `[processes] memory_mb` | — | the memory cap of one live instance |

Live instances run in their own pool, sized by `max_running`, apart from the sandbox's call slots:
a process waiting in `nine.next()` or `nine.llm.turn()` holds its own instance and never a slot an
ordinary tool call needs.

A process that exhausts its budget, or fails its health checks (standing tools' backoff and
`failing` state, generalized), is paused, and the pause reaches the human feed.

---

## 6. The `llm` capability

**A blocking turn in the process's own session.** Only a live process can call
`nine.llm.turn(text)`; it returns the reply as a string, or throws when a bound below stops it.

| Bound | Applies to | Set by |
|---|---|---|
| Reply length, inner model calls, turn duration | one turn | the turn's own limits: `max_tokens`, the loop's call cap, `task_timeout_seconds` |
| Turns and tokens per day | one process | its budget, capped by `[processes] budget`; when exhausted, `nine.llm.turn` throws and the process is paused, reported to the human feed |
| Priority and concurrency | all processes | `[processes] priority`, `max_running` |

The instance has no deadline of its own: it runs until stopped. Every host call it makes carries
its own bound, so nothing it waits on can wait forever.

**Under its role.** The role decides everything that is not
the process's text: persona, tools, enrichment, thinking. It must be **operator-authored** — built
in, or from `[skills] user_dir`. Nine can write roles (R-ROLE.7 keeps their structural flags at
leaf defaults), but an agent-authored role's tool list is bounded only by the daemon's surface,
so letting a process name one would let Nine choose its process's reach.

**The default role is lean.** `process`: Nine's identity, no tools, no enrichment, no planning
pass.

**Reach is the closure, and the roster prints it.** `tool-facilities.md` §7 rejected tool→tool
dispatch because a tool's reach would become the closure of what it can call, invisible in its
grants. A process with `llm` has that property, so its summary prints both: `llm: role writer
(read_file, memory_query)`. A role that holds `tool_write` is refused for a process unless
`[processes] allow_revision = true` (§9).

---

## 7. Events, and the feedback loop

`reactive-events.md` §1a deferred model-calling reactions and named what revisiting them
requires. Each condition has its answer:

| Condition | Answer |
|---|---|
| A cheap programmatic gate before any model call | The process: it runs on every trigger and calls `llm` only when its code decides to |
| Lowest-priority scheduling | `[processes] priority` (§5) |
| Hard per-subscriber budgets | Every process has one (§5) |
| A lineage marker with loop detection | Below |

**Lineage.** An event a process session journals carries its triggering event as parent
(`parent_span_id`). An event's depth is the number of process sessions in its ancestry. No
process is invoked for an event at depth `max_depth` or more (default 2), and the skip is
journaled. A process is never invoked for an event from its own session. A message carries its
sender's depth plus one.

**Pipes.** A process may declare `report_to = "<session>"`: what it reports is delivered to that
process session as a message, which is a trigger. A live process reports with `nine.report(text)`;
a slice process reports each non-empty result. Empty output sends nothing.

| | A pipe |
|---|---|
| Timing | one output per run, delivered as a message; the receiver runs on that trigger |
| Direction | one way: the sender never sees what the receiver does with it |
| Reach | none shared: each side runs under its own grants and role |
| Length | bounded by `max_depth`, since a message carries its sender's depth plus one |
| Declared by | the operator for declared processes; Nine only between processes it wrote, within its instance |

A pipe is not the tool→tool dispatch `tool-facilities.md` §7 rejects: the sender gains none of the
receiver's reach and cannot read its result, so each process's roster entry still states what it
can reach. What a pipe carries is data, and data reaching a model-driven receiver is in its
prompt: a process that fetches web pages and pipes them on is an injection path into the
receiver's session. The receiver's turn therefore labels delivered text with its sender, as
`[From process <name>: …]`, so the model can tell an upstream report from a person's instruction.
The label arrives in phase 2: phase 1 delivers the text verbatim, as condition triggers do today,
so its snapshots stay unchanged.

**Delivery** keeps today's condition-trigger semantics: a message is delivered if the receiver is
idle, and otherwise goes to the human feed. Queued delivery — nothing lost, but a fast sender can
pile up messages for a slow receiver — is a later decision, with a cap or coalescing per sender.

**No waking.** A process session runs turns only in itself. It reaches a person through the human
feed, and a conversation through what it writes, which that conversation reads when its model
decides to.

**No questions.** A process has no `ask_human`: no person attends its session, and a blocking
question would hold a running slot. It posts a question to the human feed, and the operator
answers by messaging the session, which is a trigger.

---

## 8. The `sql` capability

One SQLite database per instance, in its own file beside the store, shared by the instance's
processes: Nine's structured memory, with a schema Nine designs and migrates.

| Rule | Why |
|---|---|
| A separate file, never the store | Nothing a process does reaches `kv`, `skills`, the journal or the grants |
| An authorizer policy | No `ATTACH`, no extension loading, a read-only `PRAGMA` allowlist; DDL allowed so the schema can evolve |
| Quotas | Database size and statement time, from `[processes]` |
| Writes journaled as summaries | Statement kind and table, not values |

Conversations read it through `sql_query`, read-only and role-granted.

---

## 9. Writing and revising processes

**Writing.** `tool_write` with a `process` block, gated by `[tools.agent] allow_processes` (off by
default; it replaces `allow_standing`). The write passes the existing checks — parse, declaration
against the ceiling — and the approval gate in an interactive session.

**Revising.** A process cannot write processes. Revision is a model turn: a process whose role
holds `tool_write` (only with `allow_revision`) asks its turn to rewrite one. Every revision is a
journaled turn under an operator-chosen role, subject to the ceiling and the gates.

**Using processes from a conversation.** A live process is not request and response, so it is
never in a conversation's callable tool list. A model works with processes through five tools,
granted per role like any other:

| Tool | Does |
|---|---|
| `process_list`, `process_show` | state, triggers, budget used, recent journal |
| `process_send(session, text)` | a message to a process session, which is a trigger; the process receives it from `nine.next()` |
| `process_pause`, `process_resume`, `process_stop` | control, for processes Nine wrote; shipped and declared ones are the operator's |

The exchange is asynchronous: the model sends, and the answer comes back through what the process
writes — files, the database, a pipe into a session the model reads. A turn never waits on a
process, since a process may run forever. The `process` role, under which a process's own turns
run, holds none of these tools, so a process cannot use its turns to steer processes.

**Versions and rollback.** Each write keeps the previous version. A new version that fails its
first `rollback_after` invocations (default 3) is restored to the previous one, and the restore
reaches the human feed and the session's history.

---

## 10. Genesis

`[bootstrap] genesis = "genesis.md"`, read once on the first boot of an empty store, after the
self-model bootstrap (`personality-pattern.md`). A `genesis` session, under a `genesis` role
holding `tool_write` and `sql_query`, turns the document into the instance's first processes and
database schema.

---

## 11. Configuration compatibility

`[[agent]]` and `[[standing_tool]]` keep working for one minor release, translated at load into
`[[process]]` blocks with a deprecation warning naming the replacement. `when = {…}` stays as
shorthand: it translates into a process running the predicate tool with `report_to` naming the
agent's session (§7), so existing condition triggers keep their behavior. `max_goal_sessions` and `max_standing` map to
`max_running` (their sum) for the same release.

---

## 12. What this amends

| | Today | With process sessions |
|---|---|---|
| R-SUB.7 | No generative-LLM reaction, no autonomous session injection | Process sessions call the model in their own session, under §6 and §7. Go subscribers are unchanged and R-SUB.3 still binds them |
| I11 | Reactions are out-of-band | Unchanged: a process session writes its own session, `state`, the database and the human feed, and never mutates another session |
| `docs/self-modification.md` | Nine's executable shape is fixed; only its knowledge grows | The core is fixed; the runtime is Nine's, inside the core's bounds |
| `adr/durable-and-long-running-tools.md` §2, §4 | No instance outlives its call; keeping one alive was rejected | Live processes keep their instance until stopped, in their own pool with a memory cap; slice tools keep the per-call model unchanged |
| Session plans and routines | The extension point for background work | Removed: routines become shipped processes, and a conversation's `active` plan becomes a property of the conversation. The `session_plans` table and `RoutineHandler` go in a migration |
| R-TVM.20 (standing tools) | A second run mode | A slice process |
| `adr/agent-boundary.md` | External mode planned | Paused: process sessions belong to the internal agent, their policy is a role |

---

## 13. What carries over from nine-will

| From nine-will | Here |
|---|---|
| Mind modules, residents, transients | Processes; a resident is a live process, a transient a slice call |
| The being (private SQLite, authorizer policy) | `sql`; `src/kernel/sql-policy.ts` ports to Go |
| The record | The journal |
| Genesis from a document | §10 |
| The reviser | A process with a revision role (§9) |
| Lessons: cut replies, one module per reply, four-backtick fences | The genesis and revision roles' prompts |
| Children, several instances per host, the web UI | Not here: one daemon is one instance |

---

## 14. Phases

| # | Content | Acceptance |
|---|---|---|
| 0 | Turn snapshots for `pursue` and `reflect`; journal snapshots for a standing tool and a condition trigger | Recorded on `main` before any change |
| 1 | The unification: process sessions as the one background mechanism; `pursue`, `reflect` shipped; pipes with today's delivery (`report_to`); goal binding; `[[process]]` with the aliases; `[processes]` limits; live mode with `nine.next()`, `nine.llm.turn()` and `nine.report()`, for shipped processes only; `pursue` and `reflect` as live JS tools; attached processes; session plans removed | Snapshots unchanged; live evals match the baseline (`goal-create`, `delegate-subagent`, `workflow-plan`, the standing cases) |
| 2 | Budgets and health for every process; the roster in the CLI and TUI; `process_list`, `process_show`, `process_send`; the sender label on piped messages (§7), re-recording the condition-trigger snapshot | An exhausted budget pauses a process and reaches the human feed |
| 3 | Processes Nine writes: `allow_processes`, `llm` under operator-authored roles, the `process` role | A Nine-written digest process runs on a schedule and summarizes through its session |
| 4 | Event triggers, lineage, `max_depth` | Two processes triggering each other stop at `max_depth`, the skip journaled |
| 5 | `sql`, its policy, `sql_query` | Denied statements are refused with a reason |
| 6 | Revision: `allow_revision`, versions, rollback | A failing revision rolls back after `rollback_after` failures |
| 7 | Genesis | An empty store and a genesis document boot into running processes |
| 8 | Docs and spec (R-PROC), the aliases' removal scheduled | Each phase's eval cases pass on a local model |

---

## 15. Decisions taken (2026-10-06)

| Question | Decision |
|---|---|
| The private SQL database's name | **The database**: `[processes] database`, read by `sql_query` |
| Session plans, once routines are processes | **Removed**: a conversation's `active` plan becomes a property of the conversation (§12) |
| Which sessions stall detection applies to | **Goal-bound sessions only** (§4); budgets bound every other process |
| `ask_human` from a process | **No**: questions go to the human feed, answers come back as messages (§7) |
| `max_depth` default | **2**: a process may react to another process's work, not to a reaction to it |
| `max_running` default | **14**: today's 10 goal sessions plus 4 standing tools |
| Shipped processes | **Live JS tools** in the sandbox, not Go: one kind of process, limits held structurally, readable examples for Nine |
| How processes run | **Live mode**: an instance alive until stopped, with blocking `nine.next()` and `nine.llm.turn()`. Slice mode stays for today's standing tools and predicates, without model access |
| Several processes on one session | **Allowed**: one owner sets the role; attached processes' turns run in the session under it, serialized (§4) |
| How a conversation uses processes | **Through process tools**, asynchronously (§9); a live tool is never callable as an ordinary tool |
| How a watcher reaches a thinker | **Pipes** (`report_to`, §7): one process's output becomes a message to another's session. Condition triggers become shorthand for a pipe; their delivery rules are kept |

---

## Limits

- Nothing here is built.
- One daemon is one instance: several instances, children and lineage are out of scope.
- Rollback (§9) catches a revision that fails, not one that runs and does worse.
- The aliases (§11) keep old configuration working for one minor release only.
