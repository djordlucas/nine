# The plugin transport

A plugin is a subprocess that provides tools, and it speaks **MCP** — the only
wire protocol that crosses a process boundary (adr/modular_nine.md). Two
transports, one per plugin:

- **unix-socket HTTP** (the default): MCP streamable HTTP at `POST /mcp` on the
  Unix socket named by `NINE_PLUGIN_SOCKET`. Concurrent — one plugin process
  serves many requests at once.
- **stdio** (opt-in, `transport = "stdio"` in the manifest): JSON-RPC, one line
  per message, over the process's stdin/stdout. Serial — a response can only be
  matched to its request by owning the stream for the round trip — so a stdio
  plugin is registered with `max_concurrent` 1.

## Why HTTP over a socket

The point is **concurrency without a process pool**. A single plugin process can
serve many requests at once, which a line-oriented pipe cannot: with one stream
you either serialize calls or invent request correlation to interleave them.
HTTP already solved that, and JSON-RPC ids carry the correlation in the payload.

Everything else follows from reusing a mature protocol. Cancellation rides the
request context, so an abandoned turn closes the connection and the plugin sees
it. A live plugin is debuggable like any other service:

```bash
curl --unix-socket /tmp/nine/shell.<rand>.sock \
  -X POST http://unix/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{"command":"echo hi"}}}'
```

A Unix socket rather than a TCP port because the transport must not be
reachable off the machine, and a filesystem path gets that from the operating
system instead of from a bind address someone can get wrong.

## The contract

```
POST /mcp  (or a newline-delimited line on stdio)
  body:  {"jsonrpc":"2.0","id":1,"method":"<initialize | notifications/initialized
                                           | tools/list | tools/call>","params":{…}}
  reply: {"result": {…}}                          success
      |  {"error": {"code": N, "message": "…"}}   JSON-RPC error
```

Streamable HTTP answers each POST with either a JSON body or an SSE stream
(`text/event-stream`); the client accepts both. Notifications (no id) are
acknowledged with 202 and no body. A `tools/call` result carries a content array
and an `isError` flag; the daemon flattens the content to one string and maps
`isError` to a Go error, so the model sees a failed tool call. Binary parts are
written to the plugin's cache dir and named in the text (never dropped).

The daemon may send an `X-Nine-Request-ID` header on the socket transport for
log correlation.

## Concurrency is a transport property

Socket HTTP is concurrent — `max_concurrent` 0, unbounded, and that direction is
deliberate: the tools Nine ships are stateless, and defaulting to one would
reintroduce exactly the serialization this transport exists to remove. stdio is
serial (`max_concurrent` 1). **A socket-HTTP plugin holding shared mutable state
must still serialize inside itself** — the transport does not protect it.

## Sockets and startup

The daemon allocates the socket path and passes it to the plugin in the
environment; the plugin listens, the daemon dials. Paths live under a short
fixed directory rather than the system temporary directory, because macOS caps a
Unix socket path at 104 bytes and the darwin temporary path is long enough to
overflow it. The trade-off is a hardcoded directory, and the known limitation is
that the private directory created by one user blocks another on a shared machine.

The daemon dials with retry while the plugin starts. A process that exits before
listening fails immediately with its own exit status, rather than waiting out the
readiness budget and reporting a socket timeout. For stdio plugins there is no
socket; the MCP handshake itself fails at once if the process dies before
answering, under a separate handshake budget (3 minutes by default, because
`npx`/`uvx` servers routinely take far longer than any socket budget to start).

Stale sockets are removed before listening and on exit. Process lifecycle is
unchanged: a plugin is still a child process, still watched, so a crash is still
isolated to that plugin.

## MCP servers

An external MCP server declared as `[[mcp.server]]` needs no plugin binary of
its own: the manager connects to it directly — spawning the `command` over stdio,
or POSTing to the `url` over streamable HTTP — and registers it as the plugin row
`mcp:<name>` with `<server>__`-prefixed tools. From the daemon's side it is the
same lifecycle as any plugin.

## Limits

| Limit | Detail |
|-------|--------|
| stdio is serial | `max_concurrent` 1; one client-side write lock owns the stream per round trip. |
| Unbounded concurrency by default on socket HTTP | A plugin with shared mutable state that runs on socket HTTP must serialize internally; the transport will not do it. |
| Hardcoded socket directory | Socket paths live under a short fixed directory because macOS caps a Unix socket path at 104 bytes and the darwin temporary path overflows it. The private directory created by one user blocks another on a shared machine. |
| One direction only | The daemon dials the plugin; a plugin never calls back into the daemon. A plugin reports completion by being asked, not by pushing. |
| No hot reload of a running plugin | A settings change takes effect on `nine plugins reload` for user plugins, or a daemon restart for built-ins. |

## Related

- [Plugins](plugins.md) — what a plugin is, and writing one
- [Plugin capabilities](plugin-capabilities.md) — settings, cache directories
- [`spec/contracts/plugin.md`](../spec/contracts/plugin.md) — the normative contract

> Why HTTP was chosen over the alternatives, and the constraints that shaped it
> — [../adr/plugin-http-transport-design.md](../adr/plugin-http-transport-design.md).
> MCP as the only cross-process wire protocol — [../adr/modular_nine.md](../adr/modular_nine.md).
