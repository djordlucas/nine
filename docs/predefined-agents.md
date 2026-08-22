# Pre-defined agents

An operator can declare **standing agents** in `nine.toml`. They come up when
the daemon boots, without a human opening a session first, and work on their
brief indefinitely.

A standing agent is not a new kind of thing. It is a **goal seeded from
configuration**, run by the same [goal session](goal-sessions.md) machinery that
serves goals created in conversation, under a [role](roles.md) narrow enough to
do its job and nothing more.

## Declaring one

```toml
[[agent]]
id          = "sec-watch"      # stable, operator-chosen — not a UUID
description = "Monitor this repo for security issues; triage new CVEs affecting our deps."
role        = "monitor"        # optional; default "monitor" (read-only)
delegates   = false            # optional; default false — opt in to sub-agent fan-out
schedule    = "0 9 * * 1-5"    # cron: weekdays at 9am
#                              # …or…
# interval  = "24h"            # a plain duration instead
```

`id` is chosen by the operator and is what reconciliation keys on, so renaming
it creates a second agent rather than renaming the first.

`schedule` and `interval` are alternatives — a cron expression or a fixed
cadence. Setting neither gets the default goal-session cadence. See
[scheduling](scheduling.md).

`role` defaults to `monitor`, which is read-only: web and HTTP reads, file
reads, and memory. An agent that needs to change things opts into a wider role
explicitly, and `delegates` is a separate opt-in on top — narrow by default in
both dimensions, because a standing agent runs unattended.

## Who owns what

Editing a standing agent should not mean deleting and recreating it, and an
agent should be able to decide it is stuck or finished. Those two wants split
the authority cleanly:

| | Owner | On boot |
|---|---|---|
| description, role, delegates, trigger | **config** | Reconciled in place. Edit the file, restart, the agent picks it up. |
| goal status | **the agent** | Never overridden. |

So **a finished agent stays finished** even while it is still listed in
`nine.toml`. The list is the set of agents Nine knows about and will keep the
definition of — not a command to force them all active. Deciding the work is
done is the agent's call, and config does not overrule it.

Removing an entry stops Nine reconciling that goal; it does not tear the goal
down. Reactivating a finished agent is a deliberate act — set its goal status
back to active.

## Limits worth knowing

**Configuration is read at boot.** Re-activating a paused agent while the daemon
runs does not re-spawn its session with the configured role and trigger until
the next restart.

**Role, delegation and trigger are not stored on the goal.** They are re-read
from configuration each boot. The durable state is the goal — its description
and status — and the session's plan.

## Related

- [Goal sessions](goal-sessions.md) — the machinery a standing agent runs on
- [Roles](roles.md) — what `monitor` and the other roles may do
- [Scheduling](scheduling.md) — cron and interval triggers
- [Configuration](configuration.md) — the `[[agent]]` block in context

> The design, the phasing, and why this needed no new primitive —
> [../adr/predefined-agents-design.md](../adr/predefined-agents-design.md).
