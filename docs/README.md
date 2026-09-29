# Nine user guide

Nine is a self-contained AI agent daemon. It runs a persistent background
process that handles tasks, manages memory, runs background goals and
workflows, and improves itself by writing skills. It does not modify its own
code or configuration at runtime — its executable shape is fixed.

New to Nine: read [Installation](installation.md), then [CLI usage](usage.md),
then [Configuration](configuration.md). Everything else is reference.

## Quick start

The daemon runs as one container; there is no docker-compose.

```bash
make up        # build and run the daemon
make session   # open an interactive TUI session
```

Native build:

```bash
make all
./dist/nine "What files are in the current directory?"
```

Nine auto-starts the daemon on first use. Later calls share the same running
daemon and conversation history. Configure your LLM provider in `nine.toml`
before the first run.

## Getting started

| Document | Covers |
|----------|--------|
| [Installation](installation.md) | Build from source, Docker, first run |
| [Container image](docker-image.md) | The published image: registries, tags, verification, configuration |
| [CLI usage](usage.md) | Commands, interactive TUI, slash commands, background tasks |
| [Configuration](configuration.md) | `nine.toml` reference, LLM providers, environment variables |
| [Operations](operations.md) | Backup, restore, upgrade, and what retention is already discarding |

## Architecture

Ordered outside in.

| Document | Covers |
|----------|--------|
| [Architecture](architecture.md) | Topology, concurrency, turn lifecycle, boot sequence, invariants |
| [Daemon](daemon.md) | Unix socket server, message dispatch, conversation lifecycle |
| [HTTP API](api.md) | REST endpoints, OpenAPI spec, authentication, `nine api` |
| [AgentWorker](runner.md) | Per-conversation loop wrapper, stall detection, checkpointing |
| [Agent loop](agent-loop.md) | The ReAct implementation: reason, act, observe |
| [Context builder](context-builder.md) | Token budgeting, message trimming, tool relevance ranking |
| [Session plans and routines](session-plans.md) | Per-session routines, idle scheduling, self-reflection, goal pursuit |
| [Roles](roles.md) | Worker kinds as data: persona and enforced tool allowlist |
| [Event journal](event-journal.md) | Append-only record of every model exchange and tool call; subscribing to it |
| [Tool selection](tool-selection.md) | Pre-turn ranking and mid-turn search over the tool catalog |
| [Goal sessions](goal-sessions.md) | The background session paired with each top-level goal |
| [Thinking and planning](thinking-and-planning.md) | Reasoning before acting, and plan approval |

## Features

| Document | Covers |
|----------|--------|
| [Plugins](plugins.md) | The tool catalog by tier — plugins, shipped sandboxed tools, core — and writing your own |
| [Plugin transport](plugins-http-transport.md) | HTTP over a Unix socket: the contract and concurrency bounds |
| [Plugin capabilities](plugin-capabilities.md) | Per-plugin settings, cache directories, and long-running jobs |
| [Browser automation](browser.md) | Driving a browser via Playwright's MCP server |
| [Writing sandboxed tools](writing-sandboxed-tools.md) | Adding a JS or wasm tool with two files |
| [Sandboxed tools](sandboxed-tools.md) | The wasm tool host and its capability model |
| [Large tool output](tool-output.md) | Caps, spilling to the store, reading back by reference |
| [Skills](skills.md) | What skills are; creating and managing them |
| [Self-documentation](self-documentation.md) | How Nine retrieves its own bundled docs and spec |
| [Workflows](workflows.md) | Persistent multi-step execution plans for sub-agent delegation |
| [Pre-defined agents](predefined-agents.md) | Standing agents declared in config |
| [Scheduling](scheduling.md) | Interval and cron wake triggers for standing agents |
| [Human-in-the-loop](hitl.md) | `ask_human` and approval gates for interactive sessions |
| [Self-improvement and boundaries](self-modification.md) | Skill writing, and what Nine will not change |
| [Personalities](personalities.md) | Packaging a configured Nine as a specialized agent |
| [Queued messages](queued-messages.md) | Sending input while a session is busy |

## Reference

| Document | Covers |
|----------|--------|
| [Glossary](glossary.md) | Every concept in one place, grouped by topic |
| [Versioning](versioning.md) | Release, plugin-protocol, tool-ABI, config, and schema versioning |
| [Evals](evals.md) | Replay and live-model tracks, the case schema, the feature map |
| [Model compatibility](model-compatibility.md) | Models Nine has run against, results, hardware |
| [Roadmap](roadmap.md) | What is still missing or partial, undated |

## TUI slash commands

`nine` with no arguments opens the TUI. `/help` lists every command. Common ones:

| Command | Shows |
|---------|-------|
| `/sessions` | Running sessions, with IDs to reattach to |
| `/status` | Daemon uptime, active agents, loaded plugins |
| `/context [id]` | The assembled-context token breakdown, with no model call |
| `/tools [filter]` | Every tool the session can call |
| `/standing [id]` | Standing tools, or one with its recent activity |
| `/goals`, `/workflows` | Goals, and active and recent workflows |
| `/grants [approve\|deny\|revoke <id>]` | Capability requests waiting, and the ceiling in force |
| `/plan-mode <mode>` | The session's reasoning mode: `off`, `plan-only`, `always` |
| `/think <message>` | One message with reasoning forced on |
| `/new`, `/clear` | A fresh conversation; a cleared screen |

[CLI usage](usage.md#tui-slash-commands) documents them all with examples.

## Limits

| Limit | Detail |
|-------|--------|
| No search across documents | `nine docs <topic>` renders one document. The agent's `doc_search` tool searches the set; a reader on the command line does not have an equivalent. |
| Index maintained by hand | A new document under `docs/` does not appear here automatically. |
| Design rationale is elsewhere | These documents describe present behavior only. Why a design was chosen lives in [`adr/`](../adr/README.md), which does not ship in the binary. |
