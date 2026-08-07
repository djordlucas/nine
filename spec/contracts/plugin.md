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
Start(binaryPath, extraEnv…):
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

Started at daemon boot from immutable image content:

| Plugin | Tools |
|--------|-------|
| `files` | `read_file`, `write_file` |
| `shell` | `shell` (run an arbitrary command) |
| `http` | `http_get`, `http_post`, `web_search`, `web_page_read` |
| `time` | `time` |
| `browser` | headless-Chromium tools (R-PLUG.6) |

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

## Reference symbols

`internal/plugin/manager.go` (`Manager`, `Start`, `Call`, `JobStatus`, `JobCancel`,
`PluginByName`, `TryStart`, `spawnAndDescribe`, `Probe`, `SetPluginEnv`, `SetCacheConfig`,
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
`plugins/{files,shell,http,time,browser}/`, `cmd/nine/daemon.go` (`LoadUserPlugins`, job
sweeper, graceful shutdown at boot), `internal/cli/plugins.go` (`nine plugins` / `nine plugin validate`).
