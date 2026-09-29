# Nine
```
 ███╗   ██╗██╗███╗   ██╗███████╗
 ████╗  ██║██║████╗  ██║██╔════╝
 ██╔██╗ ██║██║██╔██╗ ██║█████╗
 ██║╚██╗██║██║██║╚██╗██║██╔══╝
 ██║ ╚████║██║██║ ╚████║███████╗
 ╚═╝  ╚═══╝╚═╝╚═╝  ╚═══╝╚══════╝
```
[![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8.svg)](go.mod)
[![Status: experimental](https://img.shields.io/badge/status-experimental-orange.svg)](#project-status)
[![GHCR](https://img.shields.io/badge/ghcr.io-djordlucas%2Fnine-blue?logo=github)](https://github.com/djordlucas/nine/pkgs/container/nine)

Nine is a **self-hosted AI agent runtime in a single Go binary**: local models through Ollama
or the hosted Mistral API, persistent SQLite state, a TUI and an OpenAPI-specified REST API,
and tools from plugins, MCP servers and sandboxed JS/Wasm.
Use Nine to research subjects, work on codebases, automate processes, experiment.
Nine is developed against small models as a baseline.

The binary ships for Docker, Linux and macOS, and implements client, server and plugin roles at
once. Each session runs a dedicated agent loop that plans work, calls tools, persists data and
orchestrates sub-agent loops — interactively through the TUI, or unattended through scheduled and
periodic goals. Everything — conversations, goals, memories, session events — lives in one SQLite
file.

## Screenshots

<table>
<tr>
<td><img src="docs/img/nine_what_is_ai_agent_runtime.png" alt="Nine TUI answering &quot;What is an AI agent runtime?&quot; by searching its own docs"></td>
<td><img src="docs/img/nine_tell_me_one_headline.png" alt="Nine TUI fetching the current time and searching the web for a news headline"></td>
</tr>
<tr>
<td><img src="docs/img/nine_memory.png" alt="Nine TUI writing a joke and storing it with memory_set"></td>
<td><img src="docs/img/nine_subagent.png" alt="Nine TUI spawning a sub-agent to research banana farming while researching carrot farming itself"></td>
</tr>
</table>

## Quick start

No clone needed — the image ships a working config. Published for `linux/amd64` and
`linux/arm64`. The package is private, so authenticate first with a GitHub token carrying
`read:packages`.

```bash
# 1. Registry access and a model on the host
echo "$CR_PAT" | docker login ghcr.io -u <your-github-username> --password-stdin
ollama pull qwen3.5:4b

# 2. Nine
docker run -d --name nine \
  -p 127.0.0.1:8080:8080 \
  --add-host host.docker.internal:host-gateway \
  -v nine-data:/data \
  ghcr.io/djordlucas/nine:latest

# 3. A session
docker exec -it -u nine nine nine
```

Point Nine at a different LLM without a config file, and verify what you pulled before
running it:

```bash
docker run -d --name nine -v nine-data:/data \
  -e NINE_LLM_ENDPOINT=http://192.168.1.10:11434 \
  -e NINE_LLM_MODEL=qwen3.5:9b \
  ghcr.io/djordlucas/nine:latest

cosign verify ghcr.io/djordlucas/nine:latest \
  --certificate-identity-regexp '^https://github.com/djordlucas/nine/.github/workflows/release-image.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

From a clone, for hacking on Nine or building the image yourself. `make all` produces
exactly one file, `dist/nine` — the CLI, the TUI, the daemon and the four built-in
plugins. Deploying Nine is copying that file.

```bash
git clone https://github.com/djordlucas/nine
cd nine
make all       # native build -> dist/nine
make up        # or: build and run the runtime image
make session   # interactive TUI session
```

Nine reads its config from `$NINE_CONFIG`, `./nine.toml`, `/nine.toml`, then
`~/.nine/nine.toml`, and creates its database on first run at `~/.nine/nine.db`. The repo's
`nine.toml` is a development config and works as-is against a local Ollama; the published
image's baked config is narrower. Configuration belongs to the operator — Nine cannot rewrite
`nine.toml` at runtime.

**`make ci` is the gate.** GitHub Actions is disabled for this repository, so nothing runs on
a push; run it locally before merging. `make scan` adds the Trivy passes. Both need only
Docker.

Prerequisites, the full Makefile target list, the container's environment overrides and the
configuration reference: [installation](docs/installation.md),
[Docker image](docs/docker-image.md), [configuration](docs/configuration.md).

## Concepts and features

| Concept | Description |
|---------|-------------|
| **UI** | CLI or TUI, both thin clients over the daemon's Unix socket |
| **Daemon** | Long-running background process; manages agents, plugins and state |
| **Agent** | An LLM agent running the ReAct loop |
| **Plugin** | A tool container — a standalone binary, or an MCP server |
| **Sandboxed tool** | User-supplied JS or wasm run in-process in a wasm sandbox (wazero), through capability grants |
| **Generated tool** | A sandboxed tool Nine wrote itself, stored as a row; its code is the agent's, its capabilities the operator's |
| **Capability** | A conferred reach — `fs`, `env`, `net.http` — declared by a tool's manifest, granted by the operator in `nine.toml` or by approving a request |
| **Skill** | Markdown how-to note, semantically retrieved into context |
| **Goal** | An open-ended intention with no end condition, pursued in the background |

Every other term — workflow, session plan, standing agent, supervisor, checkpoint, journal —
is defined in the [glossary](docs/glossary.md).

**Modular.** Built-in tools, custom plugins, MCP servers, and JS/Wasm tools all reach the agent
through one dispatcher. Roles gate which of them a given worker may call.

**Persistent.** Conversations, goals, workflows, memory, files, skills and generated tools live
in a database file, and every turn is checkpointed — kill the daemon mid-task and it resumes
with the same history and plan.

**Autonomous.** Given a goal, Nine spawns a background session that wakes on an interval to push
it forward. Standing agents declared in `nine.toml` skip the human entirely: cron-scheduled,
narrowly tool-scoped, surfacing findings to `nine notifications`. Background work always runs at
lower priority than your active conversation.

**Auditable.** An append-only journal records every step the agent has taken — turn boundaries,
the exact LLM request and response, tool I/O, context usage, sub-agent lifecycle. `nine trace`
and `nine replay` read it back; `nine context` shows the session's current context.

**Evolving.** Nine writes its own skills, and its own sandboxed tools, to close capability gaps
— bounded by a capability ceiling that defaults to the workspace. The agent writes the code, the
operator writes the grants.

**Local.** Built to run against a local model through Ollama, with a SQLite database; `mistral`
is the hosted alternative when you want one ([configuration](docs/configuration.md#mistral)).
Tested against `qwen3.5:4b`, `qwen3.5:9b`, `gemma4:e4b` and `gemma4:e2b` on a 16 GB M4
([model compatibility](docs/model-compatibility.md)).

## Architecture

Nine is a daemon/client pair. The CLI is thin — it opens a Unix socket, sends a message, prints
the reply. Everything long-lived is in the daemon.

```
  nine <message> / TUI  ──JSON over Unix socket──┐   (thin client: send, print)
                                                 │
┌────────────────────────────────────────────────▼──────────────────────────┐
│ Daemon                                                                    │
│                                                                           │
│   Conversation Manager ──spawns──┐        Supervisor Agent ──monitors──┐  │
│                                  ▼                                     ▼  │
│                       ┌──────────────────────────────────────────────────┐│
│                       │ Agent Loops   ReAct: reason → act → observe      ││
│                       │ builds context from roles, skills, tools         ││
│                       └───────┬──────────────────────────┬───────────────┘│
│                               │                          │                │
│            ┌──────────────────▼─────────┐   ┌────────────▼──────────────┐ │
│            │ LLM Queue → provider       │   │ Tool Dispatcher           │ │
│            │ supervisor > active        │   │  ├ core (in-process)      │ │
│            │           > background     │   │  ├ plugins (subprocess)   │ │
│            └────────────────────────────┘   │  └ wasm host (capability) │ │
│                                             └────────────┬──────────────┘ │
│   ┌──────────────────────────────────────────────────────▼──────────────┐ │
│   │ memory.Store → SQLite, one file: all durable state + event journal  │ │
│   └─────────────────────────────────────────────────────────────────────┘ │
└───────────────────────────────────────────────────────────────────────────┘
```

**The context builder** treats context as a budget rather than a buffer. Sources compete by
priority — system prompt, tool definitions, self-model, enrichment, message history, scratchpad —
and low-priority ones are trimmed when the budget is tight. Tool definitions are capped at top-N
and ranked by cosine similarity against the query, so the model sees the tools that matter for
*this* turn.

**The LLM queue** serializes requests across every concurrent agent, prioritizing the supervisor
above active conversations above background work, so a goal grinding away in the background never
makes you wait.

**Memory** is one SQLite file reached through a single store, covering conversations, goals,
workflows, KV memory, full-text-searchable files, vectors, skills, generated tools, session plans,
human-in-the-loop state, and the event journal. Operational tables are daemon-private, so an agent
cannot reach in and rewrite its own goal state.

**The event journal** is written off the turn's critical path by an async batched sink, and is
subscribable with durable per-subscriber cursors. The first subscriber links topically-similar
sessions so a later turn can pull relevant prior context in.

Topology, concurrency, the turn lifecycle, boot sequence and the invariants that hold it together:
[docs/architecture.md](docs/architecture.md).

## Tools

**The tool dispatcher** is the single place a tool name resolves to an implementation, with three
backends behind one namespace: core tools calling the store in-process, plugin subprocesses, and
the wasm sandbox host. A name resolves to exactly one — an unexpected collision is a load failure,
not a silent override.

**Plugins** are tool containers, and may run asynchronous jobs. Nine ships four — `shell`,
`files`, `http`, `time` — served out of the `nine` binary itself: the daemon starts each by
re-executing itself as `nine plugin serve <name>`, so they keep process and crash isolation
without their own artifact. A crashing plugin cannot take the daemon down with it. An **MCP
server** declared as `[[mcp.server]]` becomes a plugin too, with its tools prefixed by the server
name; that is how Nine drives a browser ([docs/browser.md](docs/browser.md)).

Writing one means answering two methods — `plugin.describe` and `plugin.call` — over HTTP on a
Unix socket, then dropping the binary beside a manifest in `[plugins].user_dir`
(`plugins.d/weather` + `plugins.d/weather.toml`). The description string is what gets embedded and
ranked for tool selection, so it decides whether your tool is offered to the model at all.
See [docs/plugins.md](docs/plugins.md) and
[docs/plugins-http-transport.md](docs/plugins-http-transport.md).

Memory, durable file storage, semantic search and skills are **not** plugins — they are core
tools, wired into the agent loop and calling the store in-process. They are still ordinary tools
from the model's side.

### Sandboxed tools

Sandboxed tools are files you drop in a directory, run in a wasm sandbox in-process with exactly
the capabilities the operator granted — by default, **none**. No subprocess, no compile step, no
image rebuild. The host runs by default; your own tools live in `[tools] user_dir`
(`./tools.d`), and a `.js` or `.wasm` file with no manifest beside it is never loaded.

```text
tools.d/
  csvstats.toml        # the manifest — the gate, and where the model's view comes from
  csvstats.schema.json # the argument schema
  csvstats.js          # the code: default-export a function
```

The `js` kind runs on a trimmed QuickJS-NG interpreter compiled to wasm: **ES2023 and nothing
else** — no Node standard library, no `require`, no `setTimeout`, no `URL`, and `fetch` only where
`net.http` was granted. Bundle dependencies at development time. For another language or full
speed, ship a `.wasm` module from Rust, TinyGo, Zig or C exporting the two-function ABI
(`nine_alloc`, `nine_run`).

**The manifest declares a need; only `nine.toml` grants it.** The two must match exactly —
declaring something ungranted fails to load, and being granted something undeclared fails too.

```toml
# csvstats.toml — the tool declares a need
[capabilities]
fs = ["read"]

# nine.toml — the operator grants it, by name
[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]
```

| Capability | You get | Default |
|---|---|---|
| `clock`, `random`, `log` | `Date.now()`, `Math.random()`, `console.*` | granted |
| `fs.read` / `fs.write` | mounted directories, addressed by their *guest* path | declare + grant |
| `env` | named keys only (`NINE_*` and `*_API_KEY` can never be granted) | declare + grant |
| `net.http` | a `fetch` subset | declare + grant |

Everything with reach starts at nothing. A capability is either a wazero pre-open or a host
function the daemon exports, so anything else is not "denied" but structurally absent: a sandboxed
tool cannot spawn a process, open a socket, load a native library, or call another tool. `net.http`
is the exception — wazero has no network, so it is a host function and its security is Nine's
problem: the hostname must match `allow_hosts` **and** the dialed address must be publicly
routable, checked immediately before connect, on every redirect hop. Loopback, link-local
(including `169.254.169.254`) and RFC 1918 are refused regardless of the allowlist. Bounds are
always on: one instance per call, a 5s wall clock, 16 MiB.

### The tier Nine writes itself

Nine writes its own tools at runtime, as rows in the store rather than files on disk, running in
the identical sandbox under the identical rules. The tier is **on by default** with
`[workspace].root` as its ceiling — the same directory the shipped file tools reach and `shell`
runs in — and is gated by `[tools] enabled` above it.

`[tools.agent.capabilities]` is a **ceiling**: the most any generated tool may be granted, never an
automatic grant. A tool gets a capability only by declaring it, one that declares nothing runs with
nothing, and declaring past the ceiling is a refusal the model can act on. Declarations are
re-resolved on every load, so narrowing the ceiling disables a tool that no longer fits rather than
leaving it running with reach you withdrew. `tool_write` writes JavaScript and a capability
*declaration*; it has no path to write a grant:

| | Code | Capabilities |
|---|---|---|
| Native plugin | operator (build time) | operator (`nine.toml`) |
| Developer sandboxed tool | developer (file on disk) | operator (`nine.toml`) |
| Generated sandboxed tool | **Nine** (runtime) | operator (`nine.toml`) |

Generated tools may always import a small vendored standard library (`nine:csv`, `nine:date`,
`nine:diff`). External npm packages are a separate switch, off by default: imports are resolved
once, at write time, in the daemon, verified against published checksums, with no install scripts,
and inlined — so by call time the tool has no imports left. A write can be gated on a human;
`require_approval` defaults to `on_capability`. A new tool is visible **next turn**.

```console
$ nine tools
  ok    csv_stats          js     fs.read /srv/data=>/data
  ok    due_date           gen    none
  SKIP  scraper            js     capability fs.read is declared by the tool but not granted; add it
                                  under [tool.<name>.capabilities] or remove the declaration
```

A tool that fails to load is always reported with its reason, and `gen` marks a tool as Nine's own.
`nine tools show <name>` prints one in full, `nine tools deps` answers what third-party code is in
this daemon, `nine tools reload` re-scans live, and `nine tool validate` checks a manifest with no
daemon running. Authoring guide:
[docs/writing-sandboxed-tools.md](docs/writing-sandboxed-tools.md). Design rationale and the full
capability model: [docs/sandboxed-tools.md](docs/sandboxed-tools.md), with the normative contract
in `spec/contracts/toolvm.md` (`nine spec toolvm`).

### Skills

Skills are how Nine improves what it *knows*; generated tools are how it improves what it can
*do*. A skill is a markdown how-to note with YAML frontmatter, stored in the database. Its
description is embedded; when a skill is semantically relevant, its *name* surfaces into the
agent's self-model and the agent reads the body on demand — on-demand documentation rather than a
permanent context tax.

The boundary is deliberate: Nine writes skills and sandboxed-tool code, both store state, listable
and deletable like a goal. It does not generate plugins, write itself a capability grant, rewrite
its config, or rebuild its source at runtime. It can *ask*: `capability_request` records a request
an operator approves or denies, and an approval takes effect without a restart
([docs/self-modification.md](docs/self-modification.md)).

## Documentation

Documentation and specs live in this repo and inside the binary, rendered as markdown when
invoked — `nine docs [topic]` and `nine spec [topic]`, each listing its topics when given none.

[docs/README.md](docs/README.md) is the full guide, ordered for a first-time reader:
[CLI usage](docs/usage.md), [agent loop](docs/agent-loop.md) and
[context builder](docs/context-builder.md), [the event journal](docs/event-journal.md),
[session plans & routines](docs/session-plans.md), [roles](docs/roles.md),
[predefined agents](docs/predefined-agents.md), [scheduling](docs/scheduling.md),
[human-in-the-loop](docs/hitl.md), and the [glossary](docs/glossary.md).

## Project status

**Experimental, stabilizing.** Interfaces change without notice, and there is no support promise
or stability guarantee.

Every feature lands with tests: unit tests, hermetic harness tests for the daemon and its wire
protocol, integration tests against a real container and a real model, and an eval suite that
replays recorded sessions deterministically and runs a live-model matrix
([evals](docs/evals.md), [model compatibility](docs/model-compatibility.md)). What that does not
buy is user mileage — the failure modes that only long uninterrupted runs, unusual hardware or an
unfamiliar model turn up are still ahead of it. Please open an issue when you hit one.

What is still missing, and what is partially there: [docs/roadmap.md](docs/roadmap.md).

## Limits

| Limit | Detail |
|-------|--------|
| Not hardened | Only sandboxed tools run behind a real boundary. The `shell` plugin and native plugins run as the daemon's process user with its full filesystem and network reach. In the published image that user is an unprivileged uid 1000, so the reach stops at the container. |
| API auth is off until configured | The API supports a bearer token (`[api] auth_token`, `--auth-token`, `NINE_API_AUTH_TOKEN`) and TLS. Neither is on by default, and the published image binds `0.0.0.0` — set a token, or publish the port to loopback. Nine warns at startup when it binds a non-loopback address with no token. |
| Single host | The daemon listens on a Unix socket, so every client runs on the same machine. |
| One model at a time | No routing across models within a deployment. |
| Ollama and Mistral only | Other providers are refused at startup rather than falling back. |
| Small-model baseline | Behavior on large hosted models is unmeasured — [model compatibility](docs/model-compatibility.md). |
| Interfaces change without notice | No stability guarantee and no support promise while the project is experimental. |
| Low user mileage | Failure modes that only long runs, unusual hardware or an unfamiliar model turn up have not been hit yet. |
| Config is operator-only | Nine cannot rewrite `nine.toml` at runtime; changing a setting means editing the file and restarting. An approved capability grant is recorded in the store and installed on the running daemon — nothing writes the file. |
| No external pull requests | Deliberate — see [Contributing](#contributing). |

## Security

**Sandboxed tools are sandboxed; nothing else is.** The wasm host is a real boundary —
default-deny capabilities, one instance per call, an SSRF-checked HTTP path — and it applies to
sandboxed tools only. The `shell` plugin runs commands as the daemon's process user, plugins are
ordinary subprocesses with the daemon's own reach, and the agent has real filesystem and network
access through them. Run Nine in Docker or under a restricted user if you are pointing it at
anything you don't trust.

In the published image the daemon runs as an unprivileged uid 1000, so that reach stops at the
container: the agent cannot write outside `/data` or install packages. The image is signed with
cosign and carries an SBOM and build provenance ([docs/docker-image.md](docs/docker-image.md)).
The API on port 8080 has no authentication until you configure one — bind it to localhost, as the
quick start does, or put it behind a reverse proxy.

`[tools.agent]` is on by default, so the agent writes code that then runs — bounded by a ceiling
that defaults to `[workspace].root`. That is the same directory the shipped file tools reach and
`shell` runs in, so it is not new reach for the agent; what it adds is reach for a *generated
tool's dependencies*. Narrow the ceiling with an explicit fs grant if your workspace holds secrets,
or set `enabled = false`. Enabling external npm dependencies for that tier is the riskiest switch
in the system and is off by default, as are `allow_long_running` and `allow_standing`.

Reporting a vulnerability: [.github/SECURITY.md](.github/SECURITY.md).

## Contributing

**Issues yes, pull requests no.** Bug reports, questions and ideas are genuinely welcome — please
open an issue. Pull requests won't be merged; this is a personal project developed solo, and
keeping it single-author is a deliberate choice. See [CONTRIBUTING.md](CONTRIBUTING.md) and the
[code of conduct](CODE_OF_CONDUCT.md).

This project was made with the author's ideas, experience and orchestration, and built with Claude.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).

Copyright (C) 2026 The Nine Authors

This program is free software: you can redistribute it and/or modify it under the terms of the GNU
General Public License as published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version. It is distributed in the hope that it will be
useful, but WITHOUT ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
FOR A PARTICULAR PURPOSE.
