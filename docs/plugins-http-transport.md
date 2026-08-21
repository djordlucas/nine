# Plugin transport: HTTP over a Unix socket

- **Status:** Implemented. Native plugins serve `plugin.describe` / `plugin.call`
  as `POST /rpc` over the Unix socket named by `NINE_PLUGIN_SOCKET`
  (`internal/plugin/serve.go`, `plugin.Serve`); the manager drives them with an
  `http.Client` (`internal/plugin/client.go`, `newHTTPClient`). MCP servers keep
  the legacy stdio JSON-RPC client.
- **Date:** 2026-06-19 (design); implemented since.
- **Superseded in part:** every reference below to a `browser` plugin is historical.
  Nine no longer ships one — browser automation is an `[[mcp.server]]`
  ([browser.md](browser.md)) — so the "plugin with shared mutable state that must
  declare `max_concurrent: 1`" example is now the MCP bridge rather than `browser`.
  MCP has also moved behind that same plugin contract (spec R-PLUG.15); it is no
  longer a separate stdio client in the core.
- **Motivation:** let a single plugin process handle **many concurrent requests** instead
  of serializing them, without spawning a process pool.

---

## Why

Today each plugin is **one long-lived process** (spawned once at boot, reused — there is
no per-request spawn), but requests to it are **serialized** in two places:

- **Client** (`internal/plugin/client.go`, `client.call`): holds `c.mu` for the entire
  request→response round trip, so only one call per plugin is ever in flight.
- **Server** (`internal/plugin/serve.go`, `Serve`): the stdin read loop runs each handler
  synchronously before reading the next line.

This contention bites exactly where Nine is parallel by design: `run_agents` fan-out, plus
concurrent conversations and background goal/reflection sessions all sharing the one
`shell`/`http`/`files` process.

Rather than hand-roll a multiplexer over stdio (reader goroutine + pending-by-id map +
write lock + fail-all-on-EOF) or run a pool of processes, switch the **native plugin
transport to HTTP over a Unix socket**. Each concurrent call gets its own pooled
connection — the OS/`http` stack does the multiplexing — so:

- the client multiplexer is unnecessary (`http.Client` pools connections, safe for
  concurrent use);
- the server runs one goroutine per request for free (`http.Server`);
- **bounded concurrency** is one field (`Transport.MaxConnsPerHost`);
- **per-request cancellation / deadlines** come from `context` (the main reason to bother
  — ties into `task_timeout`).

What this does **not** fix: handler concurrency-safety is orthogonal to transport. A plugin
with shared mutable state (e.g. `browser`'s single page) is still unsafe to run
concurrently and must stay serial regardless.

---

## Constraints discovered in the code (these shape the design)

1. **Everything goes through the `pluginClient` interface** (`call(method, params)` +
   `stop()`), implemented by the native stdio `client` and by `mcpClient` (which *wraps*
   the stdio client). `Manager`, `Plugin`, the dispatcher, and `Call`/`RawCall` operate
   through this interface — so the transport swap lives entirely behind it.
   - **`sendNotification` is not on the interface.** It's a method on the concrete stdio
     `client`, called directly by `mcpClient` for the MCP `notifications/initialized`
     handshake (`mcp.go:93`). Since MCP keeps the stdio `client` untouched, it stays put;
     the new `httpClient` does **not** need it (native plugins never receive notifications).
2. **MCP must stay on stdio.** MCP servers are external processes we do not control,
   speaking stdio JSON-RPC. So we **add** an HTTP transport for native Nine plugins and
   **keep** the stdio `client` for MCP. This is an addition, not a replacement of all
   stdio code.
3. **The only two methods that actually flow are `plugin.describe` and `plugin.call`.**
   `serve.go`'s switch handles exactly those; anything else (including `internal.*`) hits
   `default` → "method not found". The lone `RawCall("internal.workflow.fail", …)` caller
   (`internal/cli/commands.go`) is dead code — see Resolved findings #1 — so the HTTP server needs
   **no generic method route**. The wire contract is just `describe` + `call`.
4. **Go plugins all call `plugin.Serve(tools, handlers)`**; only `browser` (JS,
   `readline` loop in `plugins/browser/index.js`) is bespoke. Boot wiring is five
   `TryStart` calls in `cmd/nine/daemon.go:30-34`.

---

## Wire contract

A single endpoint with the JSON-RPC envelope preserved, so the existing
`call(method, params)` maps 1:1. Only `plugin.describe` and `plugin.call` are sent (see
Resolved findings #1 — `internal.*` is dead, so no generic route is needed; `internal/cli/commands.go`):

```
POST http://unix/rpc
  body:  {"method": "<plugin.describe | plugin.call>", "params": {...}}
  reply: {"result": {...}}                          (success)
      |  {"error": {"code": N, "message": "..."}}   (failure)
  HTTP status: always 200; errors live in the body envelope.
```

No request `id` — correlation is per-connection now, which is the point.

---

## Conventions

- **Socket path:** daemon-allocated, passed to the plugin via the `NINE_PLUGIN_SOCKET`
  environment variable. The plugin listens there; the daemon dials it.
  - ⚠️ **macOS `sun_path` limit (104 chars).** `$TMPDIR` on darwin is long
    (`/var/folders/…`), which can overflow it. Use a short fixed dir, e.g.
    `/tmp/nine/<name>.<rand>.sock`. Lock this in.
    - Note this hardcodes the dir, ignoring the configurable `cfg.SocketPath()` (which only
      governs the *daemon's* own socket, default `/tmp/nine.sock`). Accepted trade-off for
      the `sun_path` limit; known limitation is that the `0700` `/tmp/nine` created by one
      user blocks another user on a shared machine.
  - **Dir creation:** new code, none exists today (Resolved findings #2). Daemon does
    `os.MkdirAll("/tmp/nine", 0700)` before spawning — idempotent, so a pre-existing dir
    from a prior run is fine. Note `/tmp/nine` (dir) does not collide with the daemon's own
    `/tmp/nine.sock` (file). Mirror the stale-socket pattern already in
    `runtime/daemon.go`: `os.Remove(path)` before `net.Listen("unix", …)`.
- **Concurrency:** a plugin advertises `max_concurrent` in its `describe` result, mapped
  straight onto `Transport.MaxConnsPerHost` — including Go's native `0` = **unbounded**.
  A stateless plugin such as `shell` advertises `0`: its handlers are safe
  to run fully concurrent, so there's no reason to cap them at an arbitrary N. Only a
  plugin with shared mutable state declares a finite cap — `browser` sets `1` (single
  page). Bounding on the client side means the server needs no semaphore.
  - ⚠️ The cap is **opt-in to serialize**, not opt-in to parallelize: a new plugin that
    forgets to set `max_concurrent` gets unbounded concurrency. Any plugin with shared
    state *must* declare its cap. (We use `0`=unbounded rather than defaulting to `1`
    because the tools we ship are stateless; capping them by default would just
    reintroduce the serialization this whole change removes.)
- **Readiness:** the daemon dials with retry (~1s budget). If the socket never comes up,
  `Start` fails cleanly — same outcome as a failed `describe` today.
- **Cleanup:** the plugin unlinks a stale socket before listening and on exit; the manager
  best-effort unlinks on `Stop`. Process lifecycle is unchanged (still a child process;
  `cmd.Wait` for crash detection → plugin-isolation invariant preserved).
- **Debugging:** a live plugin is reachable like any microservice — no stdio shim needed:
  ```bash
  curl --unix-socket /tmp/nine/shell.<rand>.sock \
    -X POST http://unix/rpc \
    -d '{"method":"plugin.call","params":{"tool":"shell","args":{"cmd":"echo hi"}}}'
  ```

---

## Phases

### Phase 1 — Server transport (`internal/plugin/serve.go`)
`Serve` runs `http.Serve` on a Unix listener (from `NINE_PLUGIN_SOCKET`) with a `/rpc` mux
that reuses the existing `plugin.describe` / `plugin.call` switch logic, now one goroutine
per request (free from `http.Server`). **No stdio fallback** — HTTP is the only native
transport; if `NINE_PLUGIN_SOCKET` is unset, `Serve` fails fast. (The stdio loop is
deleted; MCP keeps its own separate stdio client, untouched.)

No generic `internal.*` route — those methods are dead (Resolved findings #1).

`Serve` gains a way to advertise `max_concurrent` in the describe result (Resolved findings
#3): add `MaxConcurrent int` to `DescribeResult` and let plugins set it (variadic option
or `ServeOpts`). The zero value is `0` = unbounded, which is what the stateless tools want,
so they need no option at all. **Go plugin `main.go` files otherwise do not change** —
same `Serve(tools, handlers)` signature; only a stateful plugin passes an explicit cap.

### Phase 2 — Client transport (new `httpClient`, `internal/plugin/`)
Implements `pluginClient`. Holds an `*http.Client` whose `Transport.DialContext` dials the
Unix socket, plus the `*exec.Cmd`. `call` POSTs to `/rpc`; concurrency-safe by
construction (no mutex, no reader goroutine, no pending map). `stop` sends SIGTERM, `Wait`s
with a kill-after-grace, closes idle connections, unlinks the socket.

Keep a post-`stop` guard (mirrors the stdio client's `c.closed`): a `call` after `stop`
must fail cleanly with "plugin stopped" rather than dialing an already-unlinked socket.
An `atomic.Bool` set in `stop` and checked at the top of `call` is enough — no mutex on
the hot path.

### Phase 3 — Manager wiring (`internal/plugin/manager.go`)
`Start` builds an `httpClient`: allocate the socket path per plugin, set the env, spawn,
dial, `describe`, read `max_concurrent`, then build the request `Transport` with
`MaxConnsPerHost` set to that value.

⚠️ **Do not mutate `Transport.MaxConnsPerHost` after the first request.** `net/http`
forbids modifying a `Transport`'s fields once a request has been issued on it, and
`describe` *is* the first request. So `describe` must run on a throwaway/minimal client
(or a raw `net.Conn`), and only **after** reading `max_concurrent` do we construct the
real `*http.Client`/`Transport` the plugin will use for `call`s. Ordering the field write
"after describe" on the same transport is a data race.

`StartMCP` is untouched (stays stdio). `Plugin` and `Call` signatures unchanged except for
ctx (Phase 5); `RawCall` is removed (Resolved findings #1). This phase also fixes the dead
daemon-down `WorkflowFail` branch (`internal/cli/commands.go:209-227`) to use the
in-process `memory` package directly (`store.WorkflowFail`, as `daemon.go:260` already
does).

### Phase 4 — Browser plugin (`plugins/browser/index.js`)
The only real rewrite: replace the `readline` loop with a Node `http.createServer` bound
to the Unix socket from `process.env.NINE_PLUGIN_SOCKET`. The `readline` loop is removed
(no stdio fallback). Stays `max_concurrent: 1` (shared page state).

### Phase 5 — Context threading (recommended; separable commit)
The reason HTTP is cleaner. `call(ctx, …)`, `Manager.Call(ctx, …)`; the dispatcher passes
its ctx (carrying `task_timeout`). The HTTP request rides the ctx → real cancellation.
`ToolHandler` becomes `func(ctx, args)` so handlers like `shell` (already
`exec.CommandContext`) abort on cancel. Wider blast radius (dispatcher + every handler
signature), so it can land after Phases 1–4 are validated.

⚠️ **Changing `call(method, params)` → `call(ctx, …)` touches every `pluginClient`
implementation, including `mcpClient` and the stdio `client`.** Real cancellation only
materializes for native HTTP plugins (the ctx rides the HTTP request). The stdio `client`
blocks on `stdout.Scan()` under its mutex, which a ctx cannot interrupt — so **MCP gets
ctx as a no-op / best-effort signal**, not true mid-call cancellation. State this
explicitly so the asymmetry isn't mistaken for a bug.

**Trace ID for log correlation (rides on this phase).** Do *not* reintroduce the
JSON-RPC body `id` — HTTP already correlates reply↔request per connection, and the body
`id` only ever fed the deleted stdio multiplexer. For *debuggability* once concurrency is
on (Phase 6), correlate logs instead via a **header**: the client puts a per-call trace ID
(`X-Nine-Request-ID`, or W3C `traceparent`) carried on the ctx, the server reads it onto
its `slog` context, and both sides log the same string → grep one ID across daemon and
plugin logs. No envelope change.

### Phase 6 — Turn on concurrency + tests
Leave `shell`/`http`/`files`/`time` unbounded (`max_concurrent: 0`, the default); leave
`browser` at 1.
Tests:
- **Concurrency:** N concurrent `call`s to a sleep handler — wall-time ≈ one sleep when
  unbounded (or `MaxConnsPerHost≥N`), ≈ N×sleep at a cap of 1.
- **Crash isolation:** kill the plugin mid-call → all in-flight calls error; manager
  reports failure without crashing the host.
- **Regression:** existing `manager_test.go`, `tests/integration/*`, and the MCP
  `testmcpserver` path must stay green.

---

## Defaulted decisions (override if desired)

- **No stdio fallback.** HTTP is the sole native transport; the stdio `Serve` loop and the
  browser `readline` loop are deleted. (MCP's separate stdio client is unaffected.)
- **`max_concurrent: 0` (unbounded) for the stateless tools**, not a finite cap — their
  handlers are concurrency-safe, so there's nothing to serialize. A finite cap is reserved
  for plugins with shared state (`browser`: 1). Capping is opt-in to *serialize*.
- **Phase 5 (ctx) as a separate commit** so Phases 1–4 land and are validated first.
- **Convert `shell` first** end-to-end (the `Manager` can choose transport per plugin
  during migration), prove it concurrent, then flip the rest.

---

## Resolved findings (verified in code, 2026-06-20)

1. **Memory "plugin" is dead code — no generic route needed.** `internal/memory/` is an
   **in-process Go package** (`memory.Open(cfg.MemoryPath())`, `cmd/nine/daemon.go:22`),
   not a plugin. There is no `plugins/memory/` source and the Makefile builds only
   `shell files http time` (+ browser), so `cfg.PluginBin("memory")` names a binary that is
   never built. The sole caller of both `PluginBin("memory")` and `RawCall` is the
   daemon-down branch of `WorkflowFail` (`internal/cli/commands.go:209-227`), which is doubly dead:
   the binary can't spawn, *and* `serve.go` has no `internal.*` route (it would hit
   `default` → "method not found"). **Action:** fix that branch to use the in-process
   `memory` package directly, then drop `Manager.RawCall` and the generic-route idea
   entirely. Wire contract = `describe` + `call`.
2. **Socket dir is greenfield.** No `/tmp/nine/` logic exists. The daemon's own socket is a
   *file* `/tmp/nine.sock` (`config/factory.go:14`) — no collision with a `/tmp/nine/`
   dir. Pattern to mirror: `runtime/daemon.go:134-137,177` (`os.Remove` → `net.Listen` →
   `os.Remove` on shutdown). Need new `os.MkdirAll("/tmp/nine", 0700)` (idempotent) before
   spawn.
3. **`max_concurrent` is not in the contract yet.** `DescribeResult` (`contract.go:24`)
   holds only `Tools`. Add `MaxConcurrent int \`json:"max_concurrent,omitempty"\``, let
   `Serve` set it (zero value `0` = unbounded — same as `MaxConnsPerHost`'s native
   semantics, so it feeds straight through), and read it where the manager already
   unmarshals `desc` (`manager.go:66`) to set `MaxConnsPerHost`. Note `omitempty` makes
   `0` and "absent" indistinguishable on the wire — fine, because both mean unbounded.

---

## Scope notes

- The MCP adapter (`internal/plugin/mcp.go`) and its stdio `client` are **unchanged**.
- No change to the agent loop, dispatcher routing, or tool semantics (except the optional
  ctx thread in Phase 5).
- Docs: `spec/contracts/plugin.md` is updated up-front with the HTTP transport, the
  envelope, and `max_concurrent` (R-PLUG.8), carrying a "Transport migration" note so it
  describes the target while the reference tree still ships stdio. `plugins.md` is
  still to update once shipped. `spec/contracts/wire-protocol.md` is **not** affected — it
  covers the daemon↔client `Msg` stream, not the daemon↔plugin transport.
