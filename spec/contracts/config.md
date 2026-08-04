# Contract — Configuration & Layout

**Status:** Built · **Depends on:** nothing · **Used by:** every subsystem at startup

---

## R-CFG.1 — Resolution order

Configuration is a single TOML file located by trying, in order:

1. `$NINE_CONFIG` (if set)
2. `./nine.toml`
3. `/nine.toml` (Docker bind-mount convention)
4. `~/.nine/nine.toml`

The first that exists is loaded; defaults fill anything unset. A conforming
implementation **MUST** honor this order.

---

## R-CFG.2 — Sections and fields

```toml
[llm]
provider        = "ollama"      # ollama (default) | anthropic  (openai chat NOT implemented)
model           = "qwen3.5:4b"
api_key         = ""            # or via env
endpoint        = ""            # base URL (ollama)
context_budget  = 4096          # tokens per assembled turn
max_concurrent  = 1             # in-flight LLM requests (1 for local models)
num_ctx         = 0             # ollama only: model context window
timeout_seconds = 0             # HTTP timeout for LLM calls; 0 = none

[daemon]
socket_path             = "/tmp/nine.sock"
task_timeout_seconds    = 1800  # default sub-agent group timeout (30 min); <=0 → 1800
max_goal_sessions       = 10    # concurrent pursue sessions; <=0 → default 10
event_retention_turns   = 0     # journal scrub: keep last N turns/agent; 0 → default, <0 → keep all
event_retention_days    = 0     # journal scrub: max event age in days; 0 → no age limit
related_sessions_index  = true  # out-of-band related-session indexing + surfacing (default on; no-op without embedder)
# standing_agents_authoritative = false   # treat [[agent]] as full desired state
# [[agent]] … config-declared standing agents (see predefined-agents contract)

[plugins]
dir = ""                        # plugin source/aux dir (if used)
bin = ""                        # compiled plugin binary directory
# user_dir = ""                 # operator plugins (sidecar-manifest layout); env NINE_PLUGINS_USER_DIR
# cache_dir = ""                # per-plugin cache-dir root; default os.UserCacheDir()/nine/plugins; env NINE_PLUGINS_CACHE_DIR
# job_poll_seconds = 2          # how often the sweeper polls a running plugin job
# job_max_seconds = 3600        # per-job lifetime bound; the sweeper expires an over-age job
# max_jobs_per_conversation = 8 # cap on a conversation's outstanding plugin jobs

# Per-plugin operator config (plugin.md R-PLUG.10/11). Singular [plugin.<name>]
# table, sibling to the plural [plugins] above.
# [plugin.<name>]
# persist_cache = false         # keep the plugin's cache dir across restarts (R-PLUG.11)
# [plugin.<name>.settings]      # schema-less env vars passed through verbatim at spawn (R-PLUG.10);
#   KEY = "value"               #   keys are env-var names, values TOML scalars; reserved NINE_PLUGIN_* rejected

[memory]
# PostgreSQL DSN (pgx). Default: postgres://nine:nine@localhost:5433/nine?sslmode=disable
# Overridable via NINE_DATABASE_URL.
database_url = "postgres://nine:nine@localhost:5433/nine?sslmode=disable"

[embeddings]
provider = "keyword"            # keyword (default, no network) | ollama | none
model    = ""
endpoint = ""
api_key  = ""

[ui]
theme        = "light"          # light | dark
show_context = true             # show the context-usage bar

[workspace]
root = ""                       # files-plugin working directory
```

Notes:

- `context_budget` falls back to `num_ctx` when budget-oriented code needs a number and
  `context_budget` is unset (small-model deployments often set only `num_ctx`).
- `max_concurrent = 1` serializes **all** LLM calls (correct for a single local model);
  higher values allow provider-side parallelism (R-LLM.4).

---

## R-CFG.3 — Configuration is set by the operator, applied at start

Config is read once at daemon start. There is **no** runtime config-rewrite tool and
**no** soft-reload (N2). To change configuration, edit the file and restart the daemon.
An implementation **MUST NOT** expose a tool that mutates `nine.toml`.

---

## R-CFG.4 — Volume / filesystem layout

The runtime separates **mutable state** from **immutable image content**:

```text
/data                       mutable state only (the "nine-data" volume)
└── workspace/         files-plugin working directory

/var/lib/postgresql/data   PostgreSQL cluster (the "nine-pgdata" volume)

/opt/nine                   immutable image content (NOT in a volume)
├── bin/                compiled nine binary's default plugins + browser launcher
└── browser/            browser plugin JS + node_modules
```

Primary state lives in **PostgreSQL**, which runs in the *same container* as the
daemon — supervised alongside it under s6-overlay, reachable at `localhost:5432`,
on its own volume kept separate from `/data` (docs/single-container.md) — **not**
in `/data`. Built-in skills are embedded in the `nine` binary (`//go:embed`) and
seeded into the `skills` table on every boot; there is **no** skills directory in
the image or either volume. The runtime image carries **no Go toolchain, no git,
and no source tree** (N3).

---

## R-CFG.5 — Logging & environment

- Final agent responses go to **stdout**; all logs go to **stderr**.
- `NINE_LOG_LEVEL` (e.g. `debug`, `info`) controls verbosity; `NINE_LOG_FORMAT=json`
  selects structured logs.
- `NINE_CONFIG` overrides config location (R-CFG.1); `NINE_DATABASE_URL` overrides the
  PostgreSQL DSN.
- `NINE_BIN` and `NINE_PLUGIN_SOCKET` are passed to every plugin subprocess (the plugin
  binary directory and the per-plugin Unix socket the plugin listens on); some plugins
  receive extra env (e.g. `BROWSER_*` settings for `browser`).

---

## Reference symbols

`internal/config/config.go` (`Config` and section structs, `Load`, `LoadDefault`).
