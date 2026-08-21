# Nine — User Guide

Nine is a self-contained AI agent daemon. It runs a persistent background process that handles tasks, manages memory, runs background goals and workflows, and improves itself by writing skills. It deliberately does not modify its own code or configuration at runtime — its executable shape is fixed.

## Table of Contents

These are ordered for a first-time reader: get Nine running and learn the CLI, then
work through the architecture from the outside in, then read about the individual
features built on top of that architecture.

### Getting Started

1. [Installation](installation.md) — Build from source, Docker, first run (the container's
   design rationale is recorded in [adr/](../adr/single-container.md))
2. [CLI Usage](usage.md) — Commands, interactive TUI, slash commands, background tasks, examples
3. [Configuration](configuration.md) — `nine.toml` reference, LLM providers, environment variables

### Architecture

4. [Architecture](architecture.md) — Topology, concurrency, the turn lifecycle, boot sequence, invariants
5. [Daemon Architecture](daemon.md) — Unix socket server, message dispatch, conversation lifecycle
6. [Runner Architecture](runner.md) — Per-conversation agent loop wrapper, stall detection, checkpointing
7. [Agent Loop](agent-loop.md) — The ReAct (reason → act → observe) implementation
8. [Context Builder](context-builder.md) — Token budgeting, message trimming, and tool relevance ranking
9. [Session Plans & Routines](session-plans.md) — Per-session routines, idle scheduling, self-reflection, and background goal pursuit
10. [Roles](roles.md) — Role-gated tool allowlists, delegation, depth guards
11. [The event journal](event-journal.md) — The append-only record of every model exchange and tool call, and subscribing to it
12. [How tools reach a turn](tool-selection.md) — Ranking, mid-turn lookup, and why a tool's description is its retrieval surface
13. [Goal sessions](goal-sessions.md) — The background session paired with each top-level goal: its cycle, and the bounds on it
14. [Thinking & planning](thinking-and-planning.md) — Reasoning before acting, watching it reason, and approving a plan

### Features

15. [Plugins](plugins.md) — Built-in plugins, writing custom plugins, the plugin lifecycle
16. [Plugins — HTTP transport](plugins-http-transport.md) — The HTTP/SSE plugin transport
17. [Browser Automation](browser.md) — Driving a browser via Playwright's MCP server; the worked MCP example
18. [Writing sandboxed tools](writing-sandboxed-tools.md) — Add a JS or wasm tool with two files, run in a capability-scoped sandbox
19. [Sandboxed tools — design](sandboxed-tools.md) — The wasm tool host: why it exists, the capability model, and what is deliberately unbuilt
20. [Large tool output](tool-output.md) — Caps, spilling an oversized result to the store, reading it back, and passing a payload by reference
21. [Skills](skills.md) — What skills are, creating and managing skills
22. [Self-Documentation](self-documentation.md) — How Nine retrieves its own bundled docs and spec to answer questions about itself
23. [Workflows](workflows.md) — Persistent multi-step execution plans for sub-agent delegation
24. [Predefined Agents](predefined-agents.md) — Config-declared standing agents (goals + pursue shells)
25. [Scheduling](scheduling.md) — Interval and cron wake triggers for standing agents
26. [Human-in-the-Loop](hitl.md) — `ask_human` and approval gates for interactive sessions
27. [Self-Improvement & Boundaries](self-modification.md) — Skill writing, and why Nine does not modify itself

### Reference

28. [Glossary](glossary.md) — All key concepts and features in one place, grouped by topic
29. [Versioning](versioning.md) — Release, plugin-protocol, tool-ABI, config, and schema versioning
30. [Evals](evals.md) — Test plan for replays + live models; the case schema and feature map used to generate test cases
31. [Model compatibility](model-compatibility.md) — Which models Nine has been run against, how they did, and on what hardware

## Quick Start

### Docker (recommended)

The daemon runs as one container — no docker-compose:

```bash
# 1. Build + run — the daemon (edit nine.toml for your LLM)
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
| **Checkpoint** | Serialized agent state (messages + scratchpad) persisted to the `conversations` table |

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
