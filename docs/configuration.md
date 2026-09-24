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
([Single-container Nine](../adr/single-container.md)). See [Environment Variables](#environment-variables).

---

## Full reference

```toml
[llm]
# Chat LLM backend to use. Ollama is the only one: Nine runs on local models.
# Leave it unset for the default; any other value is refused at startup rather
# than quietly served by a different backend.
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

# Delete a session — and everything keyed to it — after this many days without
# activity. Unset uses 10; 0 switches automatic deletion off entirely.
#
# This is the one setting in Nine that destroys history rather than bounding it,
# so it is worth being precise about what it will and will not take:
#
#   * Age is from LAST ACTIVITY, not creation. A conversation you have kept for
#     a year and used this morning is never stale.
#   * A session whose id matches an ACTIVE GOAL is never taken — a goal's pursue
#     session is named after its goal, so this is exact.
#   * A session carrying an ACTIVE SESSION PLAN is never taken. That covers
#     standing agents declared below: one that wakes weekly looks abandoned
#     after ten days precisely because it is working.
#
# `nine sessions` marks a protected session "kept", so an old one that is not
# being reaped explains itself. Every deletion is logged with what it removed.
# session_retention_days = 10

# Display name for this Nine instance, shown in the TUI top bar (replacing the
# literal "nine"). When set, it is authoritative and fixed. When unset, the
# daemon reuses a name it generated on a prior boot (persisted in the store), or
# — on a first boot with none — shows "nine" as a placeholder and asks the LLM
# to coin a short random name asynchronously, persists it, and pushes it live to
# connected clients. If the LLM is unavailable it falls back to a random
# "adjective-noun" name. Once generated, the name stays stable across restarts.
# instance_name = "atlas"

# The dedicated self-reflection session: a Go duration for its cadence, or "off"
# to remove it. Unset ships enabled at 2m — reflection is how Nine maintains its
# own self-model, so it is on by default.
#
# Removal is subtractive, not merely "stop creating it": an existing session is
# deactivated on the next boot, so turning reflection off takes effect on a
# machine that has already been running it. The plan row is kept rather than
# deleted, so `nine reflections` still reads its history back from the journal.
# self_reflection = "off"

# Where the daemon reports itself as running, shown to the model in the
# self-model's Environment block. Unset auto-detects: Kubernetes (via
# KUBERNETES_SERVICE_HOST), then Docker (/.dockerenv) and Podman
# (/run/.containerenv), then PID 1's cgroup for anything else, falling back to
# "host". Set this when the sandbox leaves no trace detection can see, or to say
# something more precise than "container" — an operator always knows better than
# the heuristic.
# runtime = "Firecracker microVM"

# Treat the [[agent]] list as the full desired state for pre-defined agents
# (adr/predefined-agents-design.md §7 v3). When true, a config-origin goal no longer
# listed in [[agent]] is archived and its session stopped on boot;
# conversation-created goals are never touched. Defaults to false — removing an
# entry just stops reconciling it, leaving the goal for manual archival.
standing_agents_authoritative = false

# Session event journal retention (event-journal.md), applied by a boot-time
# scrub. Keep the last N turns per agent; 0 uses the built-in default, negative
# keeps all turns. event_retention_days additionally drops events older than N
# days (0 = no age limit).
# event_retention_turns = 0
# event_retention_days  = 0

# Link topically-similar sessions out-of-band and surface a related prior session
# on a later turn (event-journal.md). On by default; requires an embedder
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
#
# Additional routines the session carries alongside its pursue shell, each waking
# on its own cadence. A session runs one turn at a time, so when several routines
# are due the one waiting longest goes first — fairness does not depend on the
# order they are written here.
#
# The pursue shell stays the session's role-bearing routine: a routine never sets a
# role, because a session has exactly one and two claimants would make it depend
# on ordering. `kind` must be a registered routine kind, and exactly one of
# interval/schedule must be set — a routine with neither would never wake.
#
#   [[agent.routine]]
#   kind     = "idle-reflection"
#   interval = "1h"


[plugins]
# The directory holding plugins that ship as their own binaries. Nine ships none:
# the Go built-in (shell) is compiled into the nine binary and
# started as `nine plugin serve <name>`, so they are not looked up here at all.
# It stays configurable for a plugin of your own that puts its binary here; in
# Docker the path is /opt/nine/bin, which the container sets via
# NINE_PLUGINS_BIN. Operator-supplied plugins use `user_dir` below, not this.
bin = "./dist/bin"
# Plugins that must never start, by name. This is how a capability is withheld —
# most obviously `shell`, which runs arbitrary commands. It applies to built-ins,
# to MCP servers (as `mcp:<name>`), and to user plugins alike, and a disabled
# plugin is reported as `off` by `nine plugins` rather than being silently
# absent. Overridable with NINE_PLUGINS_DISABLED="shell,mcp:github" so a
# container needs no second config.
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
# Long-running jobs: how often the daemon polls a running PLUGIN job (default
# 2s), the per-job lifetime bound (default 3600s), the per-conversation cap on
# outstanding jobs (default 8), and the daemon-wide cap across every conversation
# (default 32).
#
# Despite living under [plugins], the last three apply to BOTH job backends — a
# plugin's detached goroutine and a resumable sandboxed tool (spec/contracts/
# toolvm.md R-TVM.19). They stayed here rather than moving when the tool backend
# landed, because renaming settled config keys is a breaking change for a
# cosmetic gain. job_poll_seconds is plugin-only: a tool job's next call is due
# when the tool said it was, not on a poll schedule.
#
# max_jobs_per_conversation bounds one agent; max_jobs_total bounds the machine.
# The second matters more now that a tool job is work THIS daemon performs rather
# than work another process is doing.
# job_poll_seconds = 2
# job_max_seconds = 3600
# max_jobs_per_conversation = 8
# max_jobs_total = 32

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
# [plugin.files.settings]
# NINE_WORKSPACE = "/srv/data"   # override a built-in default


# MCP servers. Each entry becomes one plugin (`mcp:<name>`) with its tools
# prefixed by the server name, so two servers cannot collide. Disable one with
# [plugins] disabled = ["mcp:github"].
# [[mcp.server]]
# name    = "github"
# command = "npx"
# args    = ["-y", "@modelcontextprotocol/server-github"]
# [mcp.server.env]
# GITHUB_PERSONAL_ACCESS_TOKEN = "ghp_..."
#
# A hosted server is reached by url instead of being spawned (MCP streamable
# HTTP). Use headers for auth; env applies only to a spawned command.
# [[mcp.server]]
# name = "hosted"
# url  = "https://mcp.example.com/rpc"
# [mcp.server.headers]
# Authorization = "Bearer ..."

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
# The one directory Nine writes agent files into: the sandboxed file tools are
# mounted here and `shell` runs here. Optional; the container overrides it to
# the mounted volume at /data/workspace (NINE_WORKSPACE_ROOT).
# root = "./workspace"

# Deleted and overwritten files move to .nine/trash/ inside the workspace
# rather than being destroyed, so a mistake in an unsupervised session can be
# undone (trash_list, restore_file). The trash sits on the operator's disk, so
# it is bounded by both age and size; the sweep runs hourly, oldest entry first.
#
# trash_retention_days — days a trashed file is kept. Unset uses 7. A negative
# value disables the age sweep, leaving trash_max_bytes as the only bound.
# trash_retention_days = 7
#
# trash_max_bytes — total trash size, oldest entry removed first. An age bound
# alone is not enough: a week of large deletions can outgrow a volume before
# anything expires. Unset uses 1 GiB.
# trash_max_bytes = 1073741824

# Files in the workspace are full-text searchable, including ones Nine never
# wrote — a bind mount that arrived full, a git pull, a file dropped in. A
# background scan keeps the index in step with the directory, and a search
# refreshes the subtree it is about to look at.
#
# scan_interval_seconds — how often the workspace is rescanned for files
# changed outside Nine. Unset uses 60.
# scan_interval_seconds = 60
#
# index_max_file_bytes — largest file whose text is indexed. The bound is about
# churn, not storage: FTS5 rewrites a document's whole posting list when it
# changes, so a large file that grows costs its full size in tokenization on
# every scan that sees it. Larger files still list, and are searched when named
# directly. Unset uses 8 MiB.
# index_max_file_bytes = 8388608
#
# index_max_files — files examined in one scan. A scan that stops here says so
# in search results. Unset uses 50000.
# index_max_files = 50000

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

[api]
# HTTP API over the daemon's Unix socket (docs/api.md has the endpoint surface).
# Off by default; when off, `nine api serve` still works but the daemon does not
# start the server automatically.
enabled = false
#
# port — HTTP listen port. Unset uses 8080.
port = 8080
#
# host — bind address. Unset uses "localhost" (IPv4 loopback only). Use
# "0.0.0.0" to listen on all interfaces.
host = "localhost"
#
# auth_token — bearer token required on every request
# (`Authorization: Bearer <token>`). Empty disables authentication. Also
# settable via --auth-token or NINE_API_AUTH_TOKEN.
auth_token = ""
#
# timeout_seconds — per-request timeout. Unset uses 30. A longer request
# returns 504.
timeout_seconds = 30
#
# max_connections — concurrent connection ceiling. Unset uses 100.
max_connections = 100
#
# cors_origins — allowed CORS origins. Unset allows all; narrow it in
# production.
cors_origins = ["*"]
#
# trusted_proxies — reverse proxies whose X-Forwarded-For and X-Real-IP headers
# the API believes, as bare IPs or CIDR blocks. Empty — the default — ignores
# both headers and keys rate limiting off the transport peer. Both headers are
# attacker-controlled on any request that did not pass through a proxy you run,
# so trusting them unconditionally lets a client mint a fresh rate-limit bucket
# per request. Set this only for proxies actually in front of the API.
trusted_proxies = []

[api.rate_limit]
# Rate limiting is applied before authentication, so an unauthenticated flood is
# bounded too.
enabled             = true
requests_per_minute = 60    # unset uses 60
burst_size          = 10    # short bursts above the limit; unset uses 10
# excluded_paths — paths exempt from rate limiting. Unset uses the two probes.
excluded_paths = ["/api/v1/health", "/api/v1/status"]

[api.tls]
# HTTPS. Off by default, which is correct for a loopback bind behind a proxy
# that terminates TLS itself.
enabled   = false
cert_path = ""
key_path  = ""

[planning]
# Plan-before-execute policy (../adr/thinking-and-planning.md). Both keys have a
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
# max_output_tokens — the per-result tool output cap (docs/tool-output.md).
# A result larger than this is written whole to the memory file store under
# spill/<agent-id>/ and replaced in context by a short preview naming the path,
# which the agent can read back with read_file(offset, limit) or search with
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

# Per-call wall clock. Default 5s.
timeout = "5s"

# Per-call work budget for a `js` tool, in operations. Default 50,000,000;
# a negative value turns it off.
#
# This bounds what a call DOES, where timeout bounds how long it takes — a
# deadline is a property of the machine, a budget is a property of the tool. It
# is enforced by an uncatchable interrupt, so a tool cannot try/catch past it.
#
# Calibrated well above real work: every shipped tool finishes under 10,000
# operations, parsing 20,000 CSV rows costs 1.7M, and a one-million-pass loop
# costs 2.0M. A runaway tool meets 50M in well under a second.
#
# A `wasm` tool is NOT metered by this — the budget is QuickJS's interrupt
# handler and a raw module has no interpreter to interrupt.
max_ops = 50000000

# Per-call linear memory cap. Default 16.
memory_mb = 16

# How many tool calls run at once, across every tool and conversation. Default 8.
#
# The other half of memory_mb: that is what one call may hold, this is how many
# may hold it, and 8 x 16 MiB is the host's 128 MiB worst case. A call arriving
# with every slot taken waits; the wait is charged to the turn, not to the call's
# own deadline. job_workers below bounds the sweeper's share the same way.
max_concurrent = 8

# Long-running tools: bounds on a tool that runs as a background job.
#
# A tool whose manifest says `resumable = true` may end a call by asking to be
# called again, carrying a cursor. Each call is an ordinary call under the
# ordinary deadline; what these bound is the total.
#
# job_max_calls is not defensive. Every individual call is legal, so without a
# cap a tool that always asks to continue runs forever, one legal call at a time.
# job_min_delay_ms floors the delay a tool may request, for the same reason.
#
# A job's lifetime and per-conversation cap are shared with plugin jobs and live
# under [plugins]: job_max_seconds and max_jobs_per_conversation.
job_max_calls    = 720       # 0 uses 720
job_min_delay_ms = 250       # 0 uses 250

# How many long-running tool calls the sweeper makes at once. Distinct jobs run
# in parallel; one job is never called twice at once, whatever this is set to.
#
# Really a memory budget: each concurrent call is a wasm instantiation holding up
# to memory_mb above, so 4 workers at the default 16 MiB is 64 MiB in the worst
# case.
job_workers      = 4         # 0 uses 4

# A standing agent can also wake on a CONDITION rather than a clock — see the
# [[agent]] blocks below and docs/scheduling.md:
#
#   [[agent]]
#   id   = "sec-watch"
#   when = { tool = "cve_scan", interval = "10s" }
#
# The predicate is a sandboxed tool run on that cadence with no model in the
# loop; the agent's turn happens only when it returns something. It is a standing
# tool underneath, listed as when:<agent-id>, so it backs off when it breaks and
# can be stopped like any other.

# ── Standing tools: run one indefinitely ─────────────────────────────────────
# A resumable tool can also be run STANDING: on its own cadence, started at boot
# rather than by a turn. Same sandbox and same capability grants as any other
# tool — this only changes when and how often it runs.
#
# A CYCLE is one pass. Calls run until the tool returns a result instead of
# asking to continue; then the cursor resets and the trigger below decides when
# the next cycle starts. So there are two cadences: the trigger between cycles,
# and the tool's own afterMs within one.
#
# Ownership splits the way [[agent]] does: this file owns the definition (tool,
# args, trigger) and the runtime owns whether it is running. So a standing tool
# you stopped stays stopped across a restart, and editing this file does not
# restart it. Changing `args` does restart its cycle — the cursor it was holding
# was produced under the old arguments.
#
# A cycle's output goes to the human feed (`nine notifications`). Returning
# nothing is silent, which is what keeps a ten-second watcher usable.
#
# [[standing_tool]]
# id       = "corpus"          # operator-chosen and stable; reconciliation keys on it
# tool     = "corpus_index"    # a loaded tool whose manifest says resumable = true
# interval = "10s"             # …or schedule = "*/5 * * * *", never both
# args     = { root = "/srv/corpus" }
# enabled  = true              # false declares it without starting it

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
#
# `[tool.<name>]` also carries `timeout`, which is a resource bound rather than a
# capability and so sits outside `[capabilities]`. It overrides `[tools] timeout`
# for that tool alone — in either direction:
#
#   [tool.slow_report]
#   timeout = "30s"        # this tool needs longer
#
#   [tool.untrusted]
#   timeout = "1s"         # and this one should have less
#
# Worth having because a single global value has to accommodate the slowest tool,
# which then hands that same allowance to a tool that is merely stuck.
# [tool.<name>] max_ops overrides the work budget the same way, for the same
# reason. An outbound HTTP request is bounded at
# four fifths of the time the call has left, so raising the deadline raises that
# with it. `nine tools show <name>` prints the override when one is set.

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
# A bare "*" is permitted and means any host. It grants no address: the second
# gate above still refuses loopback, link-local and private ranges. Name the
# hosts when they are knowable — the wildcard is for a tool that fetches
# whatever URL a model chose.
[tool.weather.capabilities.net.http]
allow_hosts = ["api.weather.example", "*.cdn.weather.example"]  # exact, or a
                                                    # leading "*." (not the apex)
methods     = ["GET"]        # required; no implicit default
max_bytes   = 1048576        # response cap; 0 uses 1 MiB

# Durable state: what a tool may remember between calls.
#
# A tool is built fresh for every call and thrown away after it, so nothing in
# the interpreter survives. This grants a store the HOST owns instead: keys
# scoped to this one tool, bounded below, readable by nothing else. It is the
# proportionate answer to a tool that needs to cache 200 bytes of ETag and would
# otherwise have to be handed a directory on your disk.
#
# `scope` is REQUIRED and has no default, because the two values differ in
# something you should decide rather than inherit:
#
#   "tool"          one namespace shared by every caller. What a cache wants —
#                   and a channel from one conversation into another, since a
#                   tool's arguments come from the model and a call in one
#                   session can write down what a call in another reads back.
#                   No network grant is needed for that; grant it to a tool
#                   whose code you have read.
#   "conversation"  a separate namespace per conversation, which closes that.
#
# Quotas are per (tool, scope). Exceeding one is an error the tool can catch and
# recover from, never a silent drop. `nine tools show <name>` prints the
# resolved scope and quotas.
[tool.geocode.capabilities.state]
scope        = "tool"        # required: "tool" or "conversation"
max_keys     = 512           # 0 uses 128
max_value_kb = 8             # 0 uses 64
max_total_kb = 256           # 0 uses 1024
ttl          = "24h"         # optional; omit for no expiry

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

# May Nine write itself a tool that runs as a long-running background job? Off by
# default. Deliberately separate from the capability ceiling above: a ceiling
# bounds what a tool may REACH, and running for an hour is not reach. A
# capability-free tool that never stops is inert per call and unbounded in
# aggregate, which is exactly what a ceiling cannot express.
# [tools.agent] allow_long_running = false

# May Nine ask to run a tool it wrote STANDING — indefinitely, on its own
# cadence? Off by default, and separate from allow_long_running: a job the model
# started still ends, where a standing run does not until somebody stops it.
#
# Even with this on, a human approves every promotion — including when
# require_approval is "never". That is the one place the setting is overridden,
# and it is deliberate: "never" means the capability ceiling is the only control,
# and a ceiling bounds REACH. A capability-free tool that runs forever is inert
# per call and unbounded in aggregate, which a ceiling cannot express.
#
# A generated standing tool that fails repeatedly is switched off. One you
# declared in [[standing_tool]] is not — your declaration is a standing
# instruction, and silently disabling it would be the greater surprise.
# [tools.agent] allow_standing = false
# [tools.agent] max_standing = 4
```

---

## LLM providers

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
2. **Memory surfacing** — each `memory_set` is embedded so a later turn can pull back the memories relevant to it.

The default (`keyword`) uses a built-in feature-hashing embedder that requires no model and no network access. Switch to `ollama` for higher quality ranking.

### Built-in keyword embedder (default)

```toml
[embeddings]
provider = ""   # or "keyword" — both select the built-in embedder
```

No configuration needed. Suitable for most deployments.

### Using Ollama for embeddings

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

## Environment variables

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
| `NINE_PLUGINS_DISABLED` | Override `plugins.disabled` — comma-separated plugin names that must never start, e.g. `shell,mcp:github`. Replaces the file's list rather than adding to it. |
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

## Changing configuration

Configuration is owned by the operator, not the agent: Nine cannot modify `nine.toml`
at runtime. To change a setting, edit the file and restart the daemon so it re-reads
the config on startup. (`NINE_LLM_*` environment variables let you override the LLM
provider/model/endpoint at launch without editing the file — convenient in Docker.)

---

## Limits

| Limit | Detail |
|-------|--------|
| No runtime reload | Nine cannot modify `nine.toml`, and nothing re-reads it while the daemon runs. Every change needs a restart. |
| No schema version | `nine.toml` carries no `schema_version` and there is no migrate-on-load, so an incompatible config change would break older files. See [versioning](versioning.md#limits). |
| Environment overrides are a fixed set | Only the documented `NINE_*` variables override the file. Whether the sandboxed-tool subsystem runs at all stays in `nine.toml` by design — `NINE_TOOLS_USER_DIR` is deliberately the only tool-related override. |
| Ollama and Mistral only | An unrecognized `[llm].provider` is refused at startup rather than falling back. |
| Unknown keys are not rejected | A misspelled key is ignored rather than reported, so a setting can silently fail to apply. |
