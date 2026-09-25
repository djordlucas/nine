# The plugin transport

A plugin is a subprocess that provides tools. The daemon talks to it over
**HTTP on a Unix socket** — one endpoint, `POST /rpc`, carrying a JSON-RPC
envelope.

## Why HTTP over a socket

The point is **concurrency without a process pool**. A single plugin process can
serve many requests at once, which a line-oriented pipe cannot: with one stream
you either serialize calls or invent request correlation to interleave them.
HTTP already solved that, and connection-per-request means correlation is
handled by the connection rather than by an id in the payload.

Everything else follows from reusing a mature protocol. Cancellation rides the
request context, so an abandoned turn closes the connection and the plugin sees
it. A live plugin is debuggable like any other service:

```bash
curl --unix-socket /tmp/nine/shell.<rand>.sock \
  -X POST http://unix/rpc \
  -d '{"method":"plugin.call","params":{"tool":"shell","args":{"cmd":"echo hi"}}}'
```

A Unix socket rather than a TCP port because the transport must not be
reachable off the machine, and a filesystem path gets that from the operating
system instead of from a bind address someone can get wrong.

## The contract

```
POST http://unix/rpc
  body:  {"method": "<plugin.describe | plugin.call
                     | plugin.job_status | plugin.job_cancel>", "params": {…}}
  reply: {"result": {…}}                          success
      |  {"error": {"code": N, "message": "…"}}   failure
```

The HTTP status is **always 200**; failures live in the body envelope. Four
methods, and only the first two are required:

| Method | Does | Since |
|---|---|---|
| `plugin.describe` | announce the tools, the advertised `max_concurrent`, and the protocol version | v1 |
| `plugin.call` | run one tool | v1 |
| `plugin.job_status` | report on detached work the daemon is polling | v2 |
| `plugin.job_cancel` | ask for detached work to stop | v2 |

A v1 plugin that implements neither job method remains fully functional — the
daemon accepts protocol `{1, 2}` and treats a v1 plugin as one without jobs
([versioning.md](versioning.md#2-plugin-protocol-version)).

There is no request id: correlation is per-connection, which is what buys
concurrency without a correlation scheme in the payload.

## Concurrency is opt-in to serialize

A plugin advertises `max_concurrent` when it describes itself, and the daemon
bounds its own connections to match — so the plugin needs no semaphore of its
own.

Zero means **unbounded**, and zero is the default. That direction is
deliberate: the tools Nine ships are stateless, and defaulting to one would
reintroduce exactly the serialization this transport exists to remove.

**A plugin that declares no cap gets unbounded concurrency.** Any plugin
holding shared mutable state must declare its own limit.

## Sockets and startup

The daemon allocates the socket path and passes it to the plugin in the
environment; the plugin listens, the daemon dials.

Paths live under a short fixed directory rather than the system temporary
directory, because macOS caps a Unix socket path at 104 bytes and the darwin
temporary path is long enough to overflow it. The trade-off is a hardcoded
directory, and the known limitation is that the private directory created by
one user blocks another on a shared machine.

The daemon dials with retry while the plugin starts. A process that exits
before listening fails immediately with its own exit status, rather than
waiting out the readiness budget and reporting a socket timeout.

Stale sockets are removed before listening and on exit. Process lifecycle is
unchanged: a plugin is still a child process, still watched, so a crash is still
isolated to that plugin.

## MCP servers

An MCP server reaches Nine through this same plugin contract rather than as a
separate client in the core. Servers that run as a subprocess speak stdio;
hosted ones are reached over HTTP at a URL. Either way the daemon sees a plugin.

## Limits

| Limit | Detail |
|-------|--------|
| Unbounded concurrency by default | `max_concurrent` defaults to 0, meaning unbounded. A plugin with shared mutable state that forgets to declare a cap is raced. Defaulting to 1 would reintroduce the serialization this transport removes. |
| Hardcoded socket directory | Socket paths live under a short fixed directory because macOS caps a Unix socket path at 104 bytes and the darwin temporary path overflows it. The private directory created by one user blocks another on a shared machine. |
| One direction only | The daemon dials the plugin; a plugin never calls back into the daemon. A plugin reports completion by being asked, not by pushing. |
| No hot reload of a running plugin | A settings change takes effect on `nine plugins reload` for user plugins, or a daemon restart for built-ins. |

## Related

- [Plugins](plugins.md) — what a plugin is, and writing one
- [Plugin capabilities](plugin-capabilities.md) — settings, cache directories, long-running jobs
- [`spec/contracts/plugin.md`](../spec/contracts/plugin.md) — the normative contract

> Why HTTP was chosen over the alternatives, and the constraints that shaped it
> — [../adr/plugin-http-transport-design.md](../adr/plugin-http-transport-design.md).
