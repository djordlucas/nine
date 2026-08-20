# Scheduling — how idle-capable sessions decide when to wake

**Status:** shipped · **Used by:** [predefined-agents.md](predefined-agents.md) (standing agents), goal-sessions, self-reflection

A background session (a pursue goal session, the self-reflection session, or a
pre-defined standing agent) does work between human turns because its session
plan carries an **idle-capable aspect** with a wake **trigger**. The
`AgentWorker`'s idle timer fires when a aspect is due, calls the aspect's `OnIdle`,
and submits the returned text as the session's next turn
([session-plans.md](session-plans.md)).

There are two trigger kinds, set in a aspect's `Config` and **mutually
exclusive**:

| Trigger | Config field | Meaning |
|---|---|---|
| Fixed interval | `idle_interval_seconds` | Wake every N seconds since the last fire. |
| Cron | `schedule` | Wake at the next time matching a 5-field cron expression. |

## Cron expressions

`internal/cron` parses standard 5-field cron — `minute hour day-of-month month
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

`stageNextWake(cfg, lastFire, now)` (`internal/runtime/session_plan.go`) reduces
either trigger to a single "how long until due" duration:

- interval: `max(interval − (now − lastFire), 0)`
- cron: `max(Schedule.Next(lastFire) − now, 0)`

`armIdleTimer` arms the worker's timer for the **minimum** remaining across the
session's active idle-capable aspects; `handleIdle` fires every aspect that is due
(`remaining == 0`). `planNeedsResume` (via `stageScheduled`) treats a aspect with
either a positive interval or a valid cron as idle-capable, so the daemon revives
it on restart.

## Semantics and limits (v1)

- **No backfill.** `lastFire` resets to the worker's start time on daemon
  restart, so a cron occurrence missed while the daemon was down is not
  replayed — the session simply waits for the next occurrence.
- **Minute granularity.** Cron resolves to the minute; the timer may fire a few
  seconds late, which is fine for standing-agent cadences.
- **One trigger per aspect.** A standing agent declares `interval` **xor**
  `schedule` in `nine.toml`; setting both is a config error and the agent is
  skipped with a warning.
