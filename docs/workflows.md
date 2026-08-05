# Workflows

## What a workflow is

A workflow is a named, persistent execution plan the LLM creates before delegating multi-step work to sub-agents. It lives in the `workflows` table of the in-process SQLite store and is visible to both the LLM and the operator at any time.

A workflow has:
- A **name** — short human-readable label
- An ordered list of **steps**, each with a label and a status
- A **status** — `active`, `done`, `failed`, or `cancelled`

Steps have their own statuses: `pending`, `running`, `done`, `failed`, or `skipped`.

## When Nine uses workflows

The LLM decides when to create a workflow. The system prompt steers it toward workflows for any request that requires multiple independent sub-agents. Simple one-turn requests need no workflow.

When a workflow is active, the LLM calls `workflow_list` at the start of each turn to check for in-progress work, then `workflow_get` to read the current step statuses before deciding what to do next.

## Inspecting workflows

```bash
nine workflows
```

Shows all active workflows and the 10 most recently completed ones across all agents:

```
abc12345678  [active]  Refactor database layer
  [done   ] s0: analyse existing schema
  [running] s1: rewrite queries
  [pending] s2: update tests
  [pending] s3: write migration

def98765432  [failed]  Build release pipeline
  [done   ] s0: compile binaries
  [failed ] s1: run integration tests
  [skipped] s2: publish artifacts
```

In the TUI:

```
/workflows
```

## Stopping and failing workflows

### `nine workflow stop <id>`

Cancels an **ongoing** workflow while the daemon is running. This:
- Marks the workflow as `cancelled`
- Marks all `pending` steps as `skipped`
- Marks all `running` steps as `failed` (reason: `stopped`)

Sub-agents that are already mid-flight continue to completion but their results are discarded. The daemon must be running for stop to work.

### `nine workflow fail <id>` / `--all`

Marks one or all active workflows as `failed`. Use this for cleanup after a crash or when a workflow will never complete.

Works whether the daemon is running or not:
- **Daemon running** — sends a message to the daemon, which updates the workflow via the in-process store
- **Daemon down** — the CLI opens the store in-process (`memory.Open`) and updates it directly (memory is an in-process package, not a subprocess)

```bash
nine workflow fail abc12345678      # mark one workflow as failed
nine workflow fail --all            # mark every active workflow as failed
```

The difference from stop:
- `stop` is for live cancellation of an ongoing workflow (daemon must be up)
- `fail` is for post-mortem cleanup (works when daemon is down)

## Stale recovery

If Nine is stopped ungracefully (OOM, SIGKILL, power loss), any `running` step is permanently stuck — no process will ever update it.

On every daemon start, Nine automatically runs a **startup scrub** that:
- Marks all `running` steps as `failed` (reason: `interrupted`)
- Closes any workflow where all steps are now terminal

Workflows that still have `pending` steps are left `active` so the LLM can resume them on the next turn. The scrub requires no user action.

## TUI commands

| Command | Description |
|---------|-------------|
| `/workflows` | List active and recent workflows |
| `/status` | Shows active workflows alongside agents and sub-agents |

## Limitations

- **No goroutine kill on stop** — `workflow stop` marks steps as cancelled but does not interrupt running sub-agent goroutines. They continue to their natural completion; results are discarded. Hard cancellation is deferred.
- **Single-owner** — a workflow belongs to the agent that created it. Sub-agents cannot create child workflows (depth-capped the same way `run_agent` is).
- **No real-time streaming** — step status changes are delivered as notifications at the start of the next turn, not mid-turn. True real-time push is deferred.
