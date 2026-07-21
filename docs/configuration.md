# Configuration

Nine is configured via a single TOML file. Nine looks for the config file in this order:

1. `$NINE_CONFIG` — explicit path via environment variable
2. `./nine.toml` — current working directory
3. `/nine.toml` — Docker bind-mount location (see Docker setup)
4. `~/.nine/nine.toml`

The first file found wins. If none is found, Nine starts with default (zero) values.

There is one `nine.toml` for every deployment. It is written for the native layout
(local Ollama, the compose Postgres on its host-published port, plugins in
`./dist/bin`), and the containers override the four values that differ —
`NINE_LLM_ENDPOINT`, `NINE_DATABASE_URL`, `NINE_PLUGINS_BIN`, `NINE_WORKSPACE_ROOT` —
rather than shipping a second file. See [Environment Variables](#environment-variables).

---

## Full Reference

```toml
[llm]
# Chat LLM backend to use. Implemented options: ollama (default), anthropic.
# These are the only two; any other value falls through to the Anthropic client.
provider = "ollama"

# Model name. Examples:
#   ollama:    gemma4:e2b, qwen3.5:9b, llama3.2, mistral
#   anthropic: claude-sonnet-4-6, claude-opus-4-7, claude-haiku-4-5-20251001
model = "gemma4:e2b"

# API key for anthropic. Leave empty to read from ANTHROPIC_API_KEY.
api_key = ""

# Base URL for the API. Required for ollama (e.g. http://localhost:11434).
# Leave empty for anthropic to use its default endpoint.
endpoint = ""

# Context window size. For Ollama this is passed as num_ctx and also sets the
# context budget (how many tokens the context builder may use per turn).
# For cloud providers, set context_budget instead.
num_ctx = 32768

# Maximum concurrent LLM requests.
# Set to 1 for local models (Ollama) to prevent contention.
# Increase for cloud providers if your tier supports parallel requests.
max_concurrent = 1

# Stream the model's extended-thinking reasoning as a live trace in the TUI
# (Ollama only for now: sends think:true and drops the /no_think suppression;
# the model must advertise the "thinking" capability). Defaults to true; set to
# false to suppress thinking for lower latency.
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
# Plugin sources, and the directory holding the compiled plugin binaries.
# `make all` writes them to ./dist/bin; in Docker they are baked into the image
# at /opt/nine/bin, which the container sets via NINE_PLUGINS_BIN.
dir = "./plugins"
bin = "./dist/bin"


[memory]
# PostgreSQL connection string. Postgres holds all persistent state:
# conversations, tasks, goals, KV memory, file cache (full-text searchable via
# tsvector), vectors (pgvector), skills, and the session event journal. (Built-in
# skills are embedded in the binary and seeded into the skills table on boot —
# there is no skills dir.) The daemon fails fast if the database is unreachable.
# Overridable with the NINE_DATABASE_URL environment variable. Start a local
# instance with `docker compose up -d`.
database_url = "postgres://nine:nine@localhost:5433/nine?sslmode=disable"

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
# Defaults to true (shown) when unset.
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
```

---

## LLM Providers

Two chat providers are implemented: `ollama` and `anthropic`. There are no others —
any other value falls through to the Anthropic client.

### Ollama (Local, default)

```toml
[llm]
provider       = "ollama"
model          = "gemma4:e2b"
endpoint       = ""              # empty uses Ollama's local default
num_ctx        = 32768
max_concurrent = 1
```

Ollama must be running before starting Nine. Pull the model first:

```bash
ollama pull gemma4:e2b
```

### Anthropic

```toml
[llm]
provider = "anthropic"
model    = "claude-sonnet-4-6"
api_key  = ""   # reads ANTHROPIC_API_KEY
```

Models:
- `claude-haiku-4-5-20251001` — fastest, lowest cost
- `claude-sonnet-4-6` — balanced
- `claude-opus-4-7` — most capable, highest cost

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
| `NINE_DATABASE_URL` | Override `memory.database_url` |
| `NINE_PLUGINS_BIN` | Override `plugins.bin` |
| `NINE_WORKSPACE_ROOT` | Override `workspace.root` |
| `NINE_SKILLS_USER_DIR` | Override `skills.user_dir` |
| `ANTHROPIC_API_KEY` | Anthropic API key (used when `llm.api_key` is empty) |
| `SEARCH_PROVIDER` | `web_search` backend: `brave` or `serpapi`. Unset uses DuckDuckGo, which needs no key. |
| `SEARCH_API_KEY` | API key for the chosen `SEARCH_PROVIDER` |
| `NINE_LOG_LEVEL` | Logging verbosity: `debug`, `info`, `warn`, `error` |
| `NINE_LOG_FORMAT` | Log format: `text` (default) or `json` |
| `NINE_LOG_FILE` | Set to `off` to disable file logging (logs go to stderr only) |

These are how one `nine.toml` serves every deployment. `docker-compose.yml` sets
`NINE_DATABASE_URL`, `NINE_PLUGINS_BIN`, and `NINE_WORKSPACE_ROOT` to point each
container at its own layout, and the `NINE_LLM_*` knobs — passed through by the
Makefile's compose targets — let you switch models without editing the file:

```bash
NINE_LLM_MODEL=llama3.2 make compose-prod
```

---

## Changing Configuration

Configuration is owned by the operator, not the agent: Nine cannot modify `nine.toml`
at runtime. To change a setting, edit the file and restart the daemon so it re-reads
the config on startup. (`NINE_LLM_*` environment variables let you override the LLM
provider/model/endpoint at launch without editing the file — convenient in Docker.)
