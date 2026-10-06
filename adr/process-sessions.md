# Design note — Process sessions

**Status:** **Proposed** · **Related:** `adr/standing-tools.md`, `adr/tool-facilities.md`,
`adr/reactive-events.md`, `adr/durable-and-long-running-tools.md`, `adr/roles-design.md`,
`adr/personality-pattern.md`, `adr/agent-boundary.md` · **Amends:** R-SUB.7, and the stance of
`docs/self-modification.md` — see §8

Nine gains **process sessions**: sessions driven by a **process** — a sandboxed tool Nine wrote
for itself — instead of by a person. Events, a cadence or a message invoke the process; the
process can call the model **in its own session**, under a role the operator configures, and can
keep relational state in a private **SQL database**. The session is still Nine's: its identity,
skills and memory, its journal, its roles. What changes is who decides when a turn happens and
what it asks.

The design follows one principle: **a rigid core and a flexible runtime.** The core — the binary,
the configuration, the operator's grants and budgets, the roles — does not change at runtime and
is never written by Nine. The runtime — the processes, their declarations and source, the
database's schema and rows — is Nine's to write, revise and replace, inside the bounds the core
sets.

This note comes out of nine-will, an experiment that ran "instances made of code they write"
outside Nine, with Nine as its model runtime. The experiment worked, and the most capable parts
of it were rebuilds of what Nine already has. It moves here; §9 records what carries over.

---

## 1. Problem

Nine already writes code for itself: generated tools (`tool_write`), with durable `state`,
resumable jobs and standing runs on a cadence. Every one of them is **driven by the model**: a
turn calls a tool, the tool returns, the turn goes on. Code never drives the model, so three
shapes of work have no home:

| Work | Today | Why it fails |
|---|---|---|
| "Every morning, read these feeds and write a digest" | A standing agent on a schedule | A full agent turn decides, every time, what a fixed program could: which feeds, which format, where to write. The judgment is one summary; the rest is a program run by a model |
| "When a tool fails twice in a session, look into why" | Nothing | Reactions may not call a model (R-SUB.3), and may not wake anything (`tool-facilities.md` §6) |
| "Keep a structured record of what I learned, and query it" | KV memory, skills | No relations, no queries, no schema Nine can evolve |

The common shape: a program that knows **when** and **what**, and needs the model only for the
part that is language or judgment. Writing that program is exactly what Nine is good at, and
running it is what the sandbox already does safely.

---

## 2. Decision

| Part | What it is |
|---|---|
| **Process** | A generated tool with a `process` declaration: its triggers, its role, its budget. Source, schema and capabilities as for any generated tool |
| **Process session** | One session per process, created when the process is, owned by it. Its history is the process's continuity with the model |
| **Triggers** | `every` / `schedule` (as standing runs), `on` (journal event types, as reactions were designed), and messages sent to the session |
| **`llm` capability** | A host function: run a turn in the process session, under the declared role, and return the reply |
| **`sql` capability** | A private SQLite database for the instance, shared by its processes, behind an authorizer policy |

A trigger invokes the process exactly as a standing run invokes a tool: a fresh instance, a
deadline, the declared grants, nothing surviving the call except what it writes to `state`, the
database or its session. A long piece of work is a resumable call, as a job is.

```json
{
  "name": "digest",
  "description": "Write the morning news digest.",
  "capabilities": {
    "net.http": { "hosts": ["feeds.bbci.co.uk", "feeds.npr.org"] },
    "fs.write": ["digest/"],
    "llm": { "role": "writer" },
    "sql": {}
  },
  "process": {
    "schedule": "0 7 * * *",
    "budget": { "turns_per_day": 20, "tokens_per_day": 60000 }
  },
  "source": "export default async ({ trigger }, nine) => { … }"
}
```

The process asks the model through its session:

```js
const summary = await nine.llm.turn(`Summarize these stories in three sentences:\n${text}`);
```

---

## 3. The `llm` capability

**A turn in the process's own session, under a declared role.** The role decides everything
that is not the process's text: the persona, the tools the model may call during that turn, the
enrichment, the thinking policy. The role must be **operator-authored** — built in, or from
`[skills] user_dir`. Nine can write roles (R-ROLE.7 keeps their structural flags at leaf
defaults), but an agent-authored role's tool list is bounded only by the daemon's surface, so
letting a process name one would let Nine choose its own process's reach. The declaration is
checked against the ceiling like any other grant.

**The default role is lean.** `process` — Nine's identity, no tools, no enrichment, no planning
pass: a turn that costs its prompt and the session's history. A process that needs the model to
act (read a file, search memory) names a role that grants those tools.

**Reach is the closure, and the roster prints it.** `tool-facilities.md` §7 rejected tool→tool
dispatch because a tool's effective reach would become the closure of what it can call, invisible
in its grants. `llm` has the same property: a process's reach is its own grants plus its role's
tools. So `Summary()` prints both — `llm: role writer (read_file, memory_query)` — and a role
that includes `tool_write` or any `llm`-holding tool is refused as an `llm` role unless the
operator sets `[processes] allow_revision = true` (§6).

**Priority and budget.** Process turns queue at `PriorityBackground`, below every conversation a
person is waiting on. Each process has a budget in its declaration, capped by the operator's
`[processes]` limits: turns per day, tokens per day, turns per invocation. A process that
exhausts its budget is paused, and the pause reaches the human feed.

---

## 4. Triggers, and the feedback loop

`reactive-events.md` §1a deferred generative reactions and named what revisiting them requires:
a cheap programmatic gate before any model call, lowest-priority scheduling, hard per-subscriber
budgets, and a lineage marker with loop detection. Process sessions are that revisit, and each
condition has its answer:

| Condition | Answer |
|---|---|
| Programmatic gate | The process is the gate: it runs on every trigger and calls the model only when its own code decides to |
| Lowest priority | `PriorityBackground` (§3) |
| Hard budgets | Per process, capped by `[processes]` (§3) |
| Lineage and loop detection | Below |

**Lineage.** Every event a process session journals carries the event that triggered it as its
parent (`parent_span_id`, which exists for this). An event's **depth** is the number of process
sessions in its ancestry. A process is not invoked for an event at depth `max_depth` or more
(default 2); the skip is journaled. A process is never invoked for an event from its own session.

**Messages.** A message to a process session (from another session, the operator, or the API)
invokes it with the message as input. A message carries its sender's depth plus one, so two
processes messaging each other stop at `max_depth` like any other chain.

**Waking.** A process session does not wake other sessions. Its findings reach a person through
the human feed, and reach Nine's conversations by pull: they are in the database and the
journal, and a conversation reads them when its model decides to.

---

## 5. The `sql` capability

One SQLite database per instance, in its own file beside the store, shared by every process the
instance runs. It is Nine's structured memory: a schema Nine designs and migrates, rows its
processes write, queries they run.

| Rule | Why |
|---|---|
| A separate file, never the store | Nothing a process does can reach `kv`, `skills`, the journal or the grants |
| An authorizer policy | No `ATTACH`, no `PRAGMA` beyond a read-only allowlist, no extension loading; DDL allowed, so the schema can evolve |
| Quotas | Database size and statement time, per the operator's `[processes]` limits |
| Every write journaled as a summary | Statement kind and table, not the values — the journal stays readable and bounded |

A conversation reads the database through one tool, `sql_query`, read-only and role-granted, so
the operator decides which roles can look.

---

## 6. Writing and revising processes

**Writing.** `tool_write` with a `process` block, gated by `[tools.agent] allow_processes`, off by
default — for the reason `allow_standing` exists: a trigger is not reach, and the ceiling cannot
express it. A process's first version passes the existing write-time checks (parse, declaration
against the ceiling), and the approval gate prompts for it in an interactive session.

**Revising.** A process cannot write processes. Revision goes through the model: a process whose
`llm` role includes `tool_write` (allowed only with `[processes] allow_revision`) asks its turn to
rewrite a process. So every revision is a model turn in a journaled session, under a role the
operator chose, subject to the same ceiling and gates as any other write.

**Versions and rollback.** Each write keeps the previous version. A process whose new version
fails its first `rollback_after` invocations (default 3) is restored to the previous version, and
the restore reaches the human feed and the process session's history. nine-will's revisions
showed why: an unverified rewrite regressed a working program, and nothing noticed.

---

## 7. Genesis

A packaged instance (`personality-pattern.md`) already bootstraps its identity from a
`self-model.toml`. Genesis adds the rest: `[bootstrap] genesis = "genesis.md"`, a document read
once, on the first boot of an empty store. A `genesis` session, under a `genesis` role with
`tool_write` and `sql_query`, turns the document into the instance's first processes and database
schema. Its journal is the record of how the instance was born.

---

## 8. What this amends

| | Today | With process sessions |
|---|---|---|
| R-SUB.7 | No generative-LLM reaction, no autonomous session injection | Process sessions call the model in their own session, under §3 and §4. Subscribers and reactions are unchanged: R-SUB.3 still binds them |
| I11 | Reactions are out-of-band | Unchanged: a process session writes only its own session, its `state`, the database and the human feed; it never mutates another session |
| `docs/self-modification.md` | Nine's executable shape is fixed; only its knowledge grows | The core is fixed; the runtime (processes and the database) is Nine's, inside the core's bounds |
| Replay | Generative calls stay out of reactions | A process turn is an ordinary journaled turn, so Track R replays it from its recording |

In `adr/agent-boundary.md` terms, process sessions belong to the internal agent: their policy is
a role. The external mode loses its motivating client and is paused (§10).

---

## 9. What carries over from nine-will

| From nine-will | Here |
|---|---|
| Mind modules, residents, transients | Processes; a resident is a process with triggers, a transient a resumable call |
| The being (private SQLite, authorizer policy) | The `sql` capability; the policy (`src/kernel/sql-policy.ts`) ports to Go |
| The record | The journal |
| Genesis from a document | §7 |
| The reviser | A process with a revision role (§6) |
| Lessons: cut replies, one module per reply, four-backtick fences | The genesis and revision roles' prompts |
| Children, lineage, several instances on one host, the web UI | Not here: one daemon is one instance. A later note, if wanted |

---

## 10. Phases

| # | Content | Acceptance |
|---|---|---|
| 1 | Process sessions with `every`/`schedule` and messages; `llm` with roles, priority, budgets; the `process` role | A digest process runs on a schedule and summarizes through its session; an exhausted budget pauses it |
| 2 | Event triggers with lineage and `max_depth` | Two processes triggering each other stop at `max_depth`, with the skip journaled |
| 3 | `sql` capability, the authorizer policy, `sql_query` | Denied statements (ATTACH, a store table) are refused with a reason |
| 4 | Revision: `allow_revision`, versions, rollback | A failing revision is rolled back after `rollback_after` failures |
| 5 | Genesis | An empty store and a genesis document boot into running processes |
| 6 | Docs, spec (R-PROC), evals for each phase, the TUI and CLI process views | `make eval-live` cases for each phase pass on a local model |

Before phase 1: merge the agent-boundary refactor (phases 1a and 1b), which this builds on, and
pause its phases 2–3 (external mode).

---

## 11. Open questions

| Question | Leaning |
|---|---|
| The database's name | Open: "the database" in this note |
| May a process call `ask_human`? | No: a process session has no person attending it; questions go to the human feed |
| Should the operator's conversation see process sessions in the TUI? | Yes, read-only, like sub-agents |
| `max_depth` default | 2: a process may react to another process's work, not to a reaction to it |
| Several instances, children | Out of scope; one daemon per instance |

---

## Limits

- Nothing here is built.
- One daemon is one instance: several instances, children and lineage are out of scope (§9).
- `sql` gives no cross-instance sharing; two instances share nothing but what an operator wires.
- Rollback (§6) catches a revision that fails, not one that runs and does worse; judging
  "worse" is left to the revising process.
