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
plugin.describe → returns {protocol_version, max_concurrent, tools: [ {name, description, inputSchema [, display_name]} ]}
plugin.call({tool, args}) → returns {output}   (or an error envelope)
```

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

---

## R-PLUG.3 — Manager lifecycle

The manager owns subprocess lifecycle:

```text
Start(binaryPath, extraEnv…):
   allocate a per-plugin Unix socket path; pass it via NINE_PLUGIN_SOCKET (+ NINE_BIN, extras)
   spawn process; dial the socket with bounded retry (~1s budget)
   → plugin.describe  → {tool defs, max_concurrent}
   set the per-plugin HTTP transport's MaxConnsPerHost from max_concurrent (0 = unbounded; R-PLUG.8)
   track *Plugin{Name, client, Tools}
   register each tool with the dispatcher (handler = manager.Call(plugin, tool, args))
on each tool invocation:  → plugin.call{tool,args} → {output}   (its own pooled connection)
Stop:  SIGTERM, wait for clean exit (kill after grace), unlink the socket
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
> **Skills are also core-intercepted.** `skill_list/read/write/modify` are store-backed
> core handlers with binary-embedded immutable defaults — see [`skills.md`](skills.md).
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

## R-PLUG.7 — No runtime plugin mutation (N1)

There **MUST NOT** be any tool or path that writes plugin source, builds a plugin,
starts a new plugin binary, hot-swaps, or rolls back a plugin at runtime. To add a
capability, add a plugin to the source repo and rebuild the image.

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

## Reference symbols

`internal/plugin/manager.go` (`Manager`, `Start`, `Call`, `TryStart`),
`internal/plugin/` (`client.go` `newHTTPClient` — native HTTP transport; `client`/`mcp.go`
— stdio, retained for MCP; `serve.go` `plugin.Serve` — the plugin-side HTTP server loop on
`NINE_PLUGIN_SOCKET`; `contract.go` — `ToolDefinition`/`DescribeResult`),
`plugins/{files,shell,http,time,browser}/`.
