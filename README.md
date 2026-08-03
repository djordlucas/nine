# Nine
```
 ███╗   ██╗██╗███╗   ██╗███████╗
 ████╗  ██║██║████╗  ██║██╔════╝
 ██╔██╗ ██║██║██╔██╗ ██║█████╗
 ██║╚██╗██║██║██║╚██╗██║██╔══╝
 ██║ ╚████║██║██║ ╚████║███████╗
 ╚═╝  ╚═══╝╚═╝╚═╝  ╚═══╝╚══════╝
```
**A local and durable AI agent daemon.**

[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![Status: experimental](https://img.shields.io/badge/status-experimental-orange.svg)](#project-status)

Nine is a persistent background agent: a daemon that holds long-lived state, runs a
ReAct loop over a pluggable set of tools, pursues goals between your turns, and
writes its own skills as it learns. It is built to run against a local model with a
local database — your prompts, your conversation history, and everything the agent
remembers stay on hardware you control.

Most agent tooling assumes a cloud model and a vendor's storage. That is a
reasonable default, and it is not this one. Nine's default configuration points at
[Ollama](https://ollama.com) on `localhost` and a PostgreSQL instance you run
yourself; no API key is required and nothing leaves the machine. 

Anthropic's API is supported as a first-class provider when you want a frontier model, but it is opt-in
rather than the path of least resistance. Swapping between the two is a two-line
config change, because the LLM layer is a single-method interface.
Nine is designed and developped with smaller models to ensure Nine remains useful with modest hardware.

The other half of the idea is that an agent should be *durable*. Nine's state is not
a process that dies with your terminal. Conversations, goals, workflows, memory, and
a complete append-only journal of every step the agent has ever taken live in
Postgres. You can kill the daemon mid-task and it resumes. You can replay a recorded
session deterministically, with no live model and no tool calls, and watch exactly
what happened.

Use Nine to help you solve tasks and issues that matter to you.

Extend Nine with skills and plugins to make it more useful and deliberate.

## Project status

**Experimental, looking to stabilize.**

It works, and it is not a small system — but interfaces change without notice, and there is no support
promise or stability guarantee. Treat it as something to read, run, expirement with for now, and it 
will eventually stabilize into a production-ready state.

**Nine is not security hardened, yet, but will be.**

See [Contributing](#contributing) before opening a pull request.


## Note on documentation
Nine's documentation and specs are available within the "nine" binary.
They are rendered as markdown when invoke for easy viewing.

```
nine docs [topic] Show bundled documentation (no topic lists them)
nine spec [topic] Show a bundled specification (no topic lists them)
```

**See**: docs/usage.md for full details


## AI Use / Methodology
This project was made in part with Claude. The design is the author's, the code mostly written by Claude.
For major features, a design document was made, iterated upon many times, implemented tested and then a specification document written.

## Quick start
The fastest and recommended path is Docker, which brings up Postgres and the daemon together:

Nine defaults to Ollama at `host.docker.internal:11434`, so pull a model on the host
first:

```bash
ollama pull gemma4:e4b
```

Pull the code
```bash
git clone https://github.com/djordlucas/nine
cd nine
```

Configure Nine - see docs/configuration.md.

Interesting options to change initially:
```
[plugins]
bin      = "./dist/bin"  # plugins binaries

[workspace]
root     = "./workspace" # Filesystem root, use a bind mount for external access

[llm]                     # model configuration
provider       = "ollama"
model          = "gemma4:12b"
endpoint       = ""              # empty uses Ollama's local default
num_ctx        = 32768
max_concurrent = 1
```

Start Nine
```bash
make up                # Postgres (pgvector) + the daemon, one container
make session           # interactive TUI session
make shell             # sh into nine's container for debug
```

The daemon auto-starts on first use. Later calls share the same daemon and
conversation history. Run `nine` with no arguments for the interactive TUI.

## Installation

### Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Go | 1.26+ | Native build |
| PostgreSQL | 16+ with `pgvector` | All persistent state; `make pg` provides a standalone instance for native use |
| Node.js | 18+ | Browser plugin only |
| Docker | 24+ | Container build (runs Postgres + the daemon together, no compose) |
| golangci-lint | latest | Optional, for `make lint` |

Plus an LLM provider: a running Ollama, or an Anthropic API key.

### Build from source

```bash
git clone https://github.com/djordlucas/nine
cd nine
make all
```

This produces `dist/nine` (the CLI and daemon in one binary) and the plugin binaries
in `dist/bin/`. The browser plugin additionally needs Node and npm.

Nine looks for its config, in order: `$NINE_CONFIG`, `./nine.toml`, `/nine.toml`,
then `~/.nine/nine.toml`. The repo's `nine.toml` works as-is against a local Ollama
and `make pg`'s standalone Postgres.

### Docker (single container)

Nine cannot be decoupled from its database — the daemon fails fast if Postgres
is unreachable — so the deployment unit is **one container** running both,
supervised together by s6-overlay ([docs/single-container.md](docs/single-container.md)).
There's no docker-compose file; `docker run` is wrapped in Makefile targets:

```bash
make up                # built runtime image
make up-hot            # hot-reload: rebuilds and restarts the daemon on any .go change
make down              # stop, keeping all data
make destroy           # remove everything, including all data volumes and images
```

The runtime image holds the compiled binary, the plugins, and the built-in skills
— no Go toolchain and no source tree, because Nine never compiles anything at
runtime. Hot-reload mode bind-mounts the source and rebuilds via `inotifywait` —
this is the development path. The browser plugin ships in both images. pgAdmin
is opt-in tooling, not part of either image (`make pgadmin`).

The full Makefile target list is in [docs/installation.md](docs/installation.md).

## Configuration

Nine is configured through a single `nine.toml` — one file for every deployment. It is
written for the native layout, and the containers override the handful of values that
differ (LLM endpoint, database DSN, plugin and workspace paths) through environment
variables rather than a second config file. The essentials:

```toml
[llm]
provider       = "ollama"        # or "anthropic" — these are the only two
model          = "gemma4:12b"
endpoint       = ""              # ollama; empty uses its local default
api_key        = ""              # anthropic: empty reads ANTHROPIC_API_KEY
num_ctx        = 32768           # also sets the per-turn context budget
max_concurrent = 1               # keep at 1 for local models
thinking       = true            # stream reasoning as a live trace in the TUI

[daemon]
socket_path          = "/tmp/nine.sock"
task_timeout_seconds = 1800
max_goal_sessions    = 10        # concurrent background "pursue" sessions

[memory]
# Postgres holds every piece of durable state. The daemon fails fast if it's
# unreachable — this is primary storage, not a cache.
database_url = "postgres://nine:nine@localhost:5433/nine?sslmode=disable"

[embeddings]
provider = ""                    # "" or "keyword" = built-in, no model, no network
                                 # "ollama" for better ranking; "none" disables

[planning]
plan_mode     = "plan-only"      # off | plan-only | always
plan_approval = "on-risky"       # off | on | on-risky
```

Environment variables override the file. The most useful:

| Variable | Description |
|----------|-------------|
| `NINE_CONFIG` | Explicit config path, skipping the search order |
| `NINE_LLM_PROVIDER` / `NINE_LLM_MODEL` / `NINE_LLM_ENDPOINT` | Override the LLM without editing config |
| `NINE_DATABASE_URL` | Override the Postgres DSN |
| `NINE_PLUGINS_BIN` / `NINE_WORKSPACE_ROOT` | Override the plugin and workspace paths (how the container reuses `nine.toml`) |
| `ANTHROPIC_API_KEY` | Anthropic key |
| `SEARCH_PROVIDER` / `SEARCH_API_KEY` | `web_search` backend: `brave` or `serpapi`. Unset uses DuckDuckGo, no key needed. |
| `NINE_LOG_LEVEL` / `NINE_LOG_FORMAT` | `debug`/`info`/`warn`/`error`; `text`/`json` |

Configuration belongs to the operator, not the agent: Nine cannot rewrite `nine.toml`
at runtime. Change a setting by editing the file and restarting the daemon.

Full reference: [docs/configuration.md](docs/configuration.md).

## Architecture

Nine is a daemon/client pair. The CLI is thin — it opens a Unix socket, sends a
message, prints the reply. Everything long-lived is in the daemon.

```
┌─────────────────────────────────────────────────────┐
│  nine <message>  (CLI client)                       │
│  Connects to Unix socket, sends message,            |
|  prints reply                                       │
└─────────────────┬───────────────────────────────────┘
                  │ JSON over Unix socket
┌─────────────────▼───────────────────────────────────┐
│  Daemon                                             │
│  ┌──────────────┐   ┌───────────────┐               │
│  │ Conversation │   │  Supervisor   │               │
│  │  Manager     │   │  Agent        │               │
│  └──────┬───────┘   └───────┬───────┘               │
│         │ spawns many       │ monitors              │
│  ┌──────▼───────────────────▼───────┐               │
│  │           Agent Loops            |               |
|  |          Builds context          |               |
|  |      Uses roles, skills, tools   │               │
│  │  (ReAct: reason → act → observe) │               │
│  └──────────────┬───────────────────┘               │
│                 │                                   │
│  ┌──────────────▼───────────────────┐               │
│  │         LLM Queue                │               │
│  │  priority: supervisor > active   │               │
│  │           > background           │               │
│  └──────────────┬───────────────────┘               │
│                 │                                   │
│  ┌──────────────▼───────────────────┐               │
│  │         LLM Provider             │               │
│  │  (Anthropic / Ollama)            │               │
│  └──────────────────────────────────┘               │
│                                                     │
│  ┌────────────────────────────────────────────────┐ │
│  │  Plugin Manager                                │ │
│  │  shell  files  http  time  browser             │ │
│  │  (subprocesses; reached with unix sockets)     │ │
│  └────────────────────────────────────────────────┘ │
│                                                     │
│  ┌────────────────────────────────────────────────┐ │
│  │  memory.Store  →  PostgreSQL + pgvector        │ │
│  │  (in-process; all durable state + the event    │ │
│  │   journal; memory/file/skill tools are core,   │ │
│  │   not plugins)                                 │ │
│  └────────────────────────────────────────────────┘ │
└─────────────────────────────────────────────────────┘
```

**The agent loop** implements ReAct: assemble the turn, submit to the LLM queue, then
either dispatch the returned tool calls and loop, or commit a final answer and
checkpoint. A scratchpad accumulates tool calls and their output within a turn.

**The context builder** treats context as a budget rather than a buffer. Sources
compete by priority — system prompt, then tool definitions, self-model, enrichment,
message history, scratchpad — and the low-priority ones are trimmed or dropped when
the budget is tight. Tool definitions are capped at top-N and ranked by cosine
similarity between your query and each tool's description embedding, so the model
sees the tools that matter for *this* turn instead of all of them.

**The LLM queue** serializes requests across every concurrent agent, prioritizing the
supervisor above active conversations above background work, so a goal grinding away
in the background never makes you wait.

**Memory** is one PostgreSQL database with pgvector, reached through a single store.
Schema is applied idempotently on open. Tables cover conversations, goals, workflows,
KV memory, full-text-searchable files, vectors, skills, session plans, human-in-the-loop
state, and the event journal. Operational tables are daemon-private — never exposed to
the agent as tools — so an agent cannot reach in and rewrite its own goal state.

**The event journal** records every session's trajectory: turn boundaries, exact LLM
request and response, tool I/O with latency and errors, context usage, sub-agent
lifecycle. It is written off the turn's critical path by an async batched sink, and it
is what makes `nine trace` and deterministic `nine replay` possible. It is also
subscribable, with durable per-subscriber cursors; the first subscriber links
topically-similar sessions so a later turn can pull relevant prior context in.

**Goals vs. workflows** is the central distinction in how Nine plans. A workflow is a
finite, multi-step plan with dependency gating and auto-close. A goal is open-ended
with no end condition — "monitor this repo for security issues" — and each top-level
goal gets a background session that wakes every five minutes to make progress on it.

Deeper treatments live in [docs/architecture.md](docs/architecture.md) and
[docs/architecture_detailed.md](docs/architecture_detailed.md).

## Plugins

Nine ships five plugins — `shell`, `files`, `http`, `time`, and the optional
`browser` (headless Chromium: navigate, screenshot, extract, interact). They are
separate binaries, spawned by the daemon, fixed at build time. A crashing plugin is
isolated from the daemon and from active conversations.

Memory, durable file storage, semantic search, and skills are **not** plugins — they
are core-intercepted, wired into the agent loop and calling the store in-process.

### Writing one

A plugin is any executable that answers two methods over HTTP on a Unix socket. The
daemon spawns it with `NINE_PLUGIN_SOCKET` set; `plugin.Serve` listens there and
handles `POST /rpc`. HTTP gives per-request concurrency, pooling, and cancellation
via `context` for free. The envelope is `{"method": ..., "params": ...}`, and the
reply is `{"result": ...}` or `{"error": {"code": ..., "message": ...}}`.

`plugin.describe`, called once at startup, advertises the tools:

```json
{
  "result": {
    "protocol_version": 1,
    "max_concurrent": 0,
    "tools": [
      {
        "name": "my_tool",
        "description": "Does something useful",
        "inputSchema": {
          "type": "object",
          "properties": {"input": {"type": "string", "description": "The input value"}},
          "required": ["input"]
        }
      }
    ]
  }
}
```

`plugin.call` runs one invocation:

```json
{"method": "plugin.call", "params": {"tool": "my_tool", "args": {"input": "hello"}}}
→ {"result": {"output": "hello, world"}}
```

That description string matters more than it looks: it is what gets embedded and
ranked for tool selection, so it determines whether your tool is offered to the model
at all.

Add the plugin to the repo and rebuild — nothing is loaded at runtime. External MCP
servers are supported as an exception, speaking JSON-RPC 2.0 over stdio. See
[docs/plugins.md](docs/plugins.md) and
[docs/plugins-http-transport.md](docs/plugins-http-transport.md).

## Skills

Skills are how Nine improves itself — and the only way it does. A skill is a markdown
how-to note with YAML frontmatter, stored in the database:

```markdown
---
name: git-workflow
description: Best practices for Git branching, committing, and pull requests
tags: [git, version-control, workflow]
---

## Branch naming
Use lowercase kebab-case: `feature/add-login`, `fix/null-pointer`.
```

The description is embedded into a vector namespace. When a skill is semantically
relevant to the task at hand, its *name* surfaces into the agent's self-model, and the
agent reads the body on demand — on-demand documentation rather than permanent context
tax. Built-in skills are embedded in the binary and seeded into the database on every
boot, so editing one and rebuilding updates it; skills the agent wrote itself are left
alone.

The boundary is deliberate: Nine writes skills and nothing more. It does not generate
plugins, rewrite its config, or rebuild its source at runtime. Its executable shape is
fixed. The reasoning is in
[docs/self-modification.md](docs/self-modification.md).

## Key concepts

| Concept | Description |
|---------|-------------|
| **Daemon** | Long-running background process; manages agents, holds plugin state |
| **Agent** | A conversation thread running the ReAct loop |
| **Plugin** | Standalone binary exposing tools over HTTP/Unix socket; fixed at build time |
| **Skill** | Markdown how-to note, semantically retrieved into context |
| **Goal** | An open-ended intention with no end condition, pursued in the background |
| **Workflow** | A finite multi-step plan for sub-agent delegation |
| **Supervisor** | Special agent that monitors others for stalls and capability gaps |
| **Checkpoint** | Serialized agent state persisted to Postgres |
| **Journal** | Append-only record of every step, enabling trace and deterministic replay |

## Documentation

[docs/README.md](docs/README.md) is the full guide, ordered for a first-time reader.
Highlights:

- [CLI usage](docs/usage.md) — commands, TUI, slash commands, background tasks
- [Agent loop](docs/agent-loop.md) and [context builder](docs/context-builder.md)
- [Event log](docs/event-log.md) and [reactive events](docs/reactive-events.md)
- [Session plans & stages](docs/session-plans.md) — idle scheduling, reflection, goal pursuit
- [Roles](docs/roles.md) — role-gated tool allowlists, delegation, depth guards
- [Predefined agents](docs/predefined-agents.md) and [scheduling](docs/scheduling.md)
- [Human-in-the-loop](docs/hitl.md) — `ask_human` and approval gates
- [Glossary](docs/glossary.md) — every concept in one place

## Contributing

**Issues yes, pull requests no.** Bug reports, questions, and ideas are genuinely
welcome — please open an issue. Pull requests won't be merged; this is a personal
project developed solo, and keeping it single-author is a deliberate choice.

## Security

Nine does not sandbox anything. The `shell` plugin runs commands as the daemon's
process user, and the agent has real filesystem and network access. Run it in Docker
or under a restricted user if you are pointing it at anything you don't trust.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).

Copyright (C) 2026 The Nine Authors

This program is free software: you can redistribute it and/or modify it under the
terms of the GNU General Public License as published by the Free Software Foundation,
either version 3 of the License, or (at your option) any later version. It is
distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY; without even
the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
