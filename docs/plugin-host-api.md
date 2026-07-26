# Plugin host API — a reverse channel from plugins into Nine

- **Status:** Proposed (design note). Nothing here is built yet.
- **Date:** 2026-07-25.
- **Motivation:** let a running plugin **call back into its Nine instance** — to
  read memory on demand, to report events (e.g. "the long job finished"), and to
  persist and reuse **its own** working data — over an authenticated,
  capability-scoped channel. Today the transport is strictly one-directional
  (daemon → plugin); there is no way back.
- **Depends on:** the plugin HTTP-over-Unix-socket transport
  (`docs/plugins-http-transport.md`), the memory store (`internal/memory`,
  `spec/contracts/memory-store.md`), the notification feed
  (`internal/memory/notifications.go`, `user_notifications.go`), and — for the
  event-report path — the session journal / subscriptions
  (`docs/event-log.md`, `docs/reactive-events.md`).

---

## 1. TL;DR — recommendation

Add a **host API**: a small `host.*` RPC surface the daemon serves on its own
Unix socket, which a plugin dials when it wants something from Nine. It reuses
the existing HTTP-on-a-Unix-socket + JSON-RPC transport, just **inverted** — the
daemon becomes the server, the plugin the client. Every call carries a
**per-plugin capability token** handed to the plugin at spawn; the daemon
enforces a **default-deny, config-declared** grant set keyed to that token.

Ship the capabilities the real use cases need, and split write by **who reads it
back** (§6) rather than banning it outright:

- `host.memory.get` / `host.memory.list` — **read**, namespace-scoped.
- `host.notify` — inject a notification / event (the "I'm done" path).
- `host.memory.set` — **write**, namespace-scoped. Safe and useful when a plugin
  writes to its **own** namespace as private state it reads back itself (§3 case
  C); hazardous only when the written data reaches the *agent's* trusted context
  (§6). So write ships for plugin-private storage and stays walled off from
  agent-facing stores.

Keep this **entirely separate** from the tool-output overflow-spill idea (§2).

---

## 2. Not this: the tool-output spill is a different feature

This design was prompted alongside a second idea — spilling over-cap tool output
into memory and returning the model a key. **They are independent, and coupling
them is a mistake.**

The output cap lives in the daemon's dispatcher (`internal/agent/dispatcher.go`,
`capOutput`, `maxOutputTokens = 2048` ≈ 8192 chars). At that point the daemon
**already holds the full output** and direct access to `memory.Store` — so the
spill is a purely daemon-internal interception. It needs no reverse channel, no
plugin involvement, and no access-control model. It is a smaller, separate piece
of work and should be designed and shipped on its own. This document is only
about the reverse channel.

---

## 3. Why — the two use cases, and what each actually needs

| Use case | Direction | Surface it needs |
|---|---|---|
| A security plugin, notified of a CVE out-of-band, reads a memory key to get scan context | **pull**, read-only | `host.memory.get` (scoped read) |
| A long-running process reports "I'm done" | **push**, event | `host.notify` (not a memory write) |
| A plugin persists and reuses **its own** state across calls/restarts — scan history, cursors, last-seen timestamps, cached lookups | **pull + push**, private | `host.memory.set` / `host.memory.get` on the plugin's own namespace |

The second case is an important tell. **"I'm done" is an event, not a memory
mutation.** Nine already has the machinery to carry it: `NotificationCreate` /
`UserNotificationCreate` (`internal/memory/notifications.go`,
`user_notifications.go`) and the subscribable journal (`docs/reactive-events.md`).
Routing a completion signal through raw memory *writes* would bypass that and
land untrusted text in a store later turns read as fact. So it goes through
`host.notify` instead.

The third case is the reason **not** to ban write outright. A plugin often needs
somewhere durable to keep **its own** working data — a security scanner
remembering which CVEs it has already processed, a poller keeping a cursor, a
plugin caching an expensive lookup. Rather than every plugin inventing its own
on-disk store (paths, format, cleanup), the host API can give it a scoped
key-value space inside Nine's memory: `host.memory.set`/`get` confined to the
plugin's own namespace. This data is **written and read back by the same plugin**
and never surfaced to the agent as context — so it carries none of the injection
risk that agent-facing writes do (§6).

The consequence for the access model: it is not "read is safe, write is
dangerous." It is **who reads the write back**. Plugin-private writes (case C) are
safe; writes that land in agent-trusted stores are the hazard (§6). Case A is a
scoped read, case B is a notify, case C is a scoped *private* write — and only
writes into agent-facing memory are gated hardest.

---

## 4. The missing primitive — a plugin → daemon call

Today the daemon dials the plugin. The plugin is a **server only**: `plugin.Serve`
listens on `NINE_PLUGIN_SOCKET` and answers `plugin.describe` / `plugin.call`
(`internal/plugin/serve.go`); the manager drives it with an `http.Client`
(`internal/plugin/manager.go`, `Manager.Call`). The plugin is handed **no handle
back** to the daemon — only its own socket path and `NINE_BIN`.

Two properties shape the reverse channel:

1. **The transport is already symmetric.** `serve.go` (server side) and
   `client.go` / `httpclient.go` (client side) are a matched pair: HTTP on a Unix
   socket, JSON-RPC envelope, no request `id` (per-connection correlation). The
   host API is the *same* transport with the roles swapped — the daemon runs a
   `serve`-shaped handler, the plugin uses a `client`-shaped caller. Most of the
   code is reusable; this is not a new protocol family.

2. **Host calls happen outside any `plugin.call`.** "Ping on demand after a CVE"
   means the plugin is running its **own background loop** and calling in when
   *it* decides — possibly with no turn in flight and no conversation context to
   inherit. So a host call cannot lean on an ambient request context the way
   `plugin.call` handlers do; it must **carry its own identity** (the capability
   token) and name any scope (e.g. conversation id) explicitly in its params.

---

## 5. Transport & handshake

- **New socket, not the plugin's.** The daemon serves `host.*` on a dedicated
  host-API Unix socket, distinct from both the daemon↔client wire-protocol socket
  (`/tmp/nine.sock`) and each plugin's own `/tmp/nine/<name>.<rand>.sock`. Keeping
  it off the wire-protocol socket keeps plugin auth out of the CLI/TUI protocol
  (`spec/contracts/wire-protocol.md`) — those are different trust domains.
- **Spawn-time env.** The manager passes two new variables when it spawns a
  plugin (alongside `NINE_PLUGIN_SOCKET`, `NINE_BIN`):
  - `NINE_HOST_SOCKET` — path to the host-API socket to dial.
  - `NINE_HOST_TOKEN` — an opaque per-plugin capability token, freshly minted per
    process (mirror `allocSocketPath`'s `crypto/rand` pattern). It is the plugin's
    identity and the key the daemon looks up grants under.
- **Opt-in.** A plugin that never dials the host socket is unaffected; existing
  plugins need no change. The client half ships as a small helper in the plugin
  SDK (e.g. `plugin.Host()` reading the two env vars), so plugin authors don't
  hand-roll the socket call.
- **macOS `sun_path` limit (104 bytes)** applies here too — keep the host socket
  under `/tmp/nine/` like the plugin sockets, not `$TMPDIR`.

### Wire contract (sketch)

```
POST http://unix/host        (over NINE_HOST_SOCKET)
  header: Authorization / X-Nine-Host-Token: <NINE_HOST_TOKEN>
  body:   {"method":"host.memory.get","params":{"key":"cve/last-scan"}}
  reply:  {"result":{...}}                          (success)
       |  {"error":{"code":N,"message":"..."}}      (failure; e.g. -32001 denied)
  HTTP status: always 200; errors live in the body envelope.
```

This is a **new wire contract** → bump `plugin.ProtocolVersion`
(`internal/plugin/contract.go`) and document it in `spec/contracts/plugin.md`.
It does **not** touch `spec/contracts/wire-protocol.md` (daemon↔client), because
the host API is on its own socket.

### Methods (initial set)

| Method | Grant required | Notes |
|---|---|---|
| `host.memory.get`  | `memory: read`  | Single key, confined to the plugin's `memory_scope` prefix. |
| `host.memory.list` | `memory: read`  | Keys under a prefix, itself confined to `memory_scope`. |
| `host.notify`      | `notify: true`  | Injects a user/agent notification or a journal event. |
| `host.memory.set`  | `memory: read-write` | Scoped write. Safe into the plugin's **own private namespace** (§3 case C); writes into agent-facing memory are gated hardest and deferred (§6). |

---

## 6. Access levels & the scoping hazard

This is where "different levels of access, through config" lives — and it is the
part to get right, because a plugin is a **separate, possibly third-party
process** and Nine's memory is the agent's brain (goals, reflections, self-model,
conversation history).

### Config: per-plugin, default-deny

Grants are declared per plugin in `nine.toml` (see `internal/config`), enforced
daemon-side at the host-API boundary, keyed to the plugin's token:

```toml
[plugins.security-scanner]
memory       = "read"      # none (default) | read | read-write
memory_scope = "cve/"      # prefix the grant is confined to; required for read/write
notify       = true        # may inject notifications / events
```

A plugin with no stanza gets nothing. `memory = "read"` without a `memory_scope`
is a config error, not "read everything" — broad read must be an explicit,
visible choice, never a default. `memory_scope` is also the plugin's **private
namespace** for case C: a `read-write` grant lets it read and write freely within
that prefix and nowhere else.

### The hazard is *who reads the write back*, not write itself

The risk is not a plugin storing its own data. It is a plugin **writing
attacker-influenced text into a store that a later *turn* reads as trusted
context** — prompt injection laundered through Nine's own memory. The two are
distinguished by who consumes the write:

- **Plugin-private write (§3 case C) — safe.** A plugin writes under its own
  `memory_scope` and reads it back itself; the agent's context builder never
  surfaces that namespace. The content is never interpreted as first-party fact,
  so its trustworthiness is irrelevant. This is a plain scoped key-value store and
  ships as such.
- **Agent-facing write — the hazard, deferred.** Writing anything the agent will
  later read as context is the dangerous path. Guardrails, when it is ever built:
  - **Agent-trusted stores are never plugin-writable** — reflections, self-model,
    goals, conversation history are off-limits to `host.memory.set` regardless of
    grant.
  - **Plugin-written memory that can reach the agent is tainted** — entries carry
    an origin marker (which plugin wrote them) so the context builder and agent
    treat it with skepticism, not as first-party fact. (Needs a small schema
    addition to KV — no origin column today; see `internal/memory/kv.go`.)

The enforcement primitive is the same for both: writes are confined to the
plugin's `memory_scope`, and the context builder is what decides whether a
namespace is ever agent-visible. Keep plugin-private namespaces out of the
context builder's reach and case C needs none of the taint machinery.

- **Reads are scoped too.** `host.memory.get` outside the plugin's `memory_scope`
  is denied — a read grant is not a licence to read the agent's private state.

### Capability tiers (what actually ships)

- **`read`** (scoped) — covers use case A. Ships in phase 1.
- **`notify`** — covers use case B via the existing notification/journal path,
  needing zero memory write. Ships in phase 1.
- **`read-write`** (scoped to a plugin-private namespace) — covers use case C:
  plugins keeping their own durable state. Ships once the namespace is proven
  invisible to the context builder.
- **Agent-facing write** (a plugin writing into memory a turn reads as context) —
  covers no motivating case; gated hardest, tainted, and deferred until a concrete
  need justifies it.

---

## 7. Phases

### Phase 1 — Host transport + auth (daemon side)
Daemon opens the host-API Unix socket and serves a `host` mux (reuse the
`serve.go` envelope helpers). Mint a per-plugin token at spawn, pass
`NINE_HOST_SOCKET` + `NINE_HOST_TOKEN`, and maintain a token → grants table from
config. Every `host.*` call is authenticated and default-denied. No methods with
side effects yet beyond a `host.ping` for the handshake test.

### Phase 2 — `host.memory.get` / `host.memory.list` (read, scoped)
Wire the read methods to `memory.Store` (`Get`/`List`, `internal/memory/kv.go`),
enforcing `memory_scope` on both. This closes use case A end-to-end. Add the
`plugin.Host()` SDK helper so a Go plugin reads a key in a few lines.

### Phase 3 — `host.notify` (event/report, no write)
Route to `NotificationCreate` / `UserNotificationCreate` (or a journal event, per
`docs/reactive-events.md`'s pull-not-push discipline — a report enriches the feed;
it does not interrupt a live turn). This closes use case B. Decide whether a
report targets a specific conversation (param) or the instance-wide user feed.

### Phase 4 — `host.memory.set` for plugin-private storage (scoped write)
Wire the write method to `memory.Store`, confined to the plugin's `memory_scope`,
behind a `read-write` grant. This closes use case C — a plugin gets a durable
key-value space of its own. The one prerequisite is confirming those namespaces
are **invisible to the context builder** (so a private write can never reach the
agent), which lets case C skip the taint machinery entirely.

### Phase 5 (deferred) — agent-facing write (tainted)
Letting a plugin write into memory the agent reads as context. Requires the KV
origin/taint column (§6) and the never-over-trusted-stores restriction. Covers no
current use case; design its own note before building.

### Phase 6 — Docs & spec
`spec/contracts/plugin.md` gains the host-API contract and the `ProtocolVersion`
bump; `docs/plugins.md` documents the SDK helper and the config grants; this note
flips from Proposed to Implemented. Run `/sync-nine`.

---

## 8. Decisions taken

- **Host API on its own socket**, not the daemon↔client wire protocol — keeps
  plugin auth out of the CLI/TUI trust domain.
- **Default-deny, per-plugin config grants**, keyed to a per-process token.
- **"I'm done" is a notification/event, not a memory write** — case B never
  touches write.
- **Write is split by who reads it back.** Plugin-private writes (a plugin's own
  scoped namespace, case C) ship as a plain key-value store; writes into
  agent-facing memory are the hazard — tainted, never over trusted stores, and
  deferred until a concrete need appears.
- **`memory_scope` is both the read/write confinement prefix and the plugin's
  private namespace**; the context builder must never surface plugin-private
  namespaces.
- **Independent of the tool-output spill** (§2), which is daemon-internal.

## 9. Open questions

1. **Notification target.** Does `host.notify` post to a named conversation, to
   the instance-wide user feed, or both? Leaning: user feed by default, optional
   conversation id.
2. **Token lifetime.** Fresh per process each spawn (simple, no persistence) vs. a
   stable per-plugin token in config (lets an external cron-driven plugin
   authenticate without being daemon-spawned). Leaning: per-process for
   daemon-spawned plugins; revisit if an out-of-process caller ever needs in.
3. **Rate limiting / abuse.** A background loop hammering `host.memory.get` — do
   we need a per-token rate cap, or is a scoped read cheap enough to ignore at
   first?
4. **MCP plugins.** MCP servers are external and stdio-based (`internal/plugin/mcp.go`)
   — is the host API native-plugin-only (likely yes for v1), or does MCP get a
   path in too?
