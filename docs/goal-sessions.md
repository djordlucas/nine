# Goal sessions

A **goal** is an open-ended intention with no defined end condition — "monitor
this repository for security issues". Something has to actually work on it
between your turns, and that something is a **goal session**: a
[process session](processes.md) paired one-to-one with a top-level goal, driven
by the shipped `pursue` process, which wakes on a clock, assesses the goal, acts,
and goes back to sleep.

The pairing is exact. A goal session's identity *is* its goal's, so there is no
separate bookkeeping tying the two together and no way for them to disagree
about which goal is being pursued. Spawning is idempotent for the same reason: a
goal that already has a session does not get a second one, whatever asks.

## The cycle

Every five minutes `pursue` takes a tick and asks its session to check the goal
and its sub-goals, take any useful action toward it — including spawning
sub-goals or delegating to sub-agents — and update the goal's status if it
should change. The first tick comes five minutes after the session starts.

A tick that finds nothing to do slows the clock. A turn that only looks —
calls nothing but tools that read, such as `goal_get`, `read_file` or
`memory_get` — changes nothing, yet still pays for a model call over the whole
session. From the second such turn in a row, the next tick waits twice as long
as the last: 10 minutes, then 20, 40, and so on up to 6 hours. A turn that calls
any other tool, or a message or event the process is given, brings back the
five-minute clock. The process log says when the clock slowed and for how long.

| Turns in a row that only looked | Next tick in |
|---|---|
| 0 or 1 | 5 minutes |
| 2 | 10 minutes |
| 3 | 20 minutes |
| 8 or more | 6 hours |

Each tick's turn starts from an empty history. A goal session lives for days,
and carrying every earlier turn made each wake cost more than the last: one
goal's turns grew from 49 k to 119 k tokens over four days, with the context
full and the oldest turns cut. The model sees the tick's text and the goal,
reads `notes.md` in the [goal's folder](#a-goals-folder) for what earlier turns
did, and rewrites it before it ends. The journal keeps every turn. A message or
event turn keeps the history, until the next tick.

That turn runs through exactly the same path as a turn you typed. A goal
session is not a special execution mode; it is an ordinary session whose
prompts come from a process instead of a person.

What "useful action" means is left to the model. The cycle supplies the
occasion and the goal, not the plan.

## A goal's folder

Each goal has a folder in the workspace for what it produces, and every turn
`pursue` starts names it: `goals/`, a slug of the description, and the start
of the goal's id — `goals/news-about-canada-1eb9e2c3/`. The turn is told to look
there first for what earlier turns wrote and to update those files rather than
write new copies elsewhere. A goal that names its own location ("keep
notes/plan.md tidy") keeps it. Sub-goals share their parent's folder, since the
parent's session works on them.

## Recurring goals

A goal with a **schedule** recurs: "keep me updated on Canadian news", "a daily
digest of the repository's issues". Such a goal is never attained, so checking
it every five minutes finds nothing to do once its first pass is over. With a
schedule, `pursue` wakes on that cadence instead, and each tick is one run that
does the whole job again: find what is new since the last run, write or update
its result, end the turn. The goal stays active between runs.

| Given as | Example | Runs |
|---|---|---|
| A duration, at least a minute | `24h`, `6h`, `90m` | that long after the previous run |
| A 5-field cron expression | `0 8 * * *` | at those times, in UTC |

The model sets it with `goal_create`'s `schedule`, the operator with
`POST /goals` (`"schedule"`), the web UI's Repeat choice, or `every`/`schedule`
on a goal `[[process]]` block. Only a top-level goal recurs: a sub-goal is worked
on in its parent's runs. The schedule lives on the goal's pursue process, and
the goal reads it back as `schedule`.

## Only top-level goals get one

Sub-goals do not get their own sessions. A goal spawned by another goal is
worked on by the session already pursuing its parent, which is what keeps a
branching goal from turning into a branching population of sessions.

Two things create a top-level goal: an agent whose role is permitted to spawn
goals, and the operator — `POST /goals` on the [HTTP API](api.md), or an
goal `[[process]]` block in `nine.toml`. An operator-created goal is pursued exactly as an
agent-created one is, and `DELETE /goals/{id}` stops its session and removes it.

## Status is owned by the agent

The goal's status decides whether its `pursue` process runs: an active goal has
it running, pausing the goal stops it, reactivating the goal starts it again,
and finishing or archiving it leaves it stopped.

Status flows from the goal, not from configuration. An operator can declare a
[standing agent](predefined-agents.md), but if that agent decides its goal is
done, it is done — re-adding the declaration does not resurrect it. Deciding
that its work is finished is the agent's call to make.

## Limits

| Limit | Detail |
|-------|--------|
| Fourteen running processes | `[processes] max_running`, default 14, counts goal sessions with every other running process. At the cap, creating a goal still records the goal but starts no session for it. The goal is real and unattended rather than queued, so how far behind pursuit has fallen stays visible. |
| Sub-goals get no session | Only top-level goals are paired with a session. A goal spawned by another is worked on by the session already pursuing its parent. |
| Five-minute wake interval | A goal session's `pursue` process ticks every five minutes while it acts, and up to every 6 hours while it only looks. The cycle supplies the occasion and the goal, not a plan — what counts as useful action is left to the model. |
| A schedule is set once | A goal's schedule is set when it is created. Changing it means creating the goal again. |
| What counts as only looking | A fixed list of read tools. A tool missing from it counts as acting, so a new tool can make the clock slow less often, never more. A goal session on a cron `schedule` keeps its schedule. |
| Stall detection releases the slot | Five consecutive turns that call no tool count as a stall, which pauses the goal and so stops its process and frees the slot: one wedged pursuit cannot hold capacity indefinitely. Progress on that goal stops until it is resumed. |
| Only some roles create them | A role that may not spawn goals creates no goal sessions. The operator is not role-gated. |

## Related

- [Processes](processes.md) — `pursue`, goal binding, and the runner that wakes
  goal sessions
- [Scheduling](scheduling.md) — interval and cron wake triggers
- [Pre-defined agents](predefined-agents.md) — goals seeded from configuration,
  which run on this same shell
