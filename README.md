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

Nine is an **AI agent runtime** for putting a local model to work. A daemon stays
running in the background, holds every conversation, goal, note and journal entry in one
SQLite file on your disk, and drives a ReAct loop over a tool surface you assemble. A
model on its own can only produce text; what makes it *do* something is the tools it is
handed and somewhere to keep running once you stop typing — so Nine supplies both. It
gives the model a shell, the filesystem, HTTP, any MCP server you declare — a browser,
say — and whatever else you add, and it hosts that loop in a process that goes on taking turns without you. Anything
a computer can reach is something Nine can be pointed at, to automate outright or to
work alongside you on.

Five properties shape the design.

**Modular.** Tools reach the agent through one dispatcher with several backends behind
it: core tools calling the store in-process, native plugins as separate binaries over a
Unix socket, external MCP servers, and JS or wasm **sandboxed tools** that are two files
dropped in a directory. Roles gate which of them a given worker may call. Adding reach
means adding a tool, not editing the loop — and the LLM layer is a single-method
interface, so the model backend is swappable too.

**Persistent.** State is not a process that dies with your terminal. Conversations,
goals, workflows, memory, files, skills and generated tools live in a database file, and
every turn is checkpointed — kill the daemon mid-task and it resumes with the same
history, the same plan, and the same place in it.

**Autonomous.** Work continues between your turns, because taking a turn does not
require you. Every session carries a plan of stages with an idle scheduler behind it,
and that one mechanism drives the whole autonomous tier: a goal — open-ended, no end
condition, *"monitor this repo for security issues"* — gets a background session that
wakes on an interval to push it forward; **standing agents** declared in `nine.toml`
skip the human entirely, coming up on boot, waking on a cron schedule, narrowly
tool-scoped, and surfacing findings to `nine notifications`; a self-reflection session
and a supervisor watching for stalls and capability gaps run on the same machinery.
Background work is always queued below the conversation in front of you, so a goal
grinding away never makes you wait, and it enriches rather than interrupts — it never
steers a session you are in the middle of.

**Auditable.** An append-only journal records every step the agent has ever taken —
turn boundaries, the exact LLM request and response, tool I/O with latency and errors,
context usage, sub-agent lifecycle. `nine trace` reads it back, and `nine replay`
re-runs a recorded turn deterministically, with no live model and no tool calls, so you
can watch exactly what happened. The same holds for reach: `nine tools` prints the
capabilities each sandboxed tool actually runs with, and the ones that failed to load
with the reason why.

**Evolving.** Nine improves what it *knows* by writing **skills** — markdown how-to
notes, semantically retrieved into context when they are relevant to the task. Where the
operator turns that tier on, it also improves what it can *do*, writing its own
sandboxed tools at runtime to close the gaps it hits. The boundary is firm in both
cases: the agent writes the code, the operator writes the capability grants, and they
are never the same actor. Nine cannot rewrite its config or rebuild its binary.

It is built to run against a local model with a local database. Most agent tooling
assumes a cloud model and a vendor's storage; that is a reasonable default, and it is
not this one. Nine points at [Ollama](https://ollama.com) on `localhost` and a SQLite
file on disk — no API key, no database server, nothing leaving the machine. Local
models are the only ones it talks to, with no hosted-API provider to fall back on, by
design, and it is developed against smaller models to make sure it stays useful on
modest hardware.

## Project status

**Experimental, looking to stabilize.**

It works, and it is not a small system — but interfaces change without notice, and there is no support
promise or stability guarantee. Treat it as something to read, run, expirement with for now, and it 
will eventually stabilize into a production-ready state.

**Under active development, and tested — but not yet tested heavily.**

Every feature lands with tests: unit tests, hermetic harness tests for the daemon and
its wire protocol, integration tests against a real container and a real model, and an
eval suite that both replays recorded sessions deterministically and runs a live-model
matrix ([docs/evals.md](docs/evals.md),
[model compatibility](docs/model-compatibility.md)). What that does not yet buy is
mileage. The coverage is broad rather than deep, most of it against a handful of small
local models on one machine, and the failure modes that only long uninterrupted runs,
unusual hardware, or an unfamiliar model turn up are still ahead of it. Expect rough
edges in that territory, and please open an issue when you hit one — that is the
testing this stage of the project most needs.

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
The fastest and recommended path is Docker:

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

[tools]                   # sandboxed tools, off unless enabled
enabled  = true
user_dir = "./tools.d"    # ships with a working csv_stats example

[llm]                     # model configuration
provider       = "ollama"
model          = "qwen3.5:4b"
endpoint       = ""              # empty uses Ollama's local default
num_ctx        = 32768
max_concurrent = 1
```

Start Nine
```bash
make up                # the daemon, one container
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
| Node.js | 18+ | Optional — only for `npx`-launched MCP servers |
| Docker | 24+ | Container build (one container, no compose) |
| golangci-lint | latest | Optional, for `make lint` |

Plus a running [Ollama](https://ollama.com) with a model pulled.

Sandboxed tools need nothing extra to run: the QuickJS interpreter they execute on is
committed to the repo as a pre-built wasm artifact with a recorded SHA-256, and the
wasm runtime and JS bundler are pure-Go libraries. Rebuilding that interpreter
(`make quickjs-wasm`) is a deliberate, separate step and the only thing that wants a
wasi-sdk; `make quickjs-verify` re-checks the committed hash.

### Build from source

```bash
git clone https://github.com/djordlucas/nine
cd nine
make all
```

**Nine ships as a single binary.** That build produces exactly one file, `dist/nine`,
and it is everything: the CLI, the TUI, the daemon, and the `shell`/`files`/`http`/`time`
plugins — the daemon starts each by re-executing itself as `nine plugin serve <name>`, so
they keep their own process and crash isolation without their own artifact. Deploying
Nine is copying one file.

Nothing else is built because nothing else needs to be: a capability Nine does not
implement itself is declared as an `[[mcp.server]]` and fetched or hosted elsewhere.
A browser is the worked example — see [docs/browser.md](docs/browser.md).

Nine looks for its config, in order: `$NINE_CONFIG`, `./nine.toml`, `/nine.toml`,
then `~/.nine/nine.toml`. The repo's `nine.toml` works as-is against a local Ollama;
the database is created on first run at `~/.nine/nine.db`.

### Docker (single container)

The deployment unit is **one container** running the daemon under s6-overlay
([docs/single-container.md](docs/single-container.md)). Its database is a file on
the `/data` volume, so there is no second service to orchestrate and no
docker-compose file; `docker run` is wrapped in Makefile targets:

```bash
make up                # built runtime image
make up-hot            # hot-reload: rebuilds and restarts the daemon on any .go change
make down              # stop, keeping all data
make destroy           # remove everything, including all data volumes and images
```

The runtime image holds the compiled binary, the plugins, and the built-in skills
— no Go toolchain, no Node, no npm, and no source tree, because Nine never builds
native code at runtime. That still holds with sandboxed tools in the picture: the
wasm interpreter they run on is a pre-built artifact committed to the repo, and the
JavaScript bundler used when a generated tool pulls in a dependency is a pure-Go
library compiled into the binary. Hot-reload mode bind-mounts the source and rebuilds
via `inotifywait` — this is the development path. Neither image ships a browser (the
dev image does carry Node for `npx` MCP servers), and `tools.d/` is mounted at
`/tools.d` — though the subsystem still needs
`[tools] enabled = true` in the `nine.toml` you mount, which no environment variable
can flip on.

The full Makefile target list is in [docs/installation.md](docs/installation.md).

## Configuration

Nine is configured through a single `nine.toml` — one file for every deployment. It is
written for the native layout, and the containers override the handful of values that
differ (LLM endpoint, database DSN, plugin and workspace paths) through environment
variables rather than a second config file. The essentials:

```toml
[llm]
provider       = "ollama"        # the only chat backend
model          = "qwen3.5:4b"
endpoint       = ""              # empty uses Ollama's local default
num_ctx        = 32768           # also sets the per-turn context budget
max_concurrent = 1               # keep at 1 for local models
thinking       = true            # stream reasoning as a live trace in the TUI

[daemon]
socket_path          = "/tmp/nine.sock"
task_timeout_seconds = 1800
max_goal_sessions    = 10        # concurrent background "pursue" sessions

[memory]
# The SQLite file holding every piece of durable state — primary storage, not a
# cache. Created on first run; defaults to ~/.nine/nine.db (/data/nine.db in the
# container).
# path = "~/.nine/nine.db"

[embeddings]
provider = ""                    # "" or "keyword" = built-in, no model, no network
                                 # "ollama" for better ranking; "none" disables

[planning]
plan_mode     = "plan-only"      # off | plan-only | always
plan_approval = "on-risky"       # off | on | on-risky

[tools]                          # sandboxed tools — off unless enabled
enabled   = true
user_dir  = "./tools.d"
timeout   = "5s"                 # wall clock is the only CPU bound
memory_mb = 16

# A manifest DECLARES a capability; only config GRANTS it, per named tool.
[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]

[tools.agent]                    # the tier Nine writes itself — separately off
enabled          = true
eval             = true          # allow js_eval (run a snippet, persist nothing)
max_tools        = 64            # catalog cap; least-recently-called are evicted
require_approval = "on_capability"  # on_capability | always | never

# The ceiling: the MOST any generated tool may be granted, never an automatic grant.
[tools.agent.capabilities.fs]
read = [{ host = "${NINE_WORKSPACE}", guest = "/workspace" }]
```

Environment variables override the file. The most useful:

| Variable | Description |
|----------|-------------|
| `NINE_CONFIG` | Explicit config path, skipping the search order |
| `NINE_LLM_PROVIDER` / `NINE_LLM_MODEL` / `NINE_LLM_ENDPOINT` | Override the LLM without editing config |
| `NINE_DB_PATH` | Override the database file path |
| `NINE_PLUGINS_BIN` / `NINE_WORKSPACE_ROOT` | Override the plugin and workspace paths (how the container reuses `nine.toml`) |
| `NINE_TOOLS_USER_DIR` | Override the sandboxed-tool directory. Deliberately the *only* tool override — whether the subsystem runs at all stays in `nine.toml` |
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
│  │  (Ollama)                        │               │
│  └──────────────────────────────────┘               │
│                                                     │
│  ┌────────────────────────────────────────────────┐ │
│  │  Tool Dispatcher                               │ │
│  │  ┌──────────────────┐  ┌─────────────────────┐ │ │
│  │  │ Plugin Manager   │  │ Sandboxed Tool Host │ │ │
│  │  │ shell files http │  │ wasm, in-process    │ │ │
│  │  │ time mcp:*       │  │ tools.d + generated │ │ │
│  │  │ (subprocesses,   │  │ (capabilities are   │ │ │
│  │  │  unix sockets)   │  │  conferred by cfg)  │ │ │
│  │  └──────────────────┘  └─────────────────────┘ │ │
│  └────────────────────────────────────────────────┘ │
│                                                     │
│  ┌────────────────────────────────────────────────┐ │
│  │  memory.Store  →  SQLite (one file)            │ │
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

**The tool dispatcher** is the single place a tool name resolves to an implementation,
and it has three backends behind one namespace: core-intercepted tools calling the
store in-process, plugin subprocesses, and the wasm sandbox host. A name resolves to
exactly one of them — a collision is a load failure, not a silent override.

**Memory** is one SQLite file, reached through a single store.
Schema is applied idempotently on open. Tables cover conversations, goals, workflows,
KV memory, full-text-searchable files, vectors, skills, generated tools, session plans,
human-in-the-loop state, and the event journal. Operational tables are daemon-private — never exposed to
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

The full treatment — topology, concurrency, the turn lifecycle, boot sequence, and the
invariants that hold it together — is in [docs/architecture.md](docs/architecture.md).

## Plugins

Nine ships four plugins — `shell`, `files`, `http`, and `time`. They are served out of
the `nine` binary itself, spawned by the daemon as child processes, and the built-in set
is fixed at build time. A crashing plugin is isolated from the daemon and from active
conversations.

Beyond those, an **MCP server** declared as an `[[mcp.server]]` becomes a plugin too —
its own process, its own roster row, tools prefixed with the server's name. That is how
Nine drives a browser: see [docs/browser.md](docs/browser.md).

Memory, durable file storage, semantic search, and skills are **not** plugins — they
are core-intercepted, wired into the agent loop and calling the store in-process.

Sandboxed tools (below) are a *second backend behind the same dispatcher*, not a
replacement: the plugin protocol, transport, and lifecycle are untouched, and a
deployment that enables no sandboxed tools behaves exactly as it did before they
existed.

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

Add the plugin to the repo and rebuild, or drop the built binary beside a manifest in
`[plugins].user_dir` (`plugins.d/weather` + `plugins.d/weather.toml`), which the daemon
discovers at boot and re-scans on `nine plugins reload`. Either way it is a binary you
compiled — no source is built at runtime. External MCP
servers are supported as an exception, speaking JSON-RPC 2.0 over stdio. See
[docs/plugins.md](docs/plugins.md) and
[docs/plugins-http-transport.md](docs/plugins-http-transport.md).

## Sandboxed tools

A plugin is a binary you build and ship. A **sandboxed tool** is two files you drop in
a directory: the daemon runs them in a wasm sandbox, in-process, with exactly the
capabilities the operator granted — by default, **none**. No subprocess, no compile
step, no image rebuild.

```text
tools.d/
  csvstats.toml     # the manifest — the gate
  csvstats.js       # the code
```

A `.js` or `.wasm` file with no manifest beside it is never loaded. The subsystem is
**off until you turn it on**:

```toml
[tools]
enabled  = true
user_dir = "./tools.d"
```

The code default-exports a function; return a string and it reaches the model
untouched, return anything else and it is JSON-stringified, throw and the model reads
your message as an ordinary tool failure and can retry.

```js
// csvstats.js
export default ({ csv }) => {
  const rows = csv.trim().split("\n").map((line) => line.split(","));
  return { rows: rows.length, columns: rows[0]?.length ?? 0 };
};
```

The manifest beside it is authoritative — `name`, `description`, and `input_schema` all
live there, so it is where the model's view of the tool comes from:

```toml
# csvstats.toml
name         = "csv_stats"
kind         = "js"              # "js" or "wasm"
entrypoint   = "./csvstats.js"
description  = "Summary statistics over a CSV string."
input_schema = "./csvstats.schema.json"
```

An unknown key is an error rather than a warning: in a file whose job is declaring
capabilities, a typo'd key silently meaning nothing is the worst outcome. The tool's
name shares one namespace with built-ins and plugin tools, and a collision skips your
tool rather than overriding theirs.

The `js` kind runs on a pre-supplied, trimmed QuickJS-NG interpreter compiled to wasm:
**ES2023 and nothing else** — no Node standard library, no `require`, no `setTimeout`
and no `URL`, and `fetch` only where `net.http` was granted. Bundle your dependencies
at development time (`npx esbuild … --bundle --format=esm --platform=neutral`); an
`import` left in a developer tool's file fails at call time. For another language or
full speed, ship a `.wasm` module directly from Rust, TinyGo, Zig, or C, exporting the
two-function ABI (`nine_alloc`, `nine_run`) that passes UTF-8 JSON in and out.

### Capabilities are conferred, never claimed

The manifest **declares a need**; only `nine.toml` **grants** it. The two must name the
same capabilities exactly — declaring something ungranted fails to load, and being
granted something you did not declare *also* fails to load. Both are loud by design.

```toml
# csvstats.toml — the tool declares a need
[capabilities]
fs = ["read"]
```

```toml
# nine.toml — the operator grants it, by name
[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]
```

Your code sees the **guest** path (`/data`), which is what lets an operator narrow or
move the mount without your tool changing.

| Capability | You get | Default |
|---|---|---|
| `clock`, `random`, `log` | `Date.now()`, `Math.random()`, `console.*` | granted |
| `fs.read` / `fs.write` | mounted directories, addressed by their *guest* path | declare + grant |
| `env` | named keys only (`NINE_*` and `*_API_KEY` can never be granted) | declare + grant |
| `net.http` | a `fetch` subset | declare + grant |

Everything with reach starts at nothing, and a capability is either a wazero pre-open
or a host function the daemon exports — anything else is not "denied", it is
structurally absent. A sandboxed tool cannot spawn a process, open a socket, load a
native library, or call another tool.

`net.http` is the one capability with no primitive under it — wazero has no network —
so it is a host function and its security is Nine's problem. The guest never touches a
socket and never learns an IP. Two independent gates must both pass: the hostname
matches the tool's `allow_hosts`, **and** the address actually being dialed is publicly
routable, checked immediately before connect so there is no window to re-resolve into.
Loopback, link-local (including `169.254.169.254`, where your cloud keeps its instance
credentials), and RFC 1918 are refused regardless of the allowlist, on every redirect
hop, and `Authorization`/`Cookie` are stripped across origins. There is no bare `"*"`:
an operator wanting unrestricted egress should write a plugin, where that intent is
explicit and reviewed.

Bounds are always on and orthogonal to capabilities: **one instance per call** (no
global, no cache, and no credential survives a call), a 5s wall clock, and 16 MiB —
the last two operator-tunable. wazero has no fuel metering, so the deadline is the only
CPU bound, which is a stated limitation rather than an assumption.

### The tier Nine writes itself

Nine can also write its own tools at runtime — the gap its `gap_report` names but
could not previously close. These are rows in the store rather than files on disk, but
they run in the identical sandbox under the identical rules. The tier is off by
default and independent of `[tools] enabled`; with it off, `tool_write`, `tool_delete`,
and `js_eval` are neither registered nor advertised, and a loop is identical to one
built before the tier existed:

```toml
[tools.agent]
enabled          = true
eval             = true             # allow js_eval — run a snippet, persist nothing
max_tools        = 64               # catalog cap; least-recently-called are evicted
require_approval = "on_capability"  # prompt a human only when a tool asks for reach

[tools.agent.capabilities.fs]       # the ceiling, not a grant
read = [{ host = "${NINE_WORKSPACE}", guest = "/workspace" }]
```

`[tools.agent.capabilities]` is a **ceiling**: the most any generated tool may be
granted, never an automatic grant. A tool gets a capability only by declaring it, one
that declares nothing runs with nothing, and declaring past the ceiling is a refusal
the model can act on. Declarations are re-resolved on every load, so narrowing the
ceiling disables a tool that no longer fits rather than leaving it running with reach
you withdrew.

This is the boundary, stated precisely: **the agent writes the code, the operator
writes the grants, and they are never the same actor.** `tool_write` writes JavaScript
and a capability *declaration* — it has no path to write a grant. Nine gains one
column and never the other:

| | Code | Capabilities |
|---|---|---|
| Native plugin | operator (build time) | operator (`nine.toml`) |
| Developer sandboxed tool | developer (file on disk) | operator (`nine.toml`) |
| Generated sandboxed tool | **Nine** (runtime) | operator (`nine.toml`) |

Generated tools may always import a small vendored standard library — `nine:csv`,
`nine:date`, `nine:diff` — embedded in the binary, no config and no network. External
npm packages are a separate and much riskier switch, off by default: when an operator
enables it and names the permitted packages, `tool_write` resolves the imports **once,
at write time, in the daemon**, verifies each tarball against its published checksum,
runs no install scripts, and inlines the result, so by call time the tool has no
imports left and no way to reach the network. A tool that both declares `net.http` and
pulls a dependency is refused unless the operator lifts an explicit interlock — a
networked dependency turns the sandbox into an exfiltration path.

A write can be gated on a human: `require_approval` defaults to `on_capability`, which
prompts only when the proposed tool declares reach, since prompting on a pure
computation trains the reflex that defeats the prompt that matters (`always` and
`never` are the other two, and gates apply to interactive sessions only). The catalog
is capped at `max_tools` with least-recently-called eviction — every generated tool
competes for the same tool-ranking budget, so an unbounded catalog would degrade
selection for the built-ins too.

Writes and deletes are journalled per session and additionally logged as an operator
breadcrumb; `js_eval` runs a snippet under the same rules and persists nothing, so
iteration does not accrete single-use tools. A new tool is visible **next turn** —
loops already in flight keep the tool set they started with.

### Seeing what happened

```console
$ nine tools
  ok    csv_stats          js     fs.read /srv/data=>/data
  ok    weather            js     net.http GET api.weather.example
  ok    due_date           gen    none
  SKIP  scraper            js     capability fs.read is declared by the tool but not granted; add it
                                  under [tool.<name>.capabilities] or remove the declaration
```

A tool that fails to load is always reported with its reason — that is the point of the
surface, and the `gen` column marks a tool as Nine's own rather than yours.
`nine tools show <name>` prints one in full — provenance, resolved grant, manifest path
or store origin, dependencies; `nine tools deps` answers "what third-party code is in
this daemon, and which tool pulled it in"; `nine tools reload` re-scans the directory
live; and `nine tool validate [path]` checks a manifest, entrypoint, schema, and ABI
exports with no daemon running.

The authoring guide is [docs/writing-sandboxed-tools.md](docs/writing-sandboxed-tools.md);
the design rationale and the capability model in full are in
[docs/sandboxed-tools.md](docs/sandboxed-tools.md), with the normative contract in
`spec/contracts/toolvm.md` (`nine spec toolvm`).

## Skills

Skills are how Nine improves what it *knows*; generated tools, where an operator turned
that tier on, are how it improves what it can *do*. A skill is a markdown how-to note
with YAML frontmatter, stored in the database:

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

The boundary is deliberate: Nine writes skills, and sandboxed-tool code where the
operator enabled that tier — both of which are store state, listable and deletable
like a goal or a workflow. It does not generate plugins, write itself a capability
grant, rewrite its config, or rebuild its source at runtime. Its executable shape is
fixed. The reasoning is in
[docs/self-modification.md](docs/self-modification.md).

## Key concepts

| Concept | Description |
|---------|-------------|
| **Daemon** | Long-running background process; manages agents, holds plugin state |
| **Agent** | A conversation thread running the ReAct loop |
| **Plugin** | Standalone binary exposing tools over HTTP/Unix socket; compiled ahead of time, never at runtime |
| **Sandboxed tool** | JS or wasm run in-process in a wasm sandbox, with only the capabilities config granted it |
| **Generated tool** | A sandboxed tool Nine wrote itself, stored as a row; its code is the agent's, its capabilities the operator's |
| **Capability** | A conferred reach — `fs`, `env`, `net.http` — declared by a tool's manifest and granted only in `nine.toml` |
| **Skill** | Markdown how-to note, semantically retrieved into context |
| **Goal** | An open-ended intention with no end condition, pursued in the background |
| **Workflow** | A finite multi-step plan for sub-agent delegation |
| **Session plan** | The stages and idle schedule that let a session wake and take its own next turn |
| **Standing agent** | A goal declared in `nine.toml`; runs from boot on a cron schedule, no human turn needed |
| **Supervisor** | Special agent that monitors others for stalls and capability gaps |
| **Checkpoint** | Serialized agent state persisted to the database |
| **Journal** | Append-only record of every step, enabling trace and deterministic replay |

## Documentation

[docs/README.md](docs/README.md) is the full guide, ordered for a first-time reader.
Highlights:

- [CLI usage](docs/usage.md) — commands, TUI, slash commands, background tasks
- [Agent loop](docs/agent-loop.md) and [context builder](docs/context-builder.md)
- [Event log](docs/event-log.md) and [reactive events](docs/reactive-events.md)
- [Session plans & stages](docs/session-plans.md) — idle scheduling, reflection, goal pursuit
- [Roles](docs/roles.md) — role-gated tool allowlists, delegation, depth guards
- [Writing sandboxed tools](docs/writing-sandboxed-tools.md) and the
  [design behind them](docs/sandboxed-tools.md) — the wasm host and its capability model
- [Predefined agents](docs/predefined-agents.md) and [scheduling](docs/scheduling.md)
- [Human-in-the-loop](docs/hitl.md) — `ask_human` and approval gates
- [Glossary](docs/glossary.md) — every concept in one place

## Contributing

**Issues yes, pull requests no.** Bug reports, questions, and ideas are genuinely
welcome — please open an issue. Pull requests won't be merged; this is a personal
project developed solo, and keeping it single-author is a deliberate choice.

## Security

**Sandboxed tools are sandboxed; nothing else is.** The wasm host is a real boundary —
default-deny capabilities, one instance per call, an SSRF-checked HTTP path — and it
applies to sandboxed tools only. Everything around it is unchanged: the `shell` plugin
runs commands as the daemon's process user, plugins are ordinary subprocesses with the
daemon's own reach, and the agent has real filesystem and network access through them.
Run Nine in Docker or under a restricted user if you are pointing it at anything you
don't trust.

Two settings deserve a deliberate decision rather than a default. Enabling
`[tools.agent]` lets the agent write code that then runs — bounded by the ceiling you
confer, which is worth narrowing if your workspace holds secrets. Enabling external npm
dependencies for that tier is the riskiest switch in the system; leave it off unless you
have a reason, and leave the `net.http` interlock in place if you turn it on.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).

Copyright (C) 2026 The Nine Authors

This program is free software: you can redistribute it and/or modify it under the
terms of the GNU General Public License as published by the Free Software Foundation,
either version 3 of the License, or (at your option) any later version. It is
distributed in the hope that it will be useful, but WITHOUT ANY WARRANTY; without even
the implied warranty of MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.
