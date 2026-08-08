# Contract — Sandboxed Tools (the Wasm tool host)

**Status:** Built (stages 1–5) · **Depends on:** dispatcher (registration), config (grants + the generated ceiling), memory store (generated tools are rows), HITL (the optional write gate) · **Used by:** any turn calling a sandboxed tool

A **sandboxed tool** is a wasm module that the daemon executes **in-process**, with an
explicitly conferred set of capabilities and nothing else. It is a *second backend behind
the same dispatcher* as native plugins (`spec/contracts/plugin.md`), not a replacement:
the plugin contract, its transport, and `plugin.ProtocolVersion` are untouched, and a
deployment that leaves `[tools] enabled` unset behaves exactly as it did before this
subsystem existed.

The design rationale is `docs/sandboxed-tools.md`; the authoring guide is
`docs/writing-sandboxed-tools.md`. This file is normative.

> **Two authors, one runtime.** A **developer tool** is a file on disk with a manifest,
> installed by the operator (R-TVM.10). A **generated tool** — authored by Nine itself via
> `tool_write` — is a row in the store (R-TVM.14). Both run through the *same* host, ABI,
> capability model, and audit trail; the only differences are where the code comes from and,
> for a generated tool, that the operator confers a **ceiling** rather than a per-tool grant.
> Requirements R-TVM.1–R-TVM.12 hold for both kinds unless they say otherwise; R-TVM.14 adds
> what is specific to the generated tier.

---

## R-TVM.1 — The guest ABI

A tool is a wasm module exporting exactly two functions. `toolvm.ABIVersion` is **1**.

```text
nine_alloc(size i32) -> i32
  Reserve size bytes of the guest's linear memory; return the offset. The host writes
  the call's input there before calling nine_run.

nine_run(ptr i32, len i32) -> i64
  Run the tool against the len bytes of UTF-8 JSON at ptr. Return the result packed as
  (offset << 32) | length.
```

There is **no `free`**: the instance is destroyed when the call returns (R-TVM.3), so
every allocation is reclaimed wholesale.

Everything crossing the boundary is a UTF-8 JSON byte slice — no struct marshalling, no
proxy objects, no reference counting — because the tool contract it must satisfy already
is that narrow (`CallRequest{Tool, Args} → CallResult{Output}`).

The result **MUST** be one of:

```json
{"ok": true,  "output": "<the string the model sees>"}
{"ok": false, "error":  "<message>"}
```

A `false` result is surfaced to the model as an **ordinary tool error**, distinct from the
host failing to run the tool at all — the model can read it and retry with different
arguments.

`ABIVersion` is independent of `plugin.ProtocolVersion` (`docs/versioning.md`). A module
declaring an unsupported ABI **MUST** be refused at load, not called.

---

## R-TVM.2 — Two kinds, one contract

| Kind | Module | Built when |
|---|---|---|
| `wasm` | the developer's own module, from Rust, TinyGo, Zig, or C | at development time |
| `js` | the pre-supplied QuickJS-NG interpreter; the tool's source is its input | never — nothing is compiled at install time |

The host knows nothing about JavaScript. A `js` tool is an ordinary wasm tool whose
arguments happen to describe a JavaScript program, so both kinds share one ABI, one
capability model, one dispatcher registration, and one audit trail. QuickJS is an
implementation detail of one *kind*, not an architectural layer.

The daemon **MUST NOT** contain a compiler or a package manager for either kind. Adding
one would undo the no-toolchain property `docs/self-modification.md` insists on.

---

## R-TVM.3 — One instance per call

A module is **compiled once** and **instantiated per call**; the instance is closed when
the call returns.

This is the strongest property in the design and is required, not an optimization:
**no state survives a call.** Not a global, not a cached credential, not a poisoned
prototype, not a half-freed heap. Two calls to the same tool **MUST NOT** be able to
observe each other, and a tool **MUST NOT** be able to accumulate anything across a
session.

---

## R-TVM.4 — Resource bounds (always on, orthogonal to capabilities)

| Bound | Mechanism | Default |
|---|---|---|
| Wall clock | context deadline + `WithCloseOnContextDone(true)` | 5s (`[tools] timeout`) |
| Memory | `WithMemoryLimitPages` | 16 MiB (`[tools] memory_mb`) |
| Output | the dispatcher's existing cap + spill (R-DISP.2) | 2048 tokens |
| CPU | **none — see below** | — |

wazero has **no fuel/gas metering**. The wall-clock deadline is the only CPU bound, and it
is enforced by closing the module out from under the guest. This is adequate — a spinning
tool dies at the deadline and the model observes a normal failure — but an operator
running many concurrent sessions is trusting the deadline, **not** a work budget, and that
is a stated limitation rather than an assumption.

A call that exceeds the deadline **MUST** report a timeout naming the tool, not a generic
instantiation or trap failure.

---

## R-TVM.5 — The capability set

The default is the **empty set**. Every capability is exactly one of two things: a wazero
pre-open, or a host function the daemon exports. Anything else is not "denied" — it is
**structurally absent**, with no function to call and therefore nothing to bypass.

| Capability | Grant parameters | Default | Enforced by |
|---|---|---|---|
| `fs.read` | host→guest path mounts | **none** | wazero `WithReadOnlyDirMount` |
| `fs.write` | host→guest path mounts | **none** | wazero `WithDirMount` |
| `net.http` | host allowlist, methods, max bytes | **none** | host fn `nine.http` — R-TVM.12 |
| `env` | explicit key allowlist | **none** | `WithEnv`, per key |
| `clock` | — | **granted** | `WithSysWalltime` / `WithSysNanotime` |
| `random` | — | **granted** | `WithRandSource` |
| `log` | — | **granted** | host fn `nine.log` → `slog` |

`clock`, `random`, and `log` are unconditional because they leak nothing and every
non-trivial tool needs them. Everything with reach starts at nothing.

The **only** host module exported to a guest is `nine`, and its entire contents are `log`.

A sandboxed tool **MUST NOT** be able to spawn a process, open a socket, load a native
library, or call another tool. None of those verbs exist inside a wasm module and none are
exported to it.

---

## R-TVM.6 — Capabilities are conferred, never claimed

A manifest **declares a need**. Only `nine.toml` **grants**. These are different documents
written by different people.

```text
manifest [capabilities]        — what the tool needs   (developer, in the repo)
nine.toml [tool.<name>]        — what the tool gets    (operator, on the host)
effective = the grant, and the grant only
```

The declaration contributes **no parameters** to the effective set — only the requirement
that the two agree. The manifest is therefore documentation and a pre-flight check, never
a security control: nothing reads it at call time, so **a manifest that lies gains
nothing**.

The declared and granted capability sets **MUST** be identical, and a mismatch in either
direction is a **named load failure**, surfaced in `nine tools` rather than logged and
forgotten:

- **Declared but not granted** — the tool refuses to load rather than starting up
  crippled. Silent degradation means a tool that half-works in ways neither the developer
  nor the operator predicted.
- **Granted but not declared** — refused on the same reasoning, read the other way.
  Conferring reach on a tool that never asked for it is how an over-broad grant survives
  review. Forcing the two documents to agree keeps the manifest an accurate description of
  what the tool can do.

For `env` the *keys themselves* must match, not merely the presence of the capability: a
grant of the wrong keys would otherwise pass review as if it were the right ones.

> **The invariant this preserves.** R-PLUG.7's "**Nine cannot grant itself capabilities**"
> is unchanged and is now load-bearing for two subsystems. The operator writes every
> grant; no agent-reachable path writes one.

---

## R-TVM.7 — Grants in `nine.toml`

Following the plural-subsystem / singular-instance split (R-PLUG.10):

```toml
[tools]
enabled   = true
user_dir  = "/etc/nine/tools.d"
timeout   = "5s"
memory_mb = 16

[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]

[tool.tz_aware.capabilities]
env = ["TZ"]
```

- **Grants are per named tool.** There is **no** wildcard `[tool."*"]`. An operator
  granting filesystem access does so to a tool they have read.
- **`fs` mount host paths MUST be absolute.** A relative one would resolve against the
  daemon's working directory, which is not what an operator writing a grant is thinking
  about.
- **`env` keys matching `NINE_*` or `*_API_KEY` MUST be refused** as a config error. The
  daemon's environment holds LLM provider credentials; a tool granted one wholesale would
  be a credential exfiltration primitive. A refusal, not a filter — an operator who meant
  it finds out at load.
- **Read at load.** A grant change reaches a running tool only on `nine tools reload` or
  restart, matching R-PLUG.10.

---

## R-TVM.8 — Imports are a capability

> **Module resolution happens in the host, against a closed allowlist, before
> instantiation. The guest never receives a resolver that can touch disk or network.**

Without this, `import` is a capability-model bypass hiding in plain sight: a tool granted
nothing could import another tool's bundle and execute code the operator approved for a
different purpose, and an fs-reading resolver is an ungranted `fs.read` by another name.

The host supplies the interpreter's module loader and serves only from a map it built
before the call. There **MUST** be no relative import, no absolute path, no URL, and no
dynamic `import()` of anything absent from that map; all of them fail identically.

**For a developer tool the allowlist is empty.** Dependencies are pre-bundled at
development time (`esbuild --bundle --format=esm` or equivalent), so by the time Nine
loads the file it has no imports left. Nine has no package manager, no lockfile, and no
network at load time.

The curated `nine:*` standard library and external-dependency resolution
(`docs/sandboxed-tools.md` §4.2, §4.4) belong to the generated tier and **are not built**.

---

## R-TVM.9 — The interpreter surface is trimmed

QuickJS-NG ships `std` and `os` as **separate, opt-in init calls**, and the stock `qjs`
CLI links both. Between them they expose a filesystem API, a process API (`os.exec`), a
network fetch (`std.urlGet`), and two arbitrary-eval hooks (`std.evalScript`,
`std.loadScript`) — in scope before any capability has been granted.

**The committed blob MUST link neither.** This is a hard requirement, not a hardening
nicety, and it is asserted by test: a capability table that says `fs.read` while the guest
also holds `std.loadFile` and `os.stat` is a table that lies.

wazero's denials are the backstop, not the control. Under WASI `os.exec` has no
`proc_spawn` and `std.urlGet` has no socket — but `os.readdir` and `os.open` map onto
`fd_readdir`/`path_open`, which work fine against **any pre-open granted**, so a tool
granted `fs.read` on one directory would silently gain a second, undeclared file API over
it.

The interpreter is built from a pinned tag by `internal/toolvm/quickjs/build.sh`,
committed with a recorded SHA-256, and rebuilt only on a deliberate bump
(`make quickjs-wasm`). CI re-checks the hash (`make quickjs-verify`). An ordinary
`make build` needs no wasi-sdk, no clang, and no clone, and **the runtime image gains no
toolchain**.

---

## R-TVM.10 — Loading (the R-PLUG.9 sequence)

Developer tools are discovered from `[tools].user_dir` in the sidecar layout user plugins
already use — an operator should not have to learn a second set of rules.

```text
$NINE_TOOLS_USER_DIR/
  csvstats.toml          # manifest — the gate
  csvstats.js
  imageresize.toml
  imageresize.wasm
```

1. Candidates are taken in **deterministic name order**.
2. A malformed manifest, a missing entrypoint, or an invalid input schema is recorded as
   **skipped** — nothing of that tool is compiled or run.
3. Capabilities are resolved (R-TVM.6); a mismatch skips the tool with a named error.
4. The name is checked against **core-intercepted tools, native plugin tools, and
   earlier-accepted sandboxed tools**. Any collision **skips the whole tool — no override,
   ever**.
5. A `wasm` module missing the ABI (R-TVM.1) is rejected here, at load, not at first call.

Any single failure is logged at ERROR and kept in the load status, but **MUST NOT** abort
the others: one bad drop-in cannot take the daemon down.

The manifest is the **gate** — a `.js` or `.wasm` file with no manifest beside it is never
loaded — and, unlike a native plugin manifest, it is **authoritative** for the tool's
shape. There is no process to ask `plugin.describe`, so `name`, `description`, and
`input_schema` come from the manifest.

Required fields: `name` (matching `[a-z0-9_]+`), `kind`, `entrypoint`, `description`. An
unknown key is an error: in a file whose job is declaring capabilities, a typo'd key
silently meaning nothing is the worst available failure mode.

---

## R-TVM.11 — Visibility, reload, and reporting

A newly-loaded tool is picked up by **subsequently-built agent loops**; turns already in
flight keep the tool set they started with. This is exactly `plugins reload` semantics
(R-PLUG.9) and needs no new push machinery — a tool set that mutated mid-turn would make
the turn unreplayable, and `docs/event-log.md` depends on replay.

Sandboxed tools are advertised on the same footing as plugin tools and intersected with a
role's allowlist the same way (boundary 1 of R-ROLE.4).

| Surface | Behavior |
|---|---|
| `nine tools` | the roster: loaded tools with their **resolved** capabilities, plus every skipped candidate **with its reason** |
| `nine tools show <name>` | one tool's kind, status, resolved grant, manifest path, description |
| `nine tools reload` | re-scan and reload; operator action, never an agent tool |
| `nine tool validate [path]` | the manifest checks, locally, with no daemon |
| wire | `tools_list` / `tools_reload` → `protocol.SandboxedToolStatus` |
| `list_tools` | sandboxed tools appear under the pseudo-plugin `sandboxed` |
| `plugin_call` | reaches sandboxed tools, resolved against the **live** host |

`plugin_call` **MUST** resolve a sandboxed tool against the host itself, not through
a dispatcher snapshot: its reach must match `list_tools` (R-PROTO.5), and a dispatcher
built once at daemon assembly would keep answering from the tool set that existed at boot,
so a tool added by `nine tools reload` would be listed but uncallable. Agent loops have no
such requirement — they are rebuilt per turn, which *is* the next-turn visibility above.

The skipped entries are why the reporting surface is required: R-TVM.6 makes a capability
mismatch a load failure rather than a degraded tool, and that promise is only kept if the
operator can read the failure.

`nine tool validate` deliberately does **not** resolve capabilities. A grant lives in
`nine.toml` on the host, so validating against the local config would report a confident
answer that does not transfer. It checks what the developer owns: manifest shape,
entrypoint presence, schema validity, and ABI exports.

---

## R-TVM.12 — `net.http`

The filesystem is easy: a wazero pre-open is a capability primitive wazero enforces
without our help. The network has none — wazero has no network at all — so `net.http` is
entirely a host function, and its security is entirely Nine's problem. Getting it wrong
turns every sandboxed tool into an SSRF primitive with a manifest.

**The guest never touches a socket and never learns an IP.** It calls `nine.http` with a
JSON request; the daemon makes the request. In a `js` tool this is surfaced as a `fetch`
subset (no streaming, no AbortController, no cookie jar, no Request/Headers classes).

The host module exports `http` unconditionally, because a wasm module's imports are fixed
at compile time and the QuickJS blob is shared by every `js` tool. That is not a leak: the
**grant** is resolved per call, and a tool without one is refused before the request is
parsed. What is shared is the import, not the permission.

### Two independent gates

Both **MUST** pass, and neither is redundant:

1. **The hostname** must match the tool's `allow_hosts` (exact, or a leading `*.` pattern
   that does not match the apex).
2. **The IP actually being dialed** must be publicly routable — checked in
   `net.Dialer.Control`, which runs *after* resolution and *immediately before* connect.

> Gate 1 alone is defeated by a hostname that resolves wherever an attacker likes. Gate 2
> alone would permit any public host.
>
> Gate 2 **MUST** be enforced at connect time, not after a separate resolution step.
> Resolving, validating, then dialing leaves a window in which the name is re-resolved to
> something else — that window *is* the DNS-rebinding attack.

### The unconditional rejections

Not configurable, and not subject to the allowlist. An operator cannot be asked to
remember that `169.254.169.254` is where their cloud keeps its credentials.

Loopback · link-local (**including cloud instance metadata**) · RFC 1918 · IPv6 ULA ·
multicast · unspecified · carrier-grade NAT · benchmark, documentation and reserved
ranges · NAT64 · interface-scoped addresses.

IPv4-mapped IPv6 (`::ffff:127.0.0.1`) **MUST** be unmapped before checking — it is the
standard way past a filter that only knows `127.0.0.0/8`.

### The rest of the checklist

| # | Requirement |
|---|---|
| 1 | Method allowlist enforced; `methods` is required, with no implicit default |
| 2 | `http`/`https` only; a URL carrying credentials (`user:pass@host`) is refused |
| 3 | The guest may not set `Host`, `Content-Length`, or any hop-by-hop header |
| 4 | No proxy is ever taken from the environment — it would resolve and connect on our behalf, routing around gate 2 |
| 5 | Every redirect hop is re-checked against **both** gates; chains are bounded |
| 6 | `Authorization`, `Cookie`, and `Proxy-Authorization` are stripped on a cross-origin redirect — including a scheme or port change within one domain, which Go's own stripping does not cover |
| 7 | The response body is bounded **on read**, not merely truncated after; the timeout sits below the per-call deadline so a slow host reads as an HTTP timeout, not a killed tool |
| 8 | Every call is recorded: tool, method, host, status, bytes, truncation, duration |
| 9 | `Set-Cookie` is dropped from the response — a tool has no cookie jar, so passing them on could only leak them into the model's context |

A refusal reaches the tool as a thrown error carrying the reason, never as a status code:
letting a policy decision look like a response invites `if (res.ok)` to swallow it.

> **On item 8, precisely.** Every call is written to the daemon log with structured
> fields, unconditionally. It does **not** yet reach the `session_events` journal
> (`docs/event-log.md`), because attribution needs a session id that the tool host does
> not have — the dispatcher does not carry one into a tool call. `toolvm.Config.AuditHTTP`
> is the hook for that, and it is currently unwired. "What did this tool reach" is
> answerable today from the log; "which turn asked for it" is not, and that gap should be
> closed when session context reaches the dispatcher.

**There is no bare `"*"`.** An operator who wants unrestricted egress should write a
native plugin, where that intent is explicit and reviewed. Config validation refuses it.

---

## R-TVM.14 — Generated tools (the tier Nine authors)

A **generated tool** is one Nine wrote itself, through the core-intercepted `tool_write`
(R-DISP.3). It is a row in the store's `tools` table (`spec/contracts/memory-store.md`),
holding the tool's `js` source, its `input_schema`, and its capability **declaration** — and
deliberately **no grant**. It runs through the exact host, ABI (R-TVM.1), instance model
(R-TVM.3), bounds (R-TVM.4), and audit (R-TVM.12) a developer tool does. Kind is always
`js`: the agent cannot supply a `.wasm` blob, because a binary is not reviewable.

The tier is **off unless `[tools.agent] enabled`**. With it off, `tool_write`, `tool_delete`,
and `js_eval` are neither registered nor advertised, and a loop is identical to one built
before the tier existed (I-TVM.6 extends to it).

### The ceiling, not a grant

The operator confers a single **ceiling** — `[tools.agent.capabilities]` — that is the
**maximum** any generated tool may be granted, never an automatic grant:

- A tool receives a capability only if it **declares** it; a tool that declares nothing runs
  with nothing, whatever the ceiling permits. Least privilege is per tool, not per tier.
- Declaring a capability the ceiling excludes is a **refusal**, returned to the model as a
  message it can act on — it rewrites without the capability or calls `gap_report`. A tool
  cannot request its way past the ceiling.
- The declaration is **re-resolved against the current ceiling on every load**, so narrowing
  the ceiling disables a tool that no longer fits rather than leaving it running with reach
  the operator has withdrawn.
- For `env`, the tool receives the **intersection** of its declared keys and the ceiling's,
  and a declared key the ceiling omits is a refusal — declaring one key never confers the
  others the operator happened to list.

> **The invariant this preserves.** R-PLUG.7's "**Nine cannot grant itself capabilities**"
> is unchanged. `tool_write` writes *code*; it has no column and no path to write a *grant*.
> The agent writes the code, the operator writes the ceiling, and they are never the same
> actor — the one asymmetry the whole tier exists to enforce.

### Lifecycle

- **Write.** `tool_write` validates the proposal against the ceiling and the namespace
  (R-TVM.10's collision rules — a generated tool never overrides a built-in, plugin, or
  developer tool) **before** persisting, so a refusal leaves no row behind. It then upserts
  by name — writing an existing generated name **replaces** it, preserving the usage counters
  — and evicts.
- **Cap and eviction.** The catalog is capped at `[tools.agent].max_tools` (default 64) with
  **LRU eviction on last-called-at**: every generated tool competes for the tool-selection
  budget, so an unbounded catalog degrades ranking for the built-in tools too. A write that
  crosses the cap evicts the least-recently-called tools and names them in its result.
- **Visibility is next-turn** (R-TVM.11): a tool written this turn is callable from the next
  loop built, exactly as `plugins reload` behaves. `tool_write`'s result says so explicitly.
- **`js_eval`** (`[tools.agent] eval`) runs one snippet under the identical rules and persists
  **nothing** — no name, no row, no catalog entry. It is not a softer tier, only a less
  persistent one; it exists so iteration does not accrete single-use tools into the catalog.
- **Delete.** `tool_delete` removes a generated tool by name; it cannot touch a built-in,
  plugin, or developer tool.

### The approval gate

`[tools.agent].require_approval` selects when `tool_write` and `js_eval` route through the
HITL gate (`spec/contracts/hitl.md`, R-HITL.5), reusing it wholesale:

| Value | Gates when |
|---|---|
| `on_capability` *(default)* | the call **declares any capability** — a capability-free write is inert and passes without a prompt |
| `always` | every write and every eval |
| `never` | never — the ceiling is the only control |

The gate applies **only to a loop an interactive session owns**, like every approval gate
(R-HITL.5). A non-interactive deployment has no gate, so there the ceiling is the sole
control. The default gates on **substance, not frequency**: prompting on a pure-computation
tool trains the reflex that defeats the prompt that matters.

### Reporting

A generated tool appears in `nine tools` (R-TVM.11) with **no manifest path** and a
provenance marker; `SandboxedToolStatus.Generated` carries this on the wire
(`spec/contracts/wire-protocol.md`).

`tool_write`, `tool_delete`, and `js_eval` are ordinary dispatched tools, so each call —
its source, declared capabilities, and result — is already recorded in the `session_events`
journal (`spec/contracts/event-journal.md`), attributed to the session and turn. The daemon
**additionally** logs each write/delete with the tool name and its declared reach, as a
greppable operator breadcrumb that survives a journal scrub. The audit gap R-TVM.12 item 8
describes is **specific to `net.http`**: that call happens inside the wasm host, below the
dispatcher, with no session id — it does **not** apply to the meta-tools, which run in the
loop's own context.

---

## R-TVM.13 — Not built

The following are specified in `docs/sandboxed-tools.md` and **are not implemented**. Each is
refused by name rather than silently ignored, because accepting it would advertise a boundary
that does not exist.

| Feature | Design | Status |
|---|---|---|
| `nine:*` stdlib | §4.2 | Not present; the tool-side import allowlist is empty (R-TVM.8), for generated tools as for developer ones. |
| External npm dependencies | §4.4 | No resolver, no bundler, no registry client. `deps` in a declaration is unknown. |
| The `deps` + `net.http` interlock | §4.4 | Nothing to interlock: there are no dependencies to resolve. It becomes load-bearing the moment stage 6 is built. |

---

## Invariants

- **I-TVM.1** — The default capability set is empty. A tool that declares nothing gets
  nothing, regardless of what any other tool was granted.
- **I-TVM.2** — No agent-reachable tool or path writes a **grant**. `tool_write` writes a
  generated tool's *code and declaration* (R-TVM.14); the operator writes every grant and the
  generated ceiling, in `nine.toml`. Nine cannot grant itself capabilities (R-PLUG.7,
  unchanged) — the manifest of a developer tool and the declaration of a generated one are
  descriptions, not grants.
- **I-TVM.3** — No state survives a call.
- **I-TVM.4** — A tool name resolves to exactly one implementation. Sandboxed tools never
  override built-ins or plugin tools.
- **I-TVM.5** — The committed interpreter links neither `std` nor `os`.
- **I-TVM.6** — `[tools] enabled` unset ⇒ no host, no tools, and loops identical to those
  built before this subsystem existed.
- **I-TVM.7** — A sandboxed tool cannot reach a loopback, link-local, or private address,
  whatever its `allow_hosts` says and whatever any hostname resolves to.
