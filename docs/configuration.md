# Configuration

Nine is configured via a single TOML file. Nine looks for the config file in this order:

1. `$NINE_CONFIG` — explicit path via environment variable
2. `./nine.toml` — current working directory
3. `/nine.toml` — Docker bind-mount location (see Docker setup)
4. `~/.nine/nine.toml`

The first file found wins. If none is found, Nine starts with default (zero) values.

There is one `nine.toml` for every deployment. It is written for the native layout
(local Ollama, plugins in `./dist/bin`), and the container overrides the values that
differ — `NINE_LLM_ENDPOINT`, `NINE_PLUGINS_BIN`, `NINE_WORKSPACE_ROOT` — rather than
shipping a second file. The database path needs no override there: it defaults to
`/data/nine.db` whenever the container's `/data` volume is present
([Single-container Nine](single-container.md)). See [Environment Variables](#environment-variables).

---

## Full Reference

```toml
[llm]
# Chat LLM backend to use. Ollama is the only one: Nine runs on local models.
# Any other value is reported at boot and Ollama is used anyway.
provider = "ollama"

# Model name. Examples: qwen3.5:4b, qwen3.5:9b, gemma4:e2b, llama3.2
model = "qwen3.5:4b"

# Base URL for the Ollama API. Empty uses http://localhost:11434.
endpoint = ""

# Context window size. Passed to Ollama as num_ctx, and it also sets the context
# budget (how many tokens the context builder may use per turn) unless
# context_budget overrides it.
num_ctx = 32768

# Maximum concurrent LLM requests. Keep it at 1 for a single local model, which
# serializes on the GPU anyway; raise it only if you point Nine at an Ollama
# host that can genuinely serve calls in parallel.
max_concurrent = 1

# HTTP timeout for a single model call, in seconds. 0 uses the adapter default
# (300s); a negative value removes the timeout, leaving only turn cancellation.
timeout_seconds = 0

# Stream the model's extended-thinking reasoning as a live trace in the TUI
# (sends think:true and drops the /no_think suppression; the model must
# advertise the "thinking" capability). Defaults to true; set to false to
# suppress thinking for lower latency.
thinking = true


[daemon]
# Path to the Unix domain socket.
# The CLI and daemon must agree on this path.
socket_path = "/tmp/nine.sock"

# How long a background task may run before it's considered timed out.
# Defaults to 1800 (30 minutes) when unset or <= 0.
task_timeout_seconds = 1800

# Maximum number of concurrently-running background "pursue" sessions
# (one per top-level goal). Defaults to 10 when unset or <= 0.
max_goal_sessions = 10

# Display name for this Nine instance, shown in the TUI top bar (replacing the
# literal "nine"). When set, it is authoritative and fixed. When unset, the
# daemon reuses a name it generated on a prior boot (persisted in the store), or
# — on a first boot with none — shows "nine" as a placeholder and asks the LLM
# to coin a short random name asynchronously, persists it, and pushes it live to
# connected clients. If the LLM is unavailable it falls back to a random
# "adjective-noun" name. Once generated, the name stays stable across restarts.
# instance_name = "atlas"

# Treat the [[agent]] list as the full desired state for pre-defined agents
# (docs/predefined-agents.md §7 v3). When true, a config-origin goal no longer
# listed in [[agent]] is archived and its session stopped on boot;
# conversation-created goals are never touched. Defaults to false — removing an
# entry just stops reconciling it, leaving the goal for manual archival.
standing_agents_authoritative = false

# Session event journal retention (docs/event-log.md), applied by a boot-time
# scrub. Keep the last N turns per agent; 0 uses the built-in default, negative
# keeps all turns. event_retention_days additionally drops events older than N
# days (0 = no age limit).
# event_retention_turns = 0
# event_retention_days  = 0

# Link topically-similar sessions out-of-band and surface a related prior session
# on a later turn (docs/reactive-events.md). On by default; requires an embedder
# (a no-op when [embeddings] is disabled). Set false to disable.
# related_sessions_index = false


# Pre-defined, long-running agents (docs/predefined-agents.md). Each [[agent]]
# is seeded at boot as a config-owned goal running under the pursue shell with a
# narrowed role — no human turn needed to bring it to life. `id` is a stable,
# operator-chosen key; edit the definition and restart to reconcile in place.
# Findings surface via `nine notifications`.
#
# [[agent]]
# id          = "sec-watch"
# description = "Monitor this repo for security issues; triage new CVEs affecting our deps."
# role        = "monitor"        # optional; default "monitor" (read-only). Narrows work tools only.
# delegates   = false            # optional; default false. Opt into sub-agent fan-out.
# schedule    = "0 9 * * 1-5"    # cron (5-field, docs/scheduling.md) …XOR… interval = "24h"


[plugins]
# The directory holding plugins that ship as their own binaries — today just
# `browser`. The Go built-ins (shell/files/http/time) are compiled into the nine
# binary and started as `nine plugin serve <name>`, so they are not looked up
# here at all. `make all` writes browser to ./dist/bin; in Docker it is baked
# into the image at /opt/nine/bin, which the container sets via
# NINE_PLUGINS_BIN. Operator-supplied plugins use `user_dir` below, not this.
bin = "./dist/bin"
# Plugins that must never start, by name. This is how a capability is withheld —
# most obviously `shell`, which runs arbitrary commands. It applies to built-ins,
# to browser, and to user plugins alike, and a disabled plugin is reported as
# `off` by `nine plugins` rather than being silently absent. Overridable with
# NINE_PLUGINS_DISABLED="shell,browser" so a container needs no second config.
# disabled = ["shell"]
# Your own plugins, discovered at boot from a sidecar-manifest layout — an
# executable beside a <name>.toml (name + entrypoint). Scanned separately from
# the built-in bin dir and purely additive; unset or absent disables it. A
# non-plugin binary, or one whose tools collide with a loaded plugin, is skipped
# and surfaced — the daemon still starts. See docs/plugins.md and plugins.d/.
# The container overrides this with NINE_PLUGINS_USER_DIR.
# user_dir = "./plugins.d"
# Root for each plugin's scratch/cache directory (docs/plugin-capabilities.md §4).
# Defaults to the OS user cache dir (~/.cache/nine/plugins); the container
# overrides it with NINE_PLUGINS_CACHE_DIR. Must be durable, not /tmp.
# cache_dir = "~/.cache/nine/plugins"
# Long-running plugin jobs (docs/plugin-capabilities.md §5): how often the daemon
# polls a running job (default 2s), the per-job lifetime bound (default 3600s),
# and the per-conversation cap on outstanding jobs (default 8).
# job_poll_seconds = 2
# job_max_seconds = 3600
# max_jobs_per_conversation = 8

# Per-plugin operator config (docs/plugin-capabilities.md §3). The singular
# [plugin.<name>] table (sibling to the plural [plugins] above) configures one
# plugin. [plugin.<name>.settings] is a schema-less bag of environment variables
# passed through verbatim at spawn — so a plugin Nine was not built to know about
# (a weather plugin needing an API key) can be configured without a rebuild. Keys
# are used as env-var names; values are TOML scalars. persist_cache keeps the
# plugin's cache dir across restarts (default false).
# [plugin.weather]
# persist_cache = false
# [plugin.weather.settings]
# WEATHER_API_KEY = "sk-…"
# UNITS = "metric"
# [plugin.browser.settings]
# BROWSER_HEADLESS = "0"   # override a built-in default


# MCP servers. Each entry becomes one plugin (`mcp:<name>`) with its tools
# prefixed by the server name, so two servers cannot collide. Disable one with
# [plugins] disabled = ["mcp:github"]. stdio servers only.
# [[mcp.server]]
# name    = "github"
# command = "npx"
# args    = ["-y", "@modelcontextprotocol/server-github"]
# [mcp.server.env]
# GITHUB_PERSONAL_ACCESS_TOKEN = "ghp_..."

[memory]
# SQLite database file. It holds all persistent state: conversations, goals, KV
# memory, file cache (full-text searchable via FTS5), vectors, skills, and the
# session event journal. (Built-in skills are embedded in the binary and seeded
# into the skills table on boot — there is no skills dir.) The file and its parent
# directory are created on first run; the daemon fails fast if it cannot be opened.
# Defaults to /data/nine.db when the container's /data volume is present, else
# ~/.nine/nine.db. Overridable with the NINE_DB_PATH environment variable.
# path = "~/.nine/nine.db"

# Memory surfacing: mirror every memory_set into a shared vector pool and, on
# later turns, inject the stored memories most relevant to the current query as
# advisory enrichment (the same small, drop-when-tight band as related-session
# surfacing). On by default; requires an embedder (no-op when embeddings are
# disabled). Set false to disable both the indexing and the surfacing.
# surface_memories = false


[embeddings]
# Embeddings backend.
# Options:
#   ollama   — use a locally-running Ollama model (good quality, requires VRAM)
#   keyword  — built-in feature-hashing embedder (no model, no network)
#   none     — disable tool ranking entirely (all tools included every turn)
# Leave empty ("") to fall back to "keyword" — this is the default.
provider = ""

# Embedding model name (ollama): nomic-embed-text, mxbai-embed-large
model = "nomic-embed-text"

# Endpoint for the embeddings API. Only used when provider = "ollama"; empty
# uses Ollama's local default. In Docker: http://host.docker.internal:11434
endpoint = ""

api_key = ""


[ui]
# Color theme. Options: auto (detect from terminal), light, dark
theme = "auto"

# Show context token usage in the TUI header.
# Defaults to true (shown) when unset. When usage crosses the trim threshold
# (90% of the budget) the header shows a "⚠ ctx: used/budget (NN%)" warning even
# if this is false — a warning overrides the opt-out of the routine readout.
show_context = true


[workspace]
# Working directory for file operations. Optional.
# root = "./workspace"

[skills]
# Directory holding your own skills and roles, seeded on every boot alongside
# the built-ins (docs/skills.md). Layout mirrors the built-ins: *.md at the top
# level, role skills under roles/. The directory is the source of truth — edit
# a file and restart to update it, delete it to remove it. Discovery is
# boot-only; there is no watcher. Invalid files are skipped with a logged
# reason and the daemon still starts; check them with `nine skills validate`.
# Unset or missing disables user skills entirely.
# user_dir = "./skills.d"

[hitl]
# Human-in-the-loop (docs/hitl.md). Interactive TUI conversations only.
#
# timeout_seconds — how long a question waits for an answer before it fails the
# call and lets the model move on. Default 300 (5 min).
timeout_seconds = 300
#
# require_approval — tools that need an explicit "yes" before each call. The
# prompt is tool-aware (shell shows its command, write_file its path). A refusal
# fails that call as a normal tool error and is not retried.
require_approval = []
#
# gate_sub_agents — whether those gates also cover sub-agents spawned by an
# interactive conversation. Default true: without it, delegating a gated tool
# to a sub-agent runs it unprompted, which makes require_approval trivially
# bypassable. The prompt appears on the conversation's own stream, attributed to
# the sub-agent. Sub-agents never get ask_human either way, and a non-interactive
# session's sub-agents are never gated — no human is attached to ask.
gate_sub_agents = true

[planning]
# Plan-before-execute policy (docs/thinking-and-planning.md). Both keys have a
# live per-session override via the TUI /plan-mode command.
#
# plan_mode — how the agent reasons before acting:
#   off        no native thinking and no request-analysis pass
#   plan-only  (default) reason on the first inner call, then execute
#   always     request native thinking on every inner call
plan_mode = "plan-only"
#
# plan_approval — interactive checkpoint before a plan executes (analysis-pass
# branch only; interactive sessions only):
#   off        never prompt
#   on         always prompt
#   on-risky   (default) prompt only when the plan intends a tool listed in
#              [hitl].require_approval — one source of truth, no second list
plan_approval = "on-risky"

[tools]
# max_output_tokens — the per-result tool output cap (docs/tool-output-spill.md).
# A result larger than this is written whole to the memory file store under
# spill/<agent-id>/ and replaced in context by a short preview naming the path,
# which the agent can read back with file_fetch(offset, limit) or search with
# file_search_text(query, path). Nothing is discarded, so raising this is rarely
# necessary — do it only when models should routinely see more of a large result
# inline. Default 2048 (~8192 characters).
#
# It is [tools] rather than [agent] because [[agent]] is already the
# standing-agent table array.
max_output_tokens = 2048

# ── Sandboxed tools ──────────────────────────────────────────────────────────
# A wasm tool host: JavaScript or .wasm tools an operator installs as two files,
# run in-process with an explicitly conferred capability set. OFF by default —
# leaving `enabled` unset means no host, no tools, and agent loops identical to
# what they were before this subsystem existed.
enabled = false

# Where developer tools live: a `<name>.js` or `<name>.wasm` beside a
# `<name>.toml` manifest, the same sidecar layout [plugins].user_dir uses. A file
# with no manifest beside it is never loaded. Unset loads nothing.
user_dir = "/etc/nine/tools.d"

# Per-call wall clock. This is the ONLY CPU bound the host has — wazero offers no
# fuel metering — so a spinning tool is killed at the deadline rather than by a
# work budget. Default 5s.
timeout = "5s"

# Per-call linear memory cap. Default 16.
memory_mb = 16

# ── Capability grants, per named tool ────────────────────────────────────────
# `[tool.<name>]` (singular) is the grant half of the capability model, sibling
# to the plural `[tools]` above — the same split `[plugin.<name>]` uses.
#
# A tool's manifest DECLARES what it needs; only this GRANTS. The two must name
# the same capabilities or the tool fails to load with a named error that
# `nine tools` reports. The default for everything with reach — filesystem,
# network, environment — is nothing. Clock, randomness, and logging are always
# granted; they leak nothing.
#
# There is deliberately no wildcard `[tool."*"]`: an operator granting filesystem
# access does so to a tool they have read.

[tool.csv_stats.capabilities.fs]
# Host paths must be absolute. The guest path is what the tool's own code sees,
# which is what makes narrowing a mount a one-line change.
read  = [{ host = "/srv/data", guest = "/data" }]
# write = [{ host = "/srv/out", guest = "/out" }]

[tool.tz_aware.capabilities]
# An explicit key allowlist, never all-or-nothing: the daemon's environment holds
# the LLM provider API keys. Keys matching NINE_* or *_API_KEY are refused
# outright as a config error.
env = ["TZ"]

# Outbound HTTP. The guest never touches a socket: it asks the daemon, which
# makes the request. Two independent gates both have to pass — the hostname must
# match allow_hosts, AND the IP actually dialed must be publicly routable.
#
# The second gate is not configurable and not subject to allow_hosts. Loopback,
# link-local (169.254.0.0/16 — where every cloud serves instance credentials),
# RFC1918, IPv6 ULA, and the other non-routable ranges are refused whatever a
# hostname resolves to. That is what stops DNS rebinding: the check runs on the
# address, immediately before connect, and again on every redirect hop.
#
# There is no bare "*". If you want a tool with unrestricted egress, write a
# native plugin — where that intent is explicit and gets reviewed.
[tool.weather.capabilities.net.http]
allow_hosts = ["api.weather.example", "*.cdn.weather.example"]  # exact, or a
                                                    # leading "*." (not the apex)
methods     = ["GET"]        # required; no implicit default
max_bytes   = 1048576        # response cap; 0 uses 1 MiB

# ── Generated tools: the tier Nine writes itself ─────────────────────────────
# OFF by default and independent of [tools] enabled above — an operator may want developer
# tools without letting the agent author any. When on, the agent gets tool_write/tool_delete
# (and js_eval, separately switched); the tools it writes are rows in the store, listable and
# deletable, and run under exactly the same sandbox and bounds as a developer tool.
[tools.agent]
enabled          = false            # turns on tool_write / tool_delete
eval             = true             # additionally allow js_eval — run a snippet, persist nothing
max_tools        = 64               # catalog cap; least-recently-called tools are evicted past it

# When a write/eval routes through the human approval gate (docs/hitl.md):
#   on_capability (default) — only when the tool DECLARES reach; a pure transform passes silently
#   always                  — every write and every eval
#   never                   — the ceiling below is the only control
# The default gates on substance, not frequency: a prompt that fires on every trivial tool is a
# prompt that gets approved without reading. Interactive sessions only — a non-interactive
# deployment has no gate, so there the ceiling is everything.
require_approval = "on_capability"

# The CEILING — the MAXIMUM a generated tool may be granted, never an automatic grant. A tool
# that declares nothing gets nothing, however permissive this is; a tool cannot declare its way
# past it. Same shape as [tool.<name>.capabilities]. Narrowing it retroactively disables a tool
# that no longer fits, on the next load. Omit it entirely to keep every generated tool inert.
[tools.agent.capabilities.fs]
read = [{ host = "${NINE_WORKSPACE}", guest = "/workspace" }]  # narrow to a subdirectory if the
                                                    # workspace holds secrets

# ── The nine:* stdlib and external npm dependencies ──────────────────────────
# A generated tool may always `import` the curated nine:* stdlib — nine:csv, nine:date,
# nine:diff — served from the binary, no config needed. External npm packages are the single
# riskiest switch in the design and are OFF by default. When enabled, Nine resolves them once,
# in the daemon, at tool_write time (never at call time), verifies each tarball's sha512, runs
# no install scripts, and bundles everything into the tool's source with esbuild in-process —
# so a called tool has no imports but nine:* and no network. Resolved packages are cached under
# [tools].cache_dir (default os.UserCacheDir()/nine/tools) and listed by `nine tools deps`.
[tools.agent.deps]
mode          = "off"               # off (default) | allowlist (named packages, transitive
                                    #   included) | open (anything within budgets — dev posture)
registry      = ""                  # npm-compatible base URL; empty = the public registry
frozen        = false               # resolve only from cache/lockfile, never the network
max_packages  = 24                  # budgets: total incl. transitive / bundle KB / tree depth
max_bundle_kb = 2048
max_depth     = 4
allow = [                           # allowlist mode only: the packages an operator stands behind
  { name = "date-fns", version = "^4.1.0" },
]

# The interlock: a tool that BOTH declares net.http AND pulls an external dependency is refused,
# because a package that can reach the network can exfiltrate whatever the tool sees. Lifting it
# is the one combination that makes a supply-chain compromise materially dangerous — leave it off
# unless you understand exactly why you need it.
# [tools.agent] allow_network_deps = false
```

---

## LLM Providers

`ollama` is the only chat provider. Nine is built for local models, so there is
nothing to choose between: any other `provider` value is logged as unknown at
boot and the Ollama adapter is used anyway (Nine still starts).

### Ollama

```toml
[llm]
provider       = "ollama"
model          = "qwen3.5:4b"
endpoint       = ""              # empty uses Ollama's local default
num_ctx        = 32768
max_concurrent = 1
```

Ollama must be running before starting Nine. Pull the model first:

```bash
ollama pull qwen3.5:4b
```

`timeout_seconds` bounds a single model call. It defaults to 300s, which is
generous enough for a large model on CPU; set a negative value to remove the
bound entirely and rely on turn cancellation alone. Which models actually drive
the agent loop well is recorded in [Model compatibility](model-compatibility.md).

---

## Embeddings

Embeddings power two features:

1. **Semantic tool selection** — Only the most relevant tools are included in each LLM turn, preserving context budget.
2. **Semantic file search** — `file_search_semantic` finds files by meaning, not just exact text.

The default (`keyword`) uses a built-in feature-hashing embedder that requires no model and no network access. Switch to `ollama` for higher quality ranking.

### Built-in Keyword Embedder (Default)

```toml
[embeddings]
provider = ""   # or "keyword" — both select the built-in embedder
```

No configuration needed. Suitable for most deployments.

### Using Ollama for Embeddings

```toml
[embeddings]
provider = "ollama"
model    = "nomic-embed-text"
endpoint = ""   # empty uses Ollama's local default
```

Pull the model on the host:

```bash
ollama pull nomic-embed-text
```

---

## Environment Variables

Environment variables take priority over `nine.toml` values.

| Variable | Description |
|----------|-------------|
| `NINE_CONFIG` | Explicit path to `nine.toml` (skips the default search order) |
| `NINE_LLM_PROVIDER` | Override `llm.provider` |
| `NINE_LLM_MODEL` | Override `llm.model` |
| `NINE_LLM_ENDPOINT` | Override `llm.endpoint` |
| `NINE_EMBED_PROVIDER` | Override `embeddings.provider` |
| `NINE_DB_PATH` | Override `memory.path` |
| `NINE_PLUGINS_BIN` | Override `plugins.bin` |
| `NINE_PLUGINS_USER_DIR` | Override `plugins.user_dir` |
| `NINE_PLUGINS_CACHE_DIR` | Override `plugins.cache_dir` (the plugin cache-dir root) |
| `NINE_PLUGINS_DISABLED` | Override `plugins.disabled` — comma-separated plugin names that must never start, e.g. `shell,browser`. Replaces the file's list rather than adding to it. |
| `NINE_WORKSPACE_ROOT` | Override `workspace.root` |
| `NINE_SKILLS_USER_DIR` | Override `skills.user_dir` |
| `NINE_TOOLS_USER_DIR` | Override `tools.user_dir` (sandboxed tools). Only the path — `[tools] enabled` is deliberately not env-overridable, so a stray variable cannot switch the subsystem on. |
| `SEARCH_PROVIDER` | `web_search` backend: `brave` or `serpapi`. Unset uses DuckDuckGo, which needs no key. |
| `SEARCH_API_KEY` | API key for the chosen `SEARCH_PROVIDER` |
| `NINE_LOG_LEVEL` | Logging verbosity: `debug`, `info`, `warn`, `error` |
| `NINE_LOG_FORMAT` | Log format: `text` (default) or `json` |
| `NINE_LOG_FILE` | Set to `off` to disable file logging (logs go to stderr only) |

Logs default to `nine.log` beside the binary, falling back to stderr when that
file cannot be opened (an installed binary in a read-only directory) as well as
when `NINE_LOG_FILE=off` asks for it — which is what the container sets, so
`docker logs` carries everything.

Two places deliberately keep those logs off an interactive terminal, because the
TUI draws a full-screen UI on it and a stray log line lands in the chat area:
the TUI discards client-side logging for its own lifetime when the destination
is stderr, and a daemon auto-started by a client (`EnsureDaemon`) never inherits
the caller's stdout/stderr. Run `nine daemon` yourself to watch a daemon's
output live.

These are how one `nine.toml` serves every deployment. The Makefile's `up`/`up-hot`
targets set `NINE_PLUGINS_BIN` and `NINE_WORKSPACE_ROOT` to point the container at
its own layout (the database path needs no override — it defaults to `/data/nine.db`
when that volume is present), and the `NINE_LLM_*` knobs — passed
through by those same targets — let you switch models without editing the file:

```bash
NINE_LLM_MODEL=llama3.2 make up
```

---

## Changing Configuration

Configuration is owned by the operator, not the agent: Nine cannot modify `nine.toml`
at runtime. To change a setting, edit the file and restart the daemon so it re-reads
the config on startup. (`NINE_LLM_*` environment variables let you override the LLM
provider/model/endpoint at launch without editing the file — convenient in Docker.)
