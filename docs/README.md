# Nine — User Guide

Nine is a self-contained AI agent daemon. It runs a persistent background process that handles tasks, manages memory, runs background goals and workflows, and improves itself by writing skills. It deliberately does not modify its own code or configuration at runtime — its executable shape is fixed.

## Table of Contents

These are ordered for a first-time reader: get Nine running and learn the CLI, then
work through the architecture from the outside in, then read about the individual
features built on top of that architecture.

### Getting Started

1. [Installation](installation.md) — Build from source, Docker, first run (see also
   [Single-container Nine](single-container.md) for the container's design)
2. [CLI Usage](usage.md) — Commands, interactive TUI, slash commands, background tasks, examples
3. [Configuration](configuration.md) — `nine.toml` reference, LLM providers, environment variables

### Architecture

4. [Architecture](architecture.md) — The big picture: daemon, agent loop, context budgeting, plugin protocol, memory (PostgreSQL)
5. [Architecture (detailed)](architecture_detailed.md) — Deep dive: topology, concurrency, boot sequence, invariants
6. [Daemon Architecture](daemon.md) — Unix socket server, message dispatch, conversation lifecycle
7. [Runner Architecture](runner.md) — Per-conversation agent loop wrapper, stall detection, checkpointing
8. [Agent Loop](agent-loop.md) — The ReAct (reason → act → observe) implementation
9. [Context Builder](context-builder.md) — Token budgeting, message trimming, and tool relevance ranking
10. [Session Plans & Stages](session-plans.md) — Per-session stages, idle scheduling, self-reflection, and background goal pursuit
11. [Roles](roles.md) — Role-gated tool allowlists, delegation, depth guards
12. [Event Log](event-log.md) — The append-only session event journal, trace, and deterministic replay
13. [Reactive Events](reactive-events.md) — Journal subscriptions and out-of-band enrichment (related sessions)

### Features

14. [Plugins](plugins.md) — Built-in plugins, writing custom plugins, the plugin lifecycle
15. [Plugins — HTTP transport](plugins-http-transport.md) — The HTTP/SSE plugin transport
16. [Browser Plugin](browser.md) — Headless Chromium: navigate, screenshot, extract, interact
17. [Skills](skills.md) — What skills are, creating and managing skills
18. [Self-Documentation](self-documentation.md) — How Nine retrieves its own bundled docs and spec to answer questions about itself
19. [Workflows](workflows.md) — Persistent multi-step execution plans for sub-agent delegation
20. [Predefined Agents](predefined-agents.md) — Config-declared standing agents (goals + pursue shells)
21. [Scheduling](scheduling.md) — Interval and cron wake triggers for standing agents
22. [Human-in-the-Loop](hitl.md) — `ask_human` and approval gates for interactive sessions
23. [Self-Improvement & Boundaries](self-modification.md) — Skill writing, and why Nine does not modify itself

### Reference

24. [Glossary](glossary.md) — All key concepts and features in one place, grouped by topic
25. [Versioning](versioning.md) — Release, plugin-protocol, config, and schema versioning
26. [Evals](evals.md) — Test plan for replays + live models; the case schema and feature map used to generate test cases
27. [Model compatibility](model-compatibility.md) — Which models Nine has been run against, how they did, and on what hardware

## Quick Start

### Docker (recommended)

Postgres and the daemon run together as one container ([Single-container
Nine](single-container.md)) — no docker-compose:

```bash
# 1. Build + run — Postgres + the daemon (edit nine.toml for your LLM)
make up

# 2. Open an interactive session
make session
```

### Native (local dev)

```bash
# 1. Build
make all

# 2. Configure your LLM provider in nine.toml

# 3. Run
./dist/nine "What files are in the current directory?"
```

Nine auto-starts the daemon on first use. Subsequent calls share the same running daemon and conversation history.

## Key Concepts

| Concept | Description |
|---------|-------------|
| **Daemon** | Long-running background process, manages agents, holds plugin state |
| **Agent** | A conversation thread running the ReAct loop (reason → act → observe) |
| **Plugin** | Standalone binary exposing tools via JSON-RPC; fixed at build time, started at boot |
| **Skill** | Markdown how-to note describing a reusable capability, semantically retrieved into context |
| **Supervisor** | A special agent that monitors others for stalls and capability gaps (durable, journal-backed) |
| **Checkpoint** | Serialized agent state (messages + scratchpad) persisted to the `conversations` table in PostgreSQL |

## TUI Slash Commands

The interactive TUI (`nine` with no arguments) supports slash commands. Type `/help` to see the full list. Common ones:

| Command | Description |
|---------|-------------|
| `/status` | Daemon uptime, loaded plugins, active agents |
| `/tools` | All available tools grouped by plugin |
| `/skills` | List skills |
| `/memory` | Browse the KV memory store |
| `/config` | Show running configuration |
| `/new` | Start a fresh conversation |
| `/clear` | Clear the screen |
