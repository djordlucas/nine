# Contract — Plugins & the Plugin Manager

**Status:** Built · **Depends on:** dispatcher (registration) · **Used by:** every turn that calls an external tool

Every external capability is a **subprocess** serving a compact request/reply envelope
over **HTTP on a per-plugin Unix socket**, so one process handles many concurrent calls.
There is exactly one plugin transport: an MCP server reaches Nine through a plugin like
anything else (R-PLUG.15), so stdio is one bridge's internal detail rather than a second
path through the manager. Plugins are isolation boundaries (invariant I9): a crash is a
child-process failure, not a daemon panic. They are **immutable image content** — there is
no runtime generation, build, or hot-swap (N1).

> **Transport.** Plugins use HTTP over a Unix socket (see
> `docs/plugins-http-transport.md`); the two-method contract (R-PLUG.1) rides on it, with
> `max_concurrent` (R-PLUG.8). This is the only transport — MCP arrives through the
> bridge plugin (R-PLUG.15), not through a second one.
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
   spawn process; dial the socket until it accepts, the process exits, or the
     budget expires (R-PLUG.14)
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

## R-PLUG.6 — Browser automation is not a plugin

An implementation **MUST NOT** ship a browser as a default plugin. Browser automation is
obtained by declaring a browser MCP server as an `[[mcp.server]]` (R-PLUG.15); its tools
arrive prefixed with the server name and are otherwise indistinguishable from any other
plugin's.

Earlier revisions specified a Playwright/Chromium plugin here — `browser_navigate`,
`browser_extract`, and seven siblings — with in-process SSRF protection (private and
loopback URLs blocked by default, configurable allow/block globs). That plugin is
**withdrawn**, and with it the guarantee:

- **No URL policy is enforced** on a browser MCP server. Nine does not inspect or filter
  an MCP server's arguments, and the upstream Playwright server documents its own
  `--allowed-origins`/`--blocked-origins` as *not* a security boundary and *not* applied
  to redirects. An implementation **MUST NOT** represent them as one.
- A deployment whose agent browses untrusted pages **SHOULD** place the control at the
  network layer instead. See [`../../docs/browser.md`](../../docs/browser.md) §5.

`web_search` and `web_page_read` (R-PLUG.5, the `http` plugin) **MUST** remain available
as the no-browser path for web research: DuckDuckGo by default, `SEARCH_PROVIDER` +
`SEARCH_API_KEY` for brave/serpapi. They are plain HTTP and **MUST NOT** be described to
the model as a fallback to a browser that may not be present.

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
finite cap (the `mcp` bridge uses `1`). Bounding on the client side means the server needs no
semaphore.

Transport-level concurrency is **orthogonal to handler safety**: a cap of N lets N calls
reach the process at once, but a handler with unsynchronized shared state is unsafe
regardless — such a plugin must cap itself accordingly. The MCP bridge is the worked
example: stdio cannot match a response to a request without owning the stream for the
round trip, so it declares `max_concurrent: 1` — a limitation stated through the contract
rather than exempted from it.

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
  built-in default (e.g. `NINE_WORKSPACE`).
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

## R-PLUG.12 — Long-running jobs (opt-in)

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
- **Daemon side:** a `jobs` registry row keyed to the owning
  conversation records a stable `handle` (`job_<hex>`; the plugin-side id is never
  shown). A single sweeper polls `job_status` on an age-based backoff (base cadence
  for the first minute, then ~30s) and, on a terminal state, cap-or-spills the
  output into the row, journals the event, and posts a notification to the owner —
  so the completion enriches a **later** turn (pull, not push; per
  `adr/reactive-events.md`). A boot marks orphaned `running` rows `lost`;
  `job_max_seconds` expires an over-age job; `max_jobs_per_conversation` caps a
  conversation on admission.
- **Model-facing tools** (core-intercepted, granted whenever the registry is wired):
  `job_wait` (blocks up to a timeout, returns the result or current progress — a
  timeout is not an error), `job_check`, `job_list`, `job_cancel`. The context
  builder surfaces outstanding jobs as one compact line each.
- **The MCP bridge does not advertise jobs:** MCP has no status/cancel to map onto them,
  so the bridge reports `async_jobs: false` like any plugin without them. Nothing special
  is needed — it is a plugin declining an optional capability, not an exclusion.

---

## R-PLUG.13 — Built-in plugins are served by the `nine` binary

The one Go default plugin, `shell`, ships **inside the `nine` binary**, not as a separate
executable. (`time`, `files` and `http` were among them and are now shipped sandboxed tools
— R-TVM.16. None needed a subprocess holding the daemon's uid, which meant `read_file`
could read any absolute path and `http_get` could reach cloud instance metadata with none
of the SSRF checks in the way. `shell` stays because it needs real `exec`.) The manager starts one by re-executing that
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

No default plugin is excluded: all four are Go and all four live in the binary. A
capability that cannot (a browser) or should not (a third-party service) live in the Go
binary is an `[[mcp.server]]` instead (R-PLUG.15), not a second artifact to ship.

A built-in has no binary to omit, so the implicit lever an operator used to have —
suppress a plugin by not shipping `dist/bin/<name>` — no longer exists for these four.
R-PLUG.14 replaces it with an explicit one.

---

## R-PLUG.14 — `[plugins].disabled`

`[plugins] disabled = ["shell"]` names plugins that **MUST NOT** start. Entries are
wire names (`shell`, `mcp:<server>`, a user plugin's manifest name), and the list applies
uniformly to **every** start path — built-ins (R-PLUG.13), plugins with their own
binary, and user plugins (R-PLUG.9). `NINE_PLUGINS_DISABLED` (comma-separated)
overrides it, so a container can withhold a plugin without a second config file.

An implementation **MUST**:

- refuse a disabled plugin **before spawning anything**, so no process, socket, or
  cache dir is created for it. Every start path funnels through one check for exactly
  this reason — a caller that forgets to ask still cannot spawn a disabled plugin. With
  MCP behind a plugin (R-PLUG.15) there is no longer a second spawn path to keep in
  step;
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

## R-PLUG.15 — MCP servers are plugins

An MCP server is reached through a **bridge plugin**, not through a second code path in
the manager. The daemon starts one bridge instance per `[[mcp.server]]`:

```text
[[mcp.server]]                        [[mcp.server]]
name    = "github"                    name = "hosted"
command = "npx"                       url  = "https://mcp.example.com/rpc"
args    = ["-y", "…server-github"]    [mcp.server.headers]
[mcp.server.env]                      Authorization = "Bearer …"
GITHUB_TOKEN = "…"

  → daemon: StartBuiltinInstance("mcp", "mcp:<name>", NINE_MCP_SERVER={…})
  → bridge: listen, then connect — spawn over stdio, or POST over streamable HTTP
  → bridge: initialize → notifications/initialized → tools/list
  → bridge: serve those tools over the ordinary plugin contract (R-PLUG.1)
```

Two transports are supported, and exactly one is configured per server: `command`
spawns a local server and speaks JSON-RPC over its stdio pipes; `url` reaches a hosted
one over **streamable HTTP**, where a server answers each POST with either a JSON body
or an SSE stream and the client must handle both. Hosted servers never run locally, so
stdio cannot reach them at all.

An implementation **MUST**:

- give each server **its own plugin instance**, named `mcp:<server>`. One server crashing,
  hanging, or failing to start affects only itself — the isolation guarantee of R-PLUG.4,
  which an in-manager adapter could not offer because a wedged stdio read blocked inside
  the daemon. The name is also what the roster shows and what `[plugins].disabled`
  matches (R-PLUG.14);
- **prefix each tool** with the server name (`github__create_issue`). Two MCP servers
  commonly advertise the same generic name (`search`, `read`), and an unprefixed
  collision means one server silently loses a tool depending on load order (R-PLUG.9);
- pass the server's configuration **from the daemon**, not by reading `nine.toml` in the
  bridge — a plugin child loads no config (R-PLUG.13a), and that file carries other
  plugins' secrets;
- **declare the transport's limits through the contract, not around it.** stdio is
  serial, so the bridge reports `max_concurrent: 1` (R-PLUG.8) and `async_jobs: false`
  (R-PLUG.12). These are ordinary plugin declarations, not exemptions;
- **listen before handshaking.** The daemon bounds how long it waits for a plugin's
  socket (~3s, R-PLUG.3) but puts no deadline on `plugin.describe`. An MCP server
  launched through `npx`/`uvx` routinely needs far longer than the socket budget just
  to start — a measured 72s in one case — so a bridge that completes the handshake
  before listening is declared dead for every real server. It **MUST** listen first and
  resolve its tools inside `describe` (`plugin.ServeDeferred`), under its own finite
  timeout so a server that never answers cannot hold up boot.

The point is what the core no longer contains. There is one plugin transport, one client
implementation, and no `except MCP` clause in this contract: the stdio client, the
`plugin.call ↔ tools/call` adapter, and the second spawn path all live inside one plugin
that the daemon treats like every other.

### R-PLUG.15b — Content is flattened, never filtered

An MCP reply is an array of content parts and the plugin contract returns one string, so
the bridge flattens. It **MUST NOT** drop a part it does not understand.

Text passes through. Binary parts — `image`, `audio`, and a `resource` carrying a blob —
are decoded and written into the plugin's cache dir (R-PLUG.11), with the path named in
the returned text; the bytes stay reachable through `read_file` without spending a context
window on base64. An embedded `resource` carrying text contributes that text, a
`resource_link` contributes its name and URI, and an unrecognized type is reported as
unsupported.

Silence is the one forbidden outcome. Text-only flattening made Playwright's
`browser_take_screenshot` return its summary — *"Screenshot of viewport"*, page title, URL
— with the image discarded, so the model received a confident account of a picture it
never got and no way to notice. A part that cannot be delivered **MUST** still be
described.

A server-supplied name that reaches a path **MUST** be sanitized: tool names come from the
server, and one called `../escaped` would otherwise write outside the cache dir.

---

## Reference symbols

`internal/builtins/` (`Serve`, `Names`, `Has`; `shell.go`, `files.go`, `http.go`,
`time.go` — the built-in handlers, R-PLUG.13),
`cmd/nine/main.go` (`pluginServeArgs`, `servePluginAndExit` — the single-purpose
plugin entry point, R-PLUG.13a),
`internal/plugin/manager.go` (`Manager`, `Start`, `StartBuiltin`, `Call`, `JobStatus`,
`JobCancel`, `PluginByName`, `TryStart`, `TryStartBuiltin`, `launch`/`binaryLaunch`/
`builtinLaunch`, `builtinInstanceLaunch`, `MCPInstanceName`, `spawnAndDescribe`, `Probe`, `SetPluginEnv`, `SetBuiltinBinary`, `SetCacheConfig`,
`SweepCache`), `internal/plugin/userplugins.go` (`LoadUserPlugins`, `ReloadUserPlugins`,
`UserStatus`), `internal/plugin/cache.go` (`allocCacheDir`, `SweepCache`),
`internal/plugin/jobs.go` (`Jobs`, `Job`, `JobHandler`, `JobStatus`, `JobDir`, `SetProgress`),
`internal/plugin/manifest.go` (`Manifest`, `LoadManifest`, `discoverPlugins`),
`internal/plugin/httpclient.go` (`newHTTPClient` — the one plugin transport),
`internal/builtins/mcp.go` + `mcp_stdio.go` (the MCP bridge and its private stdio
client, R-PLUG.15),
`internal/plugin/serve.go` `plugin.Serve` — the plugin-side HTTP server loop on
`NINE_PLUGIN_SOCKET`, `WithJobHandlers`/`WithJobs`; `contract.go` — `ToolDefinition`/`DescribeResult`),
`internal/memory/jobs.go` (the `jobs` registry), `internal/runtime/jobs.go`
(job starter + sweeper), `internal/runtime/job_tools.go` (model-facing tools + surfacing),
`internal/agent/register_jobs.go` (`job_wait`/`job_check`/`job_list`/`job_cancel`),
`internal/builtins/mcp_playwright_test.go` (the opt-in end-to-end check against the real
upstream Playwright MCP server), `cmd/nine/daemon.go` (`LoadUserPlugins`, `startMCPServers`, job
sweeper, graceful shutdown at boot), `internal/cli/plugins.go` (`nine plugins` / `nine plugin validate`).

---

## R-PLUG.14 — Startup readiness: distinguish dead from slow

A spawned plugin is reachable only once it listens, so the manager polls its socket
after spawning. That wait **MUST** end on **either** of two conditions, not just one:

- the socket accepts a connection — the plugin is up; or
- **the process has exited** — the plugin is dead, and the manager **MUST** report
  that, with the exit status, rather than continuing to poll.

An implementation that only polls until a deadline reports every startup failure as
"socket not ready". That names the symptom and hides the cause — a missing library, a
bad argument, an immediate panic all look identical to a slow start — and it pays the
entire budget to reach a conclusion it could have drawn at once.

Because death is detected directly, the budget governs only the remaining case:
**alive, but not yet listening**. It **SHOULD** therefore be generous. The wait is a
poll that returns the instant the dial succeeds, so a longer budget costs a healthy
plugin nothing; it only delays the verdict on a genuinely stuck one. The reference uses
**30s**, after a 3s budget produced a recurring spurious failure in CI-like parallel
load despite a measured startup of ~13ms warm and ~205ms for a freshly-built binary.

Reaping is the constraint that shapes this: a process may be waited on only once, and
both startup and stop need the exit status. An implementation **MUST** reap exactly
once and make the result available to every reader (the reference uses a `procWatch`
whose closed channel publishes the result), rather than letting the two race for it.
