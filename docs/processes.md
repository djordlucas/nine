# Processes

Nine has two kinds of session. A **conversation** is driven by a person. A
**process session** is driven by a **process**: a sandboxed tool that runs on
its triggers — a clock, or a report piped to it — and can ask the model for a
turn in its own session. Everything Nine does between your turns is a process:
goal pursuit, standing agents, self-reflection, condition triggers, and standing
tools.

| | Driven by | Started by |
|---|---|---|
| Conversation | a person, through the TUI or the API | a message |
| Process session | a process | its triggers |

## Live and slice

A process runs in one of two modes, decided by its tool:

| | Live | Slice |
|---|---|---|
| Instance | started once, runs until stopped | created for each trigger, destroyed after |
| Receiving work | `next()` from `nine:process` waits for the next trigger | the trigger is the call |
| Asking the model | `turn(text)` runs a turn in its session and returns the reply | not possible |
| Output | `report(text)` | the call's non-empty result |
| Examples | `pursue`, `reflect`, a tool whose manifest says `live = true` | any resumable tool on a cadence — what standing tools were |

A live process's instance has no call deadline. Its work budget bounds the work
done for each trigger and refills when `next()` returns one, and every call it
makes carries its own bound: an HTTP request its timeout, a model turn the
turn's limits. Live instances run in their own pool, sized by
`[processes] max_running`, so a process waiting for a trigger never takes a slot
an ordinary tool call needs.

```js
import { next, turn, report } from "nine:process";

export default () => {
  for (;;) {
    const trigger = next();                       // a clock tick or a piped report
    report(turn(`Summarize: ${trigger.text ?? ""}`));
  }
};
```

A live tool is never in a conversation's tool list, and calling one is refused:
Nine starts it, it is not called.

## Shipped processes

| Process | Runs | Each tick |
|---|---|---|
| `pursue` | every [goal session](goal-sessions.md) and [standing agent](predefined-agents.md) | asks the session to assess and act on its goal; a piped report is put to the session as it is |
| `reflect` | self-reflection, or beside a standing agent | asks the session to reflect and update its self-model |

They are read-only programs you can inspect with `nine tools show pursue`.

## Triggers

| Trigger | Declared as | Arrives as |
|---|---|---|
| Clock | `every = "5m"` or `schedule = "0 7 * * *"` | `{ kind: "clock" }`, with the bound goal for a goal process |
| Report | another process's `report_to` naming this one | `{ kind: "message", text, from }` |

A process's first tick comes one cadence after it starts, at boot as when it is
created. A process with no clock wakes only on reports.

A process that fails — a slice call that errors, a live program that throws —
is retried after a backoff that doubles its cadence each time, up to 30
minutes. Three failures in a row make it `failing`, which reaches
`nine notifications`, as does its recovery.

## Budgets

Every process has a budget over a rolling day: the model turns its program runs
through `turn()`, and the tokens those turns spend. The day starts at its first
counted turn.

| Setting | Default | Meaning |
|---|---|---|
| `[processes] budget` | `{ turns_per_day = 200, tokens_per_day = 2000000 }` | every process's budget |
| `budget` in a `[[process]]` block | `[processes] budget` | lowers either field for that process, never raises it |

A turn the budget no longer covers is refused: `turn()` throws `E_BUDGET`, the
process is paused, and the pause reaches `nine notifications` with when it runs
again. When its day is over the process runs again by itself, with its usage
back at zero. The default is generous enough that pursuing a goal does not meet
it; what it stops is a loop that runs away.

## Goals, sessions and pipes

**Goal binding.** A process bound to a goal works on that goal, and the goal's
status decides whether it runs: paused stops the process, active again restarts
it, done or archived leaves it stopped. A goal-bound session whose turns call no
tool five times in a row pauses its goal.

**Sharing a session.** A process may attach to another's session instead of
owning one: its turns run there, with that session's history and under its role.
That is how reflection runs beside a standing agent.

**Pipes.** A process that declares `report_to` delivers its reports to that
process's session, as a trigger. The receiver takes a report only while it is
waiting for work; a report that finds it busy goes to the human feed instead, so
a finding is never silently dropped. A condition trigger is a pipe from a cheap
predicate to an agent.

A piped report arrives labelled with its sender, as
`[From process cve-scan: found: CVE-2026-1234 in libfoo]`. What a pipe carries
is data that lands in the receiver's prompt, so a process that fetches web pages
and pipes them on could otherwise pass off a page's text as an instruction.

## From a conversation

A live process is never a tool a conversation calls. A conversation works with
processes through five tools, which its role grants like any other; a process
session and a sub-agent hold none of them.

| Tool | Does |
|---|---|
| `process_list`, `process_show` | state, who stopped it, trigger, session, goal, budget use and reset, last error, recent activity |
| `process_send` | gives a running live process a message as its next trigger, labelled `[From conversation <id>: …]`; refused when it is stopped, busy, or a slice process |
| `process_start`, `process_stop` | control, under the rule below |

A send does not wait for an answer: the process answers through what it writes.

A start grants nothing the process did not have, so a model may start any
stopped process, except one stopped by:

| Stopped by | A model's `process_start` |
|---|---|
| The operator | refused: only the operator can start it |
| Its goal | refused: reactivate the goal instead |
| Its budget | refused until its day is over, when it runs again by itself |
| A model, itself, or failing | allowed |

Every start counts against `[processes] max_running`. Each refusal names its
reason.

## Declaring processes

```toml
# A standing agent: pursue, bound to a goal this file owns.
[[process]]
name      = "sec-watch"
tool      = "pursue"
goal      = "Monitor this repo for security issues; triage new CVEs."
role      = "monitor"           # default "monitor", read-only
delegates = false
schedule  = "0 9 * * 1-5"       # or: every = "24h"; neither is every 5 minutes
budget    = { turns_per_day = 50 }   # lowers [processes] budget

# A condition trigger: a predicate every ten seconds, piped to the agent.
[[process]]
name      = "cve-scan"
tool      = "cve_scan"
every     = "10s"
args      = { manifest = "/srv/app/go.sum" }
report_to = "sec-watch"

# Reflection in the agent's own session.
[[process]]
name    = "sec-watch-reflect"
tool    = "reflect"
every   = "30m"
session = "sec-watch"

# A standing tool: called on a schedule, reporting to the human feed.
[[process]]
name     = "tidy"
tool     = "tidy_logs"
schedule = "0 3 * * *"
enabled  = true                 # false declares it without starting it

[processes]
max_running   = 14              # processes running at once, live and slice
budget        = { turns_per_day = 200, tokens_per_day = 2000000 }
authoritative = false           # true: a goal process no longer listed is retired at boot
```

The file owns each process's definition; the runtime owns whether it is running.
Editing a block adjusts what a process does without restarting one that was
stopped, and a goal the agent finished is never resurrected. Every mistake in a
block — an unknown `session`, a `report_to` naming no process, both `every` and
`schedule` — fails the load with its reason.

### From the blocks `[[process]]` replaced

| Before | Now |
|---|---|
| `[[agent]] id, description` | `[[process]] name, tool = "pursue", goal` |
| `[[agent.routine]] kind = "idle-reflection"` | `[[process]] tool = "reflect", session = "<agent>"` |
| `when = { tool, interval, args }` | `[[process]] tool, every, args, report_to = "<agent>"` |
| `[[standing_tool]] id, interval` | `[[process]] name, every` |
| `[daemon] standing_agents_authoritative` | `[processes] authoritative` |
| `[daemon] max_goal_sessions`, `[tools.agent] max_standing` | `[processes] max_running` |

A file that still has one of the old blocks fails to load, naming the form to
use instead.

## Related

- [Goal sessions](goal-sessions.md) — the goals `pursue` works on
- [Pre-defined agents](predefined-agents.md) — standing agents declared in `nine.toml`
- [Scheduling](scheduling.md) — cron expressions
- [Sandboxed tools](sandboxed-tools.md) — the sandbox every process runs in

> The design and its phases — [../adr/process-sessions.md](../adr/process-sessions.md).

## Limits

| Limit | Detail |
|-------|--------|
| No backfill | A clock tick missed while the daemon or the process was down is not replayed: the first tick comes one cadence after the start. |
| Only shipped and operator processes | Nine cannot write processes yet: a live process is shipped, or a developer tool an operator declares with `[[process]]`, whose turns run under the `role` its block names. |
| No event triggers | Processes wake on clocks and reports, not on journal events. |
| Configuration is read at boot | Editing a block takes effect at the next start. |
