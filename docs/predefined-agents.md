# Pre-defined agents

An operator can declare **standing agents** in `nine.toml`. They come up when
the daemon boots, without a human opening a session first, and work on their
brief indefinitely.

A standing agent is not a new kind of thing. It is a **goal seeded from
configuration**, pursued by the same `pursue` [process](processes.md) that
serves goals created in conversation, under a [role](roles.md) narrow enough to
do its job and nothing more.

## Declaring one

```toml
[[process]]
name      = "sec-watch"      # stable, operator-chosen — not a UUID
tool      = "pursue"
goal      = "Monitor this repo for security issues; triage new CVEs affecting our deps."
role      = "monitor"        # optional; default "monitor" (read-only)
delegates = false            # optional; default false — opt in to sub-agent fan-out
schedule  = "0 9 * * 1-5"    # cron: weekdays at 9am
#                            # …or…
# every   = "24h"            # a plain duration instead
```

`name` is chosen by the operator and is what reconciliation keys on, so renaming
it creates a second agent rather than renaming the first. It is also the goal's
id and the session's.

`schedule` and `every` are alternatives — a cron expression or a fixed cadence.
Setting neither gets the default goal-session cadence, five minutes. See
[scheduling](scheduling.md).

**A condition** wakes the agent when a cheap predicate finds something, instead
of on a clock: the predicate is a process of its own, piped to the agent's
session.

```toml
[[process]]
name      = "cve-scan"
tool      = "cve_scan"
every     = "10s"
args      = { manifest = "/srv/app/go.sum" }
report_to = "sec-watch"
```

**Reflection** can run in the agent's own session, with its history and under
its role:

```toml
[[process]]
name    = "sec-watch-reflect"
tool    = "reflect"
every   = "30m"
session = "sec-watch"
```

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
| goal description, role, delegates, trigger | **config** | Reconciled in place. Edit the file, restart, the agent picks it up. |
| goal status | **the agent** | Never overridden. |

So **a finished agent stays finished** even while it is still listed in
`nine.toml`. The list is the set of agents Nine knows about and will keep the
definition of — not a command to force them all active. Deciding the work is
done is the agent's call, and config does not overrule it.

Removing an entry stops Nine reconciling that goal; it does not tear the goal
down, unless `[processes] authoritative = true`, which retires it at the next
boot. Reactivating a finished agent is a deliberate act — set its goal status
back to active.

## Related

- [Processes](processes.md) — `pursue`, pipes, attachment, and the `[[process]]` block
- [Goal sessions](goal-sessions.md) — the machinery a standing agent runs on
- [Roles](roles.md) — what `monitor` and the other roles may do
- [Scheduling](scheduling.md) — cron and interval triggers

> The design, the phasing, and why this needed no new primitive —
> [../adr/predefined-agents-design.md](../adr/predefined-agents-design.md).

## Limits

| Limit | Detail |
|-------|--------|
| Configuration is read at boot | Editing a block takes effect at the next start. |
| Renaming `name` creates a second agent | Reconciliation keys on `name`, so an edited name leaves the old goal in place and adds a new one. |
| A standing agent counts against the process cap | It runs as a process, so `[processes] max_running` (default 14) bounds standing agents, conversational goals and every other process together. |
| A goal process runs `pursue` | `goal` is only for `tool = "pursue"`; another tool cannot be bound to a goal yet. |
| A condition needs the tool host | Its predicate is a sandboxed tool, so it requires `[tools] enabled`. |
