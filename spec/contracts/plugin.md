# Contract — Plugins & the Plugin Manager

**Status:** Built · **Depends on:** dispatcher (registration) · **Used by:** every turn that calls an external tool

Every external capability is a **subprocess**. A native Nine plugin serves a compact
request/reply envelope over **HTTP on a per-plugin Unix socket**, so one process handles
many concurrent calls; MCP servers — external, not under our control — keep speaking
JSON-RPC 2.0 over stdio. Plugins are isolation boundaries (invariant I9): a crash is a
child-process failure, not a daemon panic. They are **immutable image content** — there is
no runtime generation, build, or hot-swap (N1).

> **Transport.** Native plugins use HTTP over a Unix socket (see
> `docs/plugins-http-transport.md`); the two-method contract (R-PLUG.1) rides on it, with
> `max_concurrent` (R-PLUG.8). MCP servers use stdio JSON-RPC.
>
> **Packaging.** "Subprocess" does not imply "separate executable": the Go default
> plugins are served out of the `nine` binary as `nine plugin serve <name>` (R-PLUG.13).
> They are still one process each — everything below applies to them unchanged.

---

## R-PLUG.1 — The plugin contract (two methods)

A plugin is any executable that implements exactly two methods. A native plugin serves
them over HTTP — `POST /rpc` on the Unix socket named by `NINE_PLUGIN_SOCKET` — with a
compact envelope. There is **no JSON-RPC `id`**: each call owns its connection, so
reply↔request correlation is per-connection. Only these two methods flow; there is no
generic method route.

```text
plugin.describe → returns {protocol_version, max_concurrent, async_jobs?, tools: [ {name, description, inputSchema [, display_name]} ]}
plugin.call({tool, args}) → returns {output [, job_id]}   (or an error envelope)
```

A native plugin that runs long work **MAY** additionally implement two job
methods (R-PLUG.12); `async_jobs: true` in `describe` advertises them, and a
`plugin.call` that started detached work returns a `job_id` in place of a result.
These are additive — protocol **v2** (see `docs/versioning.md`) — and a v1 plugin
that implements neither is fully conforming.

```text
POST http://unix/rpc
  body:   {"method":"plugin.call","params":{"tool":"my_tool","args":{"input":"hi"}}}
  reply:  {"result":{"output":"…"}}                  (success)
       |  {"error":{"code":-1,"message":"…"}}         (failure)
  HTTP status: always 200; errors live in the body envelope.
```

A conforming implementation **MAY** support plugins in any language; the contract is the
wire format, not the runtime. `display_name` is optional UI metadata and **MUST NOT**
reach the LLM (invariant I8). `max_concurrent` is optional (R-PLUG.8).

The daemon **MAY** send an `X-Nine-Request-ID` header carrying a per-call trace ID; a
plugin **SHOULD** log it (and surface it on the handler context) so one ID greps across
daemon and plugin logs. It is not part of the envelope — reply↔request correlation is
per-connection.

> MCP servers are the exception: they speak JSON-RPC 2.0 (with `id`) over stdio, and the
> manager adapts `plugin.call` ↔ `tools/call` behind the same internal client interface.

---

## R-PLUG.2 — `plugin.Serve` (author ergonomics)

A helper — `plugin.Serve` in `internal/plugin` — implements the HTTP server loop so a
plugin author only declares tools and handlers:

```text
plugin.Serve(
    []plugin.ToolDefinition{ {Name, Description, InputSchema (raw JSON via plugin.Schema) [, DisplayName]} },
    map[string]plugin.ToolHandler{ "name": func(ctx, args) (output, error) },
    opts... // e.g. plugin.WithMaxConcurrent(n)
)
```

It handles the HTTP listener on `NINE_PLUGIN_SOCKET`, `describe`/`call` routing, and error
encoding (`plugin.InvalidArgs`, etc.) — one goroutine per request, so handlers run
concurrently up to the advertised `max_concurrent`. This is a convenience, not a
requirement; the requirement is R-PLUG.1.

For long-running work, `plugin.WithJobHandlers(map[string]plugin.JobHandler)` (with an
optional `plugin.WithJobs(plugin.NewJobs())`) makes `Serve` advertise `async_jobs`, answer
the job methods (R-PLUG.12), and run each job on a context **detached from the HTTP
request** so writing the reply does not cancel the work. See R-PLUG.12.

---

## R-PLUG.3 — Manager lifecycle

The manager owns subprocess lifecycle:

```text
Start(binaryPath, extraEnv…)         — a plugin shipped as its own binary
StartBuiltin(name, extraEnv…)        — a plugin served by the nine binary (R-PLUG.13)
   allocate a per-plugin Unix socket path; pass it via NINE_PLUGIN_SOCKET
     (+ NINE_BIN, the cache-dir vars (R-PLUG.11), operator settings (R-PLUG.10), extras)
   spawn process; dial the socket with bounded retry (~3s budget)
   → plugin.describe  → {tool defs, max_concurrent, async_jobs}
   set the per-plugin HTTP transport's MaxConnsPerHost from max_concurrent (0 = unbounded; R-PLUG.8)
   track *Plugin{Name, client, Tools, AsyncJobs}
   register each tool with the dispatcher (handler = manager.Call(plugin, tool, args))
on each tool invocation:  → plugin.call{tool,args} → {output}  (or {job_id} → R-PLUG.12; its own pooled connection)
Stop:  SIGTERM, wait for clean exit (kill after grace), unlink the socket, remove the ephemeral cache dir
```

The manager registers tool definitions at start. (It does **not** write tool embeddings
to the `vectors` table — tool-relevance ranking uses per-tool description embeddings the
`AgentBuilder` computes and caches **in memory**; there is no `tools:` vector namespace.
See [`context-builder.md`](context-builder.md).)

---

## R-PLUG.4 — Crash isolation & recovery (I9)

If a plugin subprocess exits unexpectedly:

- the failure is contained to that plugin — the daemon and all active sessions continue;
- recovery is **restart-from-the-existing-binary**, never recompilation;
- the supervisor receives an `EventPluginCrashed` and triggers the manager's restart
  (see [`supervisor.md`](supervisor.md)).

Tool calls to a down plugin return an error the agent observes as a normal tool failure.

---

## R-PLUG.5 — Default plugins

Started at daemon boot from immutable content:

| Plugin | Tools | Artifact |
|--------|-------|----------|
| `files` | `read_file`, `write_file` | the `nine` binary (R-PLUG.13) |
| `shell` | `shell` (run an arbitrary command) | the `nine` binary (R-PLUG.13) |
| `http` | `http_get`, `http_post`, `web_search`, `web_page_read` | the `nine` binary (R-PLUG.13) |
| `time` | `time` | the `nine` binary (R-PLUG.13) |
| `browser` | headless-Chromium tools (R-PLUG.6) | its own binary under `[plugins].bin` |

> **Memory/file/vector operations are NOT a subprocess plugin.** `memory_*`, `file_*`,
> and the vector tools are **core-intercepted** (handled in-process by the dispatcher,
> backed by the store). This is a deliberate difference from older designs that shipped a
> `memory` plugin.
>
> They are still tools like any other from a client's point of view: `list_tools` reports
> them under the `core` plugin and `plugin_call` invokes them (R-PROTO.5). Having no
> subprocess behind them is an implementation detail, not a narrower surface.
>
> **Skills are also core-intercepted.** `skill_list/search/read/write/modify` are
> store-backed core handlers with binary-embedded immutable defaults — see
> [`skills.md`](skills.md).
> This migration is **complete**: there is no `skills` subprocess and no `plugins/skills/`
> directory; skills are seeded from the `nine` binary (`//go:embed`) into the `skills`
> table on boot.

---

## R-PLUG.6 — Browser plugin

A Playwright/Chromium plugin (reference build uses Bun, so no Node/npm at runtime)
exposing: `browser_navigate`, `browser_screenshot`, `browser_extract`, `browser_click`,
`browser_fill`, `browser_eval`, `browser_wait`, `browser_status`, `browser_reset`.

- It declares `max_concurrent: 1` (single shared page) — the one default plugin that
  must stay serial (R-PLUG.8).
- **SSRF protection:** private/loopback URLs are blocked by default; allow/block glob
  lists are configurable.
- Web research uses the browser by default; if it is unavailable, Nine falls back to
  `web_search` (DuckDuckGo by default; `SEARCH_PROVIDER`+`SEARCH_API_KEY` for
  brave/serpapi) or `web_page_read`.

The browser plugin is **MAY**-grade for a minimal conforming implementation, but the
fallbacks (`web_search`/`web_page_read`) **SHOULD** exist so web research degrades
gracefully.

---

## R-PLUG.7 — No runtime plugin mutation *by the agent* (N1)

No **agent-reachable** tool or path may write plugin source, build a plugin, start
a new plugin binary, hot-swap, or roll back a plugin. This is a self-modification
boundary: Nine cannot grant itself capabilities. Adding a built-in plugin means
editing the source repo and rebuilding the image.

This is distinct from **operator**-initiated loading. Just as `[skills].user_dir`
lets an operator add skills (R-SKILL.2), `[plugins].user_dir` lets an operator add
plugins (R-PLUG.9). Both are driven by operator-controlled config and CLI, never by
an agent tool, so neither is a path by which Nine mutates its own capabilities.

**Scope: native plugins.** Sandboxed tools (`spec/contracts/toolvm.md`) are a separate
subsystem with its own runtime, and the rule above is written about this one. The
sentence that generalizes — *Nine cannot grant itself capabilities* — is unchanged and
now load-bearing for both: a sandboxed tool's capabilities come from `[tool.<name>]` in
`nine.toml`, written by the operator, and no agent-reachable path writes one (I-TVM.2).
Nothing in the sandboxed-tool subsystem as built lets an agent author a tool at all.

---

## R-PLUG.9 — User plugins (operator-supplied)

Operator plugins are discovered from `[plugins].user_dir` (env
`NINE_PLUGINS_USER_DIR`), scanned separately from the built-in `bin` dir. Unset or
absent disables the feature; it is purely additive and never touches built-ins.

- **Sidecar-manifest layout.** A user plugin is a pre-built executable beside a
  `<name>.toml` manifest declaring `name` and `entrypoint` (resolved relative to
  the manifest). The manifest is a **gate**: a binary with no manifest beside it
  **MUST NOT** be executed. The manifest declares intent only — the tool list is
  authoritatively `plugin.describe` (R-PLUG.1), not the manifest.
- **Load sequence**, per manifest, in deterministic name order: (1) a malformed
  manifest or missing binary is skipped without executing anything; (2) the binary
  is started and **MUST** pass the R-PLUG.1 handshake and R-PLUG.3 protocol-version
  check, else it is skipped; (3) its tools **MUST NOT** collide with any
  already-loaded plugin — built-in first, then earlier user plugins — and a
  collision skips the whole plugin. **No override, ever.**
- **Fail-soft.** Any single failure is logged at ERROR and recorded in the
  per-plugin status (surfaced by `nine plugins`), but **MUST NOT** abort the boot
  or the loading of the other plugins.
- **Reload.** `nine plugins reload` (the `plugins_reload` wire message) re-runs
  discovery, stopping and restarting **only** user plugins; built-ins are
  untouched. It is an operator CLI/wire action, not an agent tool (R-PLUG.7).
  Newly-started plugins are seen by subsequently-built agent loops; in-flight turns
  keep the tool set they started with.
- **Pre-flight.** `nine plugin validate` runs the same handshake locally (no
  daemon), so a binary can be vetted before deployment.

`plugin.Probe` performs the spawn → `describe` → version-check → stop handshake
without tracking, and backs both the pre-load vetting and `validate`.

---

## R-PLUG.8 — Per-plugin concurrency

A plugin advertises `max_concurrent` in its `describe` result; the manager maps it onto
the per-plugin HTTP transport's `MaxConnsPerHost`. The value `0` (or omitted) means
**unbounded** — the default, and the correct choice for stateless handlers
(`shell`/`http`/`files`/`time`). A plugin with shared mutable state **MUST** declare a
finite cap (`browser` uses `1`). Bounding on the client side means the server needs no
semaphore.

Transport-level concurrency is **orthogonal to handler safety**: a cap of N lets N calls
reach the process at once, but a handler with unsynchronized shared state is unsafe
regardless — such a plugin must cap itself accordingly. (MCP plugins are serialized by
their stdio transport and ignore `max_concurrent`.)

---

## R-PLUG.10 — Operator settings pass-through

An operator configures a plugin Nine has never heard of through a **singular**
`[plugin.<name>]` table (sibling to the plural `[plugins]` subsystem table). Its
`[plugin.<name>.settings]` sub-table is a **schema-less** bag of keys Nine copies
through to the plugin process as environment variables at spawn — Nine never
declares them, so no rebuild is needed to configure a third-party plugin.

- **Keys** are used **verbatim** as env-var names; a key outside `[A-Za-z_][A-Za-z0-9_]*`
  is a **config error at load**, not a silent skip.
- **Values** are TOML scalars, stringified (bool → `"true"`/`"false"`); a table or
  array value is a config error.
- **Precedence** (later wins): OS env → `NINE_BIN` → Nine-owned vars → `PluginEnvs`
  built-in defaults → operator `settings`. Operator settings therefore override a
  built-in default (e.g. `BROWSER_HEADLESS`).
- **Reserved:** `NINE_PLUGIN_SOCKET`, `NINE_PLUGIN_CACHE_DIR`, and
  `NINE_PLUGIN_CACHE_PERSISTENT` **MUST NOT** be set via `settings` (config error) —
  not the whole `NINE_` prefix, so `NINE_WORKSPACE` stays overridable.
- Applies to **every** plugin including user plugins and MCP servers; read at spawn
  (a change reaches a running plugin only on `nine plugins reload` or restart).

## R-PLUG.11 — Per-plugin cache directory

The manager creates a scratch directory per plugin process and hands it over as
`NINE_PLUGIN_CACHE_DIR` (guaranteed to exist, mode `0700`), with
`NINE_PLUGIN_CACHE_PERSISTENT` = `0`/`1`. Nine **never reads it** — it is opaque
scratch, never embedded, and reaches the model only if the plugin returns it.

- **Root:** `[plugins].cache_dir`, else `os.UserCacheDir()/nine/plugins` (env
  `NINE_PLUGINS_CACHE_DIR`, **plural**). Must be durable, not `/tmp`.
- **Ephemeral (default):** `<root>/<name>.<rand8>/`, removed on `Stop`/`StopAll` and
  by a **boot sweep** of leftover `<name>.<hex>` dirs (the primary reclaim path,
  since a hard-killed daemon never runs `Stop`).
- **Persistent (`[plugin.<name>].persist_cache = true`):** `<root>/<name>/`, never
  removed by Nine.
- `Probe` (validate / user-plugin vetting) always uses a throwaway ephemeral dir,
  regardless of `persist_cache`, so validation never touches persistent state.

## R-PLUG.12 — Long-running jobs (native plugins only)

A tool call **MAY** start detached work and return a `job_id` plus a one-line
`output` ack instead of a result. Such a plugin **MUST** advertise `async_jobs`
and implement two methods; the daemon **rejects a `job_id` from a plugin that did
not advertise it** (fail-closed against version skew).

```text
plugin.job_status({job_id}) → {state: queued|running|done|failed|cancelled, progress?, output?, error?}
plugin.job_cancel({job_id}) → {cancelled: true}
```

An unknown `job_id` **MUST** answer `job_status` with `failed` and a clear error
(not an RPC error), so a daemon that lost track cannot wedge. `cancelled` is
terminal and distinct from `failed`. `progress` is free text.

- **Plugin side** (`plugin.NewJobs`): runs jobs through a worker pool honouring
  `max_concurrent` (excess jobs sit `queued`, since a job start frees the HTTP
  connection at once and the daemon's cap cannot hold that line), gives each job a
  `<cache_dir>/jobs/<job_id>/` dir, and evicts terminal jobs after a TTL.
- **Daemon side:** a `plugin_jobs` registry row keyed to the owning
  conversation records a stable `handle` (`job_<hex>`; the plugin-side id is never
  shown). A single sweeper polls `job_status` on an age-based backoff (base cadence
  for the first minute, then ~30s) and, on a terminal state, cap-or-spills the
  output into the row, journals the event, and posts a notification to the owner —
  so the completion enriches a **later** turn (pull, not push; per
  `docs/reactive-events.md`). A boot marks orphaned `running` rows `lost`;
  `job_max_seconds` expires an over-age job; `max_jobs_per_conversation` caps a
  conversation on admission.
- **Model-facing tools** (core-intercepted, granted whenever the registry is wired):
  `job_wait` (blocks up to a timeout, returns the result or current progress — a
  timeout is not an error), `job_check`, `job_list`, `job_cancel`. The context
  builder surfaces outstanding jobs as one compact line each.
- **MCP is excluded:** its adapter collapses replies to `{output}` and has no
  status/cancel to map. Settings (R-PLUG.10) and the cache dir (R-PLUG.11) do apply
  to MCP.

---

## R-PLUG.13 — Built-in plugins are served by the `nine` binary

The Go default plugins (`files`, `shell`, `http`, `time`) ship **inside the `nine`
binary**, not as separate executables. The manager starts one by re-executing that
binary:

```text
StartBuiltin(name, extraEnv…):
   resolve the nine binary (os.Executable)
   spawn it as:  nine plugin serve <name>
   …then exactly as R-PLUG.3: socket, describe, version check, transport, tracking
```

`nine plugin serve <name>` is a daemon-internal entry point, not an operator command:
it is absent from `nine help`. This is the same self-exec the client already uses to
auto-start the daemon ([`wire-protocol.md`](wire-protocol.md) R-PROTO.7).

### R-PLUG.13a — A plugin child does plugin work and nothing else

Sharing the binary means a plugin child could otherwise run the whole of nine's
startup. An implementation **MUST** dispatch `plugin serve` **before** any of it, so a
plugin process:

- **loads no configuration.** The config search reaches the cwd and `$HOME`, which a
  plugin inherits, and the file there carries `[embeddings].api_key` and every
  `[plugin.<name>.settings]` block — *including other plugins' settings*. A plugin that
  read it would walk straight around `sanitizedHostEnv`, whose whole purpose is to
  withhold that class of data (R-PLUG.10, R-PLUG.3). A plugin receives exactly the
  environment the manager hands it, and nothing it fetches for itself.
- **opens no log file.** Plugin output goes to stderr, which the manager wires to the
  daemon's. The daemon's log stays one process's account of itself rather than five
  interleaved.
- **builds no CLI**, so it never holds a `StartDaemon` or `StartTUI`.

It **MUST** also fail closed: with `NINE_PLUGIN_SOCKET` unset there is no caller to
answer, so the process exits non-zero naming the missing variable instead of idling.

This is confinement against mistakes, not a sandbox. `shell` runs arbitrary commands
by design, so no entry-point check bounds what a compromised *handler* can do; what is
guaranteed is that **starting** a plugin has no effect beyond that plugin.

An implementation **MUST** preserve every property R-PLUG.3/R-PLUG.4 give an
out-of-binary plugin, because only the packaging changes:

- a built-in runs as its **own process**, so crash isolation (I9) is unchanged — a
  panic in `shell` cannot take down the daemon;
- it receives the same **sanitized environment** (the daemon's secrets and database
  path are withheld), its own cache dir (R-PLUG.11), and its operator settings
  (R-PLUG.10);
- it is bound by `max_concurrent` the same way (R-PLUG.8).

An implementation **MUST NOT** derive a built-in's name from its executable path: the
path is `nine` for all of them, so the name is carried explicitly from the caller.

The protocol-version check (R-PLUG.3) still runs for built-ins but can no longer fail
for them — daemon and plugin are the same build. It remains load-bearing for user
plugins (R-PLUG.9) and MCP servers, which are genuinely separate artifacts.

**`browser` is deliberately excluded.** It is Node + Chromium, so it cannot live in a
Go binary; it stays a separate artifact resolved through `[plugins].bin` and started
with `TryStart`.

A built-in has no binary to omit, so the implicit lever an operator used to have —
suppress a plugin by not shipping `dist/bin/<name>` — no longer exists for these four.
R-PLUG.14 replaces it with an explicit one.

---

## R-PLUG.14 — `[plugins].disabled`

`[plugins] disabled = ["shell"]` names plugins that **MUST NOT** start. Entries are
wire names (`shell`, `browser`, a user plugin's manifest name), and the list applies
uniformly to **every** start path — built-ins (R-PLUG.13), plugins with their own
binary, and user plugins (R-PLUG.9). `NINE_PLUGINS_DISABLED` (comma-separated)
overrides it, so a container can withhold a plugin without a second config file.

An implementation **MUST**:

- refuse a disabled plugin **before spawning anything**, so no process, socket, or
  cache dir is created for it. Every native start path funnels through one check for
  exactly this reason — a caller that forgets to ask still cannot spawn a disabled
  plugin. `StartMCP` builds its own stdio client rather than going through that path,
  so it **MUST** carry the check itself;
- treat the refusal as a **decision, not a failure**: the daemon boots normally and
  the tolerant starters (`TryStart`, `TryStartBuiltin`, user-plugin loading) report it
  as switched off rather than broken;
- **surface it**. A disabled plugin appears in `plugins_list` with `disabled: true`
  and a reason, and `nine plugins` prints it as `off`. A plugin that is simply absent
  from the roster gives an operator debugging a missing tool nothing to read — the
  same reasoning as the skipped-user-plugin entries in R-PLUG.9 and the skipped tools
  in [`toolvm.md`](toolvm.md) R-TVM.6.

A name that matched nothing **MUST** be reported. `disabled = ["shel"]` withholds
nothing and leaves `shell` — arbitrary command execution — running, with no error and
no roster entry, which is the one failure mode of this setting that is worse than not
having it: it fails open while reading as closed. Names cannot be validated when
config is parsed, because a user plugin's name is not known until its directory is
scanned, so the check belongs after loading. It is a warning rather than a hard
failure: the name may legitimately belong to a user plugin the operator has not
deployed yet.

Disabling is an **operator** action, never an agent one (N1, R-PLUG.7): it is read
from config at boot and there is no tool or wire message that switches a plugin on or
off at runtime. `NINE_PLUGINS_DISABLED` can add to or replace the list but **MUST NOT
be able to clear it** — an environment variable that could re-enable `shell` is a
hazard in the one direction this setting must never move by accident.

`nine plugin validate` still works against a disabled plugin — validation asks whether
a binary *is* a plugin, which is independent of whether this daemon runs it.

---

## Reference symbols

`internal/builtins/` (`Serve`, `Names`, `Has`; `shell.go`, `files.go`, `http.go`,
`time.go` — the built-in handlers, R-PLUG.13),
`cmd/nine/main.go` (`pluginServeArgs`, `servePluginAndExit` — the single-purpose
plugin entry point, R-PLUG.13a),
`internal/plugin/manager.go` (`Manager`, `Start`, `StartBuiltin`, `Call`, `JobStatus`,
`JobCancel`, `PluginByName`, `TryStart`, `TryStartBuiltin`, `launch`/`binaryLaunch`/
`builtinLaunch`, `spawnAndDescribe`, `Probe`, `SetPluginEnv`, `SetBuiltinBinary`, `SetCacheConfig`,
`SweepCache`), `internal/plugin/userplugins.go` (`LoadUserPlugins`, `ReloadUserPlugins`,
`UserStatus`), `internal/plugin/cache.go` (`allocCacheDir`, `SweepCache`),
`internal/plugin/jobs.go` (`Jobs`, `Job`, `JobHandler`, `JobStatus`, `JobDir`, `SetProgress`),
`internal/plugin/manifest.go` (`Manifest`, `LoadManifest`, `discoverPlugins`),
`internal/plugin/` (`client.go` `newHTTPClient` — native HTTP transport; `client`/`mcp.go`
— stdio, retained for MCP; `serve.go` `plugin.Serve` — the plugin-side HTTP server loop on
`NINE_PLUGIN_SOCKET`, `WithJobHandlers`/`WithJobs`; `contract.go` — `ToolDefinition`/`DescribeResult`),
`internal/memory/plugin_jobs.go` (the `plugin_jobs` registry), `internal/runtime/plugin_jobs.go`
(job starter + sweeper), `internal/runtime/job_tools.go` (model-facing tools + surfacing),
`internal/agent/register_jobs.go` (`job_wait`/`job_check`/`job_list`/`job_cancel`),
`plugins/browser/`, `cmd/nine/daemon.go` (`LoadUserPlugins`, job
sweeper, graceful shutdown at boot), `internal/cli/plugins.go` (`nine plugins` / `nine plugin validate`).
