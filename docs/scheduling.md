# Scheduling

Used by every [process](processes.md) with a clock: goal sessions, standing
agents, self-reflection, condition predicates and standing tools.

A process's clock is one of two **mutually exclusive** triggers, set on its
`[[process]]` block:

| Trigger | Field | Meaning |
|---|---|---|
| Fixed interval | `every` | Tick every N since the last tick (a Go duration: `10s`, `5m`, `24h`). |
| Cron | `schedule` | Tick at the next time matching a 5-field cron expression. |

A process with neither has no clock and wakes only on reports piped to it.

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

## How a tick is scheduled

The process runner keeps each process's next tick in the store and checks for
due ones on every pass. A live process gets its first tick one cadence after it
starts — at boot as when it is created — and each tick schedules the next: for
`every`, one interval later; for `schedule`, the next cron match. A tick that
comes due while the process is still busy with the last one waits, and waits
once: a process that fell behind runs once, not once per missed tick.

A slice process — what standing tools were — is called when its tick is due, and
a cycle it does not finish in one call continues at the delay it asked for.

## Condition triggers

A clock is the wrong shape for "tell me when X happens". At a useful polling rate
most wakes find nothing, and each one costs a full LLM turn to be told so — a
ten-second check is thousands of turns a day to hear that nothing changed.

A **condition** puts a cheap deterministic predicate in front of the expensive
tier: a slice process on a fast clock, with no model in the loop, piped to the
agent with `report_to`. The agent's turn happens only when the predicate returns
something, and that something becomes the turn's input.

```toml
[[process]]
name      = "cve-scan"
tool      = "cve_scan"
every     = "10s"
args      = { manifest = "/srv/app/go.sum" }
report_to = "sec-watch"
```

The predicate follows the standing-tool convention exactly: **return nothing when
there is nothing to report**, and a non-empty result is the finding.

Properties:

- **It composes with a clock.** The agent keeps its own `every`/`schedule`,
  giving it a periodic sweep plus something that wakes it sooner.
- **It is a process like any other**, so it backs off when it breaks, shows up as
  `failing`, and can be stopped with `nine tool stop cve-scan`.
- **A delivery is lossy on purpose.** If the agent is mid-turn, the finding does
  not queue a second turn: it goes to the human feed. A predicate firing twice
  while the agent is still reading the first finding wants the agent to *look*,
  not to run two turns.
- **A finding that cannot be delivered goes to the human feed** rather than
  vanishing — a silently-dropped condition is the failure nobody would discover.

**On why a tool may wake an agent here at all.** A tool normally cannot: it
leaves a note and a human decides. What a pipe adds is an operator writing that
link in their own configuration — the human is in the loop when the connection
is made, rather than each time it fires.

## Limits

| Limit | Detail |
|-------|--------|
| No backfill | A process's first tick comes one cadence after it starts, so a cron occurrence missed while the daemon was down is not replayed. The process waits for the next occurrence. |
| Minute granularity | Cron resolves to the minute and the timer may fire a few seconds late. Adequate for standing-agent cadences, not for anything finer. |
| One clock per process | A `[[process]]` declares `every` **xor** `schedule`. Setting both fails the load. |
| No cron names | `JAN`, `MON` and friends are not parsed. Numeric fields only. |
| Local timezone only | Schedules evaluate in the daemon's local timezone. There is no per-agent timezone. |
| Wakes are lossy by design | If the agent is mid-turn or a wake is already queued, a new one drops. A predicate firing twice while the agent reads the first finding should make it look, not run two turns. |
| Conditions need the sandboxed-tool tier | A condition trigger is a standing tool, so it needs the sandboxed-tool host — on unless you set `[tools] enabled = false`. |
