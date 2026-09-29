# Scheduling

Used by standing agents ([predefined-agents.md](predefined-agents.md)), goal
sessions, and self-reflection.

A background session (a pursue goal session, the self-reflection session, or a
pre-defined standing agent) does work between human turns because its session
plan carries an **idle-capable routine** with a wake **trigger**. The
`AgentWorker`'s idle timer fires when a routine is due, calls the routine's `OnIdle`,
and submits the returned text as the session's next turn
([session-plans.md](session-plans.md)).

There are two *clock* trigger kinds, set in a routine's `Config` and **mutually
exclusive**:

| Trigger | Config field | Meaning |
|---|---|---|
| Fixed interval | `idle_interval_seconds` | Wake every N seconds since the last fire. |
| Cron | `schedule` | Wake at the next time matching a 5-field cron expression. |

A standing agent may also carry a **condition** trigger, which is not a clock —
see [Condition triggers](#condition-triggers).

## Cron expressions

`cron` parses standard 5-field cron — `minute hour day-of-month month
day-of-week` — with no external dependency. Each field supports `*`, single
values, ranges (`a-b`), lists (`a,b`), and steps (`*/n`, `a-b/n`). Day-of-week is
`0-6` with Sunday `0` (`7` is also accepted for Sunday). Names (`JAN`, `MON`) are
not supported.

Day-of-month / day-of-week follow the common Vixie-cron rule: when **both** are
restricted (neither is `*`) a day matches if **either** field matches; when one
is `*`, the other is ANDed normally.

Examples:

```
0 9 * * 1-5     09:00 every weekday
*/15 * * * *    every 15 minutes
0 0 1 * *       midnight on the 1st of each month
0 0 15 * 1      midnight on the 15th, or on any Monday
```

`Schedule.Next(after)` returns the earliest match strictly after `after`, in
`after`'s location, truncated to the minute. Times are evaluated in the daemon's
local timezone.

## How the scheduler uses a trigger

`stageNextWake(cfg, lastFire, now)` reduces
either trigger to a single "how long until due" duration:

- interval: `max(interval − (now − lastFire), 0)`
- cron: `max(Schedule.Next(lastFire) − now, 0)`

`armIdleTimer` arms the worker's timer for the **minimum** remaining across the
session's active idle-capable routines; `handleIdle` fires every routine that is due
(`remaining == 0`). `planNeedsResume` (via `stageScheduled`) treats a routine with
either a positive interval or a valid cron as idle-capable, so the daemon revives
it on restart.

## Condition triggers

A clock is the wrong shape for "tell me when X happens". At a useful polling rate
most wakes find nothing, and each one costs a full LLM turn to be told so — a
ten-second check is thousands of turns a day to hear that nothing changed.

A **condition trigger** puts a cheap deterministic predicate in front of the
expensive tier. It is a sandboxed tool, evaluated on its own cadence with no
model in the loop; the agent's turn happens only when the predicate returns
something, and that something becomes the turn's input.

```toml
[[agent]]
id          = "sec-watch"
description = "Triage new CVEs affecting our dependencies."
when        = { tool = "cve_scan", interval = "10s", args = { manifest = "/srv/app/go.sum" } }
```

The predicate follows the standing-tool convention exactly: **return nothing when
there is nothing to report**, and a non-empty result is the finding.

Properties:

- **It composes with a clock.** An agent may have `interval`/`schedule` *and* a
  `when`, giving it a periodic sweep plus something that wakes it sooner.
- **It is a standing tool underneath** (`nine tools standing` lists it as
  `when:<agent-id>`), so it backs off when it breaks, shows up as `failing`, and
  can be stopped with `nine tool stop when:<agent-id>` like any other.
- **A wake is lossy on purpose.** If the agent is mid-turn, or a wake is already
  queued, the new one drops. A predicate firing twice while the agent is still
  reading the first finding wants the agent to *look*, not to run two turns.
- **A finding that cannot be delivered goes to the human feed** rather than
  vanishing — a silently-dropped condition is the failure nobody would discover.

**On why a tool may wake an agent here at all.** A standing tool normally cannot:
it leaves a note and a human decides
([`toolvm.md`](../spec/contracts/toolvm.md) R-TVM.20). What a condition trigger
adds is an operator writing that link in their own configuration — the human is
in the loop when the connection is made, rather than each time it fires.

## Limits

| Limit | Detail |
|-------|--------|
| No backfill | `lastFire` resets to the worker's start time on daemon restart, so a cron occurrence missed while the daemon was down is not replayed. The session waits for the next occurrence. |
| Minute granularity | Cron resolves to the minute and the timer may fire a few seconds late. Adequate for standing-agent cadences, not for anything finer. |
| One clock trigger per routine | A standing agent declares `interval` **xor** `schedule`. Setting both is a config error and the agent is skipped with a warning. |
| No cron names | `JAN`, `MON` and friends are not parsed. Numeric fields only. |
| Local timezone only | Schedules evaluate in the daemon's local timezone. There is no per-agent timezone. |
| Wakes are lossy by design | If the agent is mid-turn or a wake is already queued, a new one drops. A predicate firing twice while the agent reads the first finding should make it look, not run two turns. |
| Conditions need the sandboxed-tool tier | A condition trigger is a standing tool, so it needs the sandboxed-tool host — on unless you set `[tools] enabled = false`. |
