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

That turn runs through exactly the same path as a turn you typed. A goal
session is not a special execution mode; it is an ordinary session whose
prompts come from a process instead of a person.

What "useful action" means is left to the model. The cycle supplies the
occasion and the goal, not the plan.

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
| Five-minute wake interval | A goal session's `pursue` process ticks on a fixed clock. The cycle supplies the occasion and the goal, not a plan — what counts as useful action is left to the model. |
| Stall detection releases the slot | Five consecutive turns that call no tool count as a stall, which pauses the goal and so stops its process and frees the slot: one wedged pursuit cannot hold capacity indefinitely. Progress on that goal stops until it is resumed. |
| Only some roles create them | A role that may not spawn goals creates no goal sessions. The operator is not role-gated. |

## Related

- [Processes](processes.md) — `pursue`, goal binding, and the runner that wakes
  goal sessions
- [Scheduling](scheduling.md) — interval and cron wake triggers
- [Pre-defined agents](predefined-agents.md) — goals seeded from configuration,
  which run on this same shell
