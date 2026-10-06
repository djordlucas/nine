# Design note — Process sessions

**Status:** **Proposed** (revised 2026-10-06: one concept for all background work) ·
**Related:** `adr/standing-tools.md`, `adr/tool-facilities.md`, `adr/reactive-events.md`,
`adr/predefined-agents-design.md`, `adr/roles-design.md`, `adr/personality-pattern.md`,
`adr/agent-boundary.md` · **Amends:** R-SUB.7, the stance of `docs/self-modification.md`, and
the configuration of standing agents and standing tools — see §12

Nine has **two kinds of session**. A **conversation** is driven by a person. A **process
session** is driven by a **process**: a sandboxed tool that its triggers — a clock, a journal
event, a message — invoke, and that can call the model in its own session. Everything Nine does
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
budget. Source, schema and capabilities are those of any sandboxed tool.

**A process session** is the one session a process owns, created with it. Its history is the
process's continuity with the model; its journal is the process's activity log, for every
invocation, whether or not it called the model.

**A trigger** invokes the process as a standing run invokes a tool today: a fresh instance, a
deadline, the declared grants, nothing surviving the call except what it writes to its `state`,
the database (§8) or its session. Long work is a resumable call.

| Trigger | Declared as | Delivers |
|---|---|---|
| Clock | `every = "5m"` xor `schedule = "0 7 * * *"` | the time |
| Event | `on = ["tool_end"]`, optionally filtered | the journal event (§7) |
| Message | always on | a message sent to the session by the operator, a conversation or the API |

**The `llm` capability** runs a turn in the process session under the declared role and returns
the reply (§6). A process without it never calls the model.

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
export default async ({ trigger, args }, nine) => {
  const stories = await fetchAll(args.feeds);           // net.http
  const summary = await nine.llm.turn(`Summarize in three sentences:\n${stories}`);
  await writeFile("digest/today.md", summary);          // fs.write
};
```

---

## 3. What each mechanism becomes

| Today | As a process session |
|---|---|
| Goal session | The shipped `pursue` process, bound to its goal (§4), every 5 min |
| Standing agent (`[[agent]]`) | `[[process]]` running `pursue` on a config-owned goal, with the declared role and trigger |
| Self-reflection session | The shipped `reflect` process, at `[daemon] self_reflection` |
| Condition trigger (`when = {…}`) | A process that runs the check and calls `llm` only when it finds something; the wake becomes a turn in its own session |
| Standing tool | A process without `llm` |
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

Session-plan routines become shipped processes, compiled into the binary like shipped tools.
They are core: Nine cannot rewrite them, and a process it writes cannot take their names.

| Process | Replaces | Does |
|---|---|---|
| `pursue` | the `pursue` routine | one `llm` turn under the session's role: assess the goal and its sub-goals, act, update the status |
| `reflect` | the `idle-reflection` routine | one `llm` turn under the `reflection` role |

**Goal binding.** A process session may be bound to a goal (`goal = "<id>"`). The binding carries
over the rules goal sessions have today: one session per top-level goal, the goal's status
decides whether the session runs (active runs, paused pauses, done or archived retires), and the
agent owns that status — re-declaring a finished standing agent does not resurrect it.

**Stall.** Five consecutive `llm` turns that call no tool pause a goal-bound session's goal, as
today. It is a rule of goal binding, not of every process: a summarizer's turns call no tool by
design.

The prompts, roles and turn shapes of `pursue` and `reflect` are today's, moved, not rewritten.
Phase 1 is accepted on that (§14).

---

## 5. One set of limits

| Setting | Replaces | Meaning |
|---|---|---|
| `[processes] max_running` | `max_goal_sessions`, `max_standing` | process sessions active at once; at the cap a new one is recorded and not started, as goal sessions are today |
| `[processes] budget` | — | the default budget of every process; a declaration may lower it, never raise it |
| `[processes] max_depth` | — | the lineage limit (§7) |
| `[processes] priority` | — | `background`, the queue priority of every process turn |

A process that exhausts its budget, or fails its health checks (standing tools' backoff and
`failing` state, generalized), is paused, and the pause reaches the human feed.

---

## 6. The `llm` capability

**A turn in the process's own session, under its role.** The role decides everything that is not
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

**No waking.** A process session runs turns only in itself. It reaches a person through the human
feed, and a conversation through what it writes, which that conversation reads when its model
decides to.

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
`[[process]]` blocks with a deprecation warning naming the replacement. `when = {…}` translates
into a shipped `watch` process (run the tool; call `pursue`'s turn when it reports a finding),
so existing condition triggers keep their behavior. `max_goal_sessions` and `max_standing` map to
`max_running` (their sum) for the same release.

---

## 12. What this amends

| | Today | With process sessions |
|---|---|---|
| R-SUB.7 | No generative-LLM reaction, no autonomous session injection | Process sessions call the model in their own session, under §6 and §7. Go subscribers are unchanged and R-SUB.3 still binds them |
| I11 | Reactions are out-of-band | Unchanged: a process session writes its own session, `state`, the database and the human feed, and never mutates another session |
| `docs/self-modification.md` | Nine's executable shape is fixed; only its knowledge grows | The core is fixed; the runtime is Nine's, inside the core's bounds |
| Session plans and routines | The extension point for background work | Replaced by shipped processes; conversations keep their `active` plan |
| R-TVM.20 (standing tools) | A second run mode | A process without `llm` |
| `adr/agent-boundary.md` | External mode planned | Paused: process sessions belong to the internal agent, their policy is a role |

---

## 13. What carries over from nine-will

| From nine-will | Here |
|---|---|
| Mind modules, residents, transients | Processes; a resident is a process with triggers, a transient a resumable call |
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
| 1 | The unification: process sessions as the one background mechanism; `pursue`, `reflect`, `watch` shipped; goal binding; `[[process]]` with the aliases; `[processes]` limits; `llm` for shipped processes only | Snapshots unchanged; live evals match the baseline (`goal-create`, `delegate-subagent`, `workflow-plan`, the standing cases) |
| 2 | Budgets and health for every process; the roster in the CLI and TUI | An exhausted budget pauses a process and reaches the human feed |
| 3 | Processes Nine writes: `allow_processes`, `llm` under operator-authored roles, the `process` role | A Nine-written digest process runs on a schedule and summarizes through its session |
| 4 | Event triggers, lineage, `max_depth` | Two processes triggering each other stop at `max_depth`, the skip journaled |
| 5 | `sql`, its policy, `sql_query` | Denied statements are refused with a reason |
| 6 | Revision: `allow_revision`, versions, rollback | A failing revision rolls back after `rollback_after` failures |
| 7 | Genesis | An empty store and a genesis document boot into running processes |
| 8 | Docs and spec (R-PROC), the aliases' removal scheduled | Each phase's eval cases pass on a local model |

---

## 15. Open questions

| Question | Leaning |
|---|---|
| The database's name | Open: "the database" here |
| Does a conversation's plan (`active`) stay a session plan, or become a property of the conversation? | Becomes a property; session plans then have no remaining use |
| May a process call `ask_human`? | No: no person attends a process session; questions go to the human feed |
| `max_depth` default | 2 |
| `max_running` default | 14: today's 10 goal sessions and 4 standing tools |

---

## Limits

- Nothing here is built.
- One daemon is one instance: several instances, children and lineage are out of scope.
- Rollback (§9) catches a revision that fails, not one that runs and does worse.
- The aliases (§11) keep old configuration working for one minor release only.
