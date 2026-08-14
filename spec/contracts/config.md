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
provider        = "ollama"      # ollama is the only chat backend (openai chat NOT implemented)
model           = "qwen3.5:4b"
endpoint        = ""            # base URL; empty = http://localhost:11434
context_budget  = 4096          # tokens per assembled turn
max_concurrent  = 1             # in-flight LLM requests (1 for local models)
num_ctx         = 0             # model context window
timeout_seconds = 0             # HTTP timeout per call; 0 = adapter default (300s), <0 = none

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
bin = ""                        # dir of plugins shipping their own binary (browser);
                                # the Go built-ins live in the nine binary (plugin.md R-PLUG.13)
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

# Sandboxed tools (toolvm.md is normative for the whole [tools] / [tool.<name>] surface).
# [tools] enabled unset ⇒ no host, no tools. [tools.agent] is the generated tier (R-TVM.14):
# off and independent of [tools] enabled — an operator may want developer tools without
# letting the agent author any.
# [tools] cache_dir = ""            # dependency cache root; default os.UserCacheDir()/nine/tools
# [tools.agent]
# enabled            = false        # turns on tool_write/tool_delete
# eval               = false        # additionally allow js_eval
# max_tools          = 64           # catalog cap; LRU eviction past it
# require_approval   = "on_capability" # on_capability (default) | always | never — validated at load
# allow_network_deps = false        # lift the deps+net.http interlock (R-TVM.15); dangerous
# [tools.agent.capabilities]        # the CEILING: the maximum a generated tool may be granted,
#   fs = { read = [ … ] }           #   never an automatic grant. A tool that declares nothing
#                                   #   gets nothing. Same shape as [tool.<name>.capabilities].
# [tools.agent.deps]                # external npm deps (R-TVM.15); off by default
#   mode          = "off"           #   off (default) | allowlist | open — validated at load
#   registry      = ""              #   npm-compatible base URL; empty = public registry
#   frozen        = false           #   resolve only from cache/lockfile, never the network
#   max_packages  = 24              #   budgets (incl. transitive) / max_bundle_kb / max_depth
#   allow = [{ name = "date-fns", version = "^4.1.0" }]  # allowlist mode: named packages + ranges

[memory]
# SQLite database file. Created on first run, along with its parent directory.
# Default: /data/nine.db when the container's /data volume is present, else
# ~/.nine/nine.db. Overridable via NINE_DB_PATH.
path = "~/.nine/nine.db"

[embeddings]
provider = "keyword"            # keyword (default, no network) | ollama | none
model    = ""
endpoint = ""
api_key  = ""

[ui]
theme        = "light"          # light | dark
show_context = true             # show the context-usage bar (a ⚠ warning shows at ≥90% even when false)

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
├── nine.db             the SQLite database (plus its -wal/-shm sidecars)
└── workspace/          files-plugin working directory

/opt/nine                   immutable image content (NOT in a volume)
├── bin/                browser launcher (the Go plugins are inside the nine binary)
└── browser/            browser plugin JS + node_modules
```

Primary state is the **SQLite** file at `/data/nine.db`, so the database and the
workspace share one volume (docs/single-container.md) and the container runs a
single process. Built-in skills are embedded in the `nine` binary (`//go:embed`) and
seeded into the `skills` table on every boot; there is **no** skills directory in
the image or either volume. The runtime image carries **no Go toolchain, no git,
and no source tree** (N3).

---

## R-CFG.5 — Logging & environment

- Final agent responses go to **stdout**; all logs go to **stderr**.
- `NINE_LOG_LEVEL` (e.g. `debug`, `info`) controls verbosity; `NINE_LOG_FORMAT=json`
  selects structured logs.
- `NINE_CONFIG` overrides config location (R-CFG.1); `NINE_DB_PATH` overrides the
  database file path.
- `NINE_BIN` and `NINE_PLUGIN_SOCKET` are passed to every plugin subprocess (the plugin
  binary directory and the per-plugin Unix socket the plugin listens on); some plugins
  receive extra env (e.g. `BROWSER_*` settings for `browser`).

---

## Reference symbols

`internal/config/config.go` (`Config` and section structs, `Load`, `LoadDefault`).
