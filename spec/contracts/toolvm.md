# Contract — sandboxed tools (the wasm tool host)

**Status:** Built (stages 1–6) · **Depends on:** dispatcher (registration), config (grants + the generated ceiling + the deps policy), memory store (generated tools are rows), HITL (the optional write gate), esbuild (write-time dependency bundling) · **Used by:** any turn calling a sandboxed tool

A **sandboxed tool** is a wasm module that the daemon executes **in-process**, with an
explicitly conferred set of capabilities and nothing else. It is a *second backend behind
the same dispatcher* as native plugins (`spec/contracts/plugin.md`), not a replacement:
the plugin contract, its transport, and `plugin.ProtocolVersion` are untouched, and a
deployment that sets `[tools] enabled = false` behaves exactly as it did before this
subsystem existed. `enabled` defaults to **true**, because this tier carries the workspace
file tools (R-TVM.16) — without it an agent cannot read or write a file.

The design rationale is `docs/sandboxed-tools.md`; the authoring guide is
`docs/writing-sandboxed-tools.md`. This file is normative.

> **Two authors, one runtime.** A **developer tool** is a file on disk with a manifest,
> installed by the operator (R-TVM.10). A **generated tool** — authored by Nine itself via
> `tool_write` — is a row in the store (R-TVM.14). Both run through the *same* host, ABI,
> capability model, and audit trail; the only differences are where the code comes from and,
> for a generated tool, that the operator confers a **ceiling** rather than a per-tool grant.
> Requirements R-TVM.1–R-TVM.12 hold for both kinds unless they say otherwise; R-TVM.14 adds
> what is specific to the generated tier, and R-TVM.15 the modules it may import.

---

## R-TVM.1 — the guest ABI

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

A **success MAY** be bytes instead of text:

```json
{"ok": true, "output_b64": "<base64>", "media_type": "image/png"}
```

`output_b64` is set instead of `output`, never alongside it. The dispatcher writes the
decoded bytes to the file store — base64-encoded, since that store is a TEXT column that
strips NULs — and replaces the result with a path plus a description, because a language
model cannot read bytes and inlining them would consume the output budget to no purpose.
This requires no `fs.write` grant: the file store is Nine's, not the operator's filesystem.

`media_type` is advisory and **MUST** be treated as untrusted: it is interpolated into text
the model reads in Nine's own voice, so anything outside an RFC 6838 token is dropped.

With no spill sink registered, a byte result is an **error** rather than a degraded
success — unlike over-cap text, there is no smaller-but-valid form of a truncated blob.

A success **MAY** instead say the tool is **not finished**:

```json
{"ok": true, "continue": {"cursor": "<opaque>", "progress": "41% · 1.2 GB/2.9 GB",
                          "after_ms": 2000}}
```

The host persists `cursor`, waits, and calls the tool again with it handed back —
R-TVM.19. `output` and `output_b64` are absent: a continuation is the tool declining to
produce a result yet, not a smaller one.

A tool whose manifest does not declare `resumable = true` returning this **MUST** be an
error. Shape is the manifest's to state (R-TVM.10), and a tool must not be able to acquire
a lifecycle by returning a field.

A failure **MAY** carry structure alongside the message:

```json
{"ok": false, "error": "weather API timed out",
 "error_detail": {"name": "TypeError", "code": "E_UPSTREAM",
                  "retryable": true, "cause": ["connect ETIMEDOUT"]}}
```

| Field | Meaning |
|---|---|
| `name` | The error's class. Diagnostic; `"Error"` is not surfaced, being the default. |
| `code` | The tool's own stable identifier, e.g. `"E_RANGE"`. Stable across rewordings. |
| `retryable` | Whether trying again could plausibly work. **Absent is not `false`** — one is the tool declining to say, the other is it saying no. |
| `cause` | The chain behind the failure, outermost first, flattened to messages. |

Every field is optional, and `error` remains the message, so a guest that sets none of them
produces exactly the envelope it produced before `error_detail` existed. This is why the
addition **does not** bump `ABIVersion`: the two-export contract is unchanged and no
existing parser breaks (adr/rich-js-tools.md §8). The same reasoning covers `continue`.

The host renders these into the error the model reads, since the dispatcher's channel for a
tool failure is one string. A `js` tool's harness fills them from the thrown `Error`
(`name`, the conventional `code`, and the ES2022 `cause` chain); a `wasm` tool writes the
fields itself, or calls `nine_fail_code` from `nine.h`.

`ABIVersion` is independent of `plugin.ProtocolVersion` (`docs/versioning.md`). A module
declaring an unsupported ABI **MUST** be refused at load, not called.

---

## R-TVM.2 — two kinds, one contract

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

## R-TVM.3 — one instance per call

A module is **compiled once** and **instantiated per call**; the instance is closed when
the call returns.

This is the strongest property in the design and is required, not an optimization:
**no state survives a call implicitly.** Not a global, not a cached credential, not a
poisoned prototype, not a half-freed heap. Two calls to the same tool **MUST NOT** be able
to observe each other *through the machine*, and a tool **MUST NOT** be able to accumulate
anything across a session except through a capability it was granted.

> **Amended by R-TVM.18.** The original read "no state survives a call", full stop. A tool
> granted `state` can now carry values between calls — but only through a host-owned store,
> named by the tool, bounded by quota, and conferred by the operator. What the instance
> model still guarantees is unchanged and is what the sandbox rests on: the guest's globals,
> heap, and interpreter realm are destroyed at return, so no *implicit* carryover is
> possible. What was given up is deliberate and audited in
> `adr/durable-and-long-running-tools.md` §2.

---

## R-TVM.4 — resource bounds (always on, orthogonal to capabilities)

| Bound | Mechanism | Default |
|---|---|---|
| Wall clock | context deadline + `WithCloseOnContextDone(true)` | 5s (`[tools] timeout`, overridable per tool) |
| Memory | `WithMemoryLimitPages`, per call | 16 MiB (`[tools] memory_mb`) |
| Concurrency | a host semaphore held across instantiation | 8 calls (`[tools] max_concurrent`) |
| Output | the dispatcher's existing cap + spill (R-DISP.2) | 2048 tokens |
| Work (`js` only) | QuickJS interrupt handler | 50M operations (`[tools] max_ops`, overridable per tool) |

The host **MUST** bound simultaneous calls. Memory is capped per call, so without
a concurrency bound the host's worst case is whatever the turns in flight ask
for; the two multiply, and `[tools] max_concurrent` is the second factor. A call
that finds every slot taken **MUST** wait rather than fail, and the wait **MUST**
be bounded by the caller's context rather than by the tool's own deadline — a
call that queued for four seconds and then ran under a one-second remainder would
report a timeout that describes the queue and not the tool. A caller whose
context ends while queued **MUST** be told it was queueing.

The wall clock **MAY** be overridden for a named tool with `[tool.<name>] timeout`, which
takes precedence over `[tools] timeout` for that tool alone. It is a resource bound, not a
capability, so it sits outside `[capabilities]` and confers nothing.

The override exists in both directions and both matter. Raising it lets one tool that
legitimately takes twenty seconds have them without handing twenty seconds to a tool with
an infinite loop; lowering it pins a risky tool below the global bound. Since this deadline
is the only CPU bound there is, a single global value forces the most permissive tool's
requirement onto every other tool.

**A `net.http` request is bounded at four fifths of the time the call has left**, so a slow
host surfaces as an HTTP timeout the tool can catch and report rather than as the whole
call being killed under it. That ratio generalizes the fixed 4s-of-5s default rather than
replacing it, and it is computed from the context deadline, so a tool that has already
spent most of its budget does not get a request bound longer than its remaining life.

A `js` call **MUST** be bounded by work as well as by time. The host converts the
operator's budget into interrupt checks and the guest installs a QuickJS interrupt handler
before anything the envelope named is evaluated, the harness included, so no code runs
unmetered. Exhaustion **MUST** be sticky and **MUST** surface as an ordinary tool error
carrying `error_detail.code = "E_WORK_BUDGET"`; the host renders the sentence, because the
guest counts checks and knows neither the configured number nor the key that raises it.

The bound **MUST NOT** be catchable. QuickJS marks the interrupt uncatchable, so the
unwinder skips every `catch` and an `async` function propagates rather than rejecting — a
tool cannot wrap its loop in `try`/`catch` and continue. A budget a tool can decline is not
a bound.

The unit is approximate: QuickJS polls its handler once per 10,000 backward jumps and
calls, so an "operation" is a loop iteration or a call rather than a bytecode op, and the
bound is granular to ±10,000. This is stated rather than hidden because the number an
operator sets is not the number the guest counts.

A **`wasm` tool is not metered.** wazero has no fuel/gas metering and a raw module has no
interpreter to interrupt, so for those tools the wall-clock deadline remains the only bound
and that is a stated limitation rather than an assumption.

**A failing call MUST carry the tail of what it printed.** A tool reaches `nine.log`
through an import granted to every tool, and until a call fails those lines are the
daemon's alone — the model that wrote the tool cannot read them. A failure **MUST** append
the most recent lines, bounded in count and bytes, to the error the caller receives; a
**success MUST NOT**, because logs nobody asked for are noise. This confers nothing: the
lines were already printed through a granted import, and this only stops discarding them
when they turn out to explain something.

The bound is on the buffer, not on the tool: a tool printing in a loop keeps the lines
nearest its failure, since those are the ones that explain it. Because the buffer reaches
the model and the turn, a tool that prints a credential has published it — the authoring
guide says so, and that is the stated cost of the feature rather than a defect in it.

A call that exceeds the deadline **MUST** report a timeout naming the tool, not a generic
instantiation or trap failure.

---

## R-TVM.5 — the capability set

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
| `log` | — | **granted** | host fn `nine.log` → `slog`, and the tail of it onto a failure (R-TVM.4) |
| `state` | scope (**required**), quotas, ttl | **none** | host fn `nine.state` → `tool_state` — R-TVM.18 |

`clock`, `random`, and `log` are unconditional because they leak nothing and every
non-trivial tool needs them. Everything with reach starts at nothing.

The **only** host module exported to a guest is `nine`, and its entire contents are `log`.

A sandboxed tool **MUST NOT** be able to spawn a process, open a socket, load a native
library, or call another tool. None of those verbs exist inside a wasm module and none are
exported to it.

---

## R-TVM.6 — capabilities are conferred, never claimed

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
> is unchanged and now covers two subsystems. The operator writes every grant; no
> agent-reachable path writes one.

---

## R-TVM.7 — grants in `nine.toml`

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

[tool.geocode.capabilities.state]
scope        = "tool"      # required; "tool" or "conversation"
max_keys     = 512
max_value_kb = 8
ttl          = "24h"       # optional; omit for no expiry
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
- **Read at boot.** A conforming implementation resolves grants when it builds the host
  and **MUST NOT** re-read `nine.toml` while the daemon runs. `nine tools reload` re-scans
  `user_dir` against the grants already held — it picks up a new or edited tool, and a
  changed grant needs a restart.

---

## R-TVM.8 — imports are a capability

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

**For a generated tool the allowlist is the `nine:*` stdlib** (R-TVM.15), served
host-side from the pinned, vendored module map. External npm imports are **not** served at
call time either: they are resolved and bundled **into the tool's source at write time**
(R-TVM.15), so by the time the tool runs it has no imports left but `nine:*`. The rule
above is unchanged and is *implemented* by that write-time bundler's resolver, which serves
only from a verified cache — never weakened by it.

---

## R-TVM.9 — the interpreter surface is trimmed

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

**This is about `std`/`os`, not about having a filesystem API at all.** The blob does
expose a narrow one — `nine:fs` and `nine:env`, built on ordinary libc calls in
`qjs_host.c` — and that is compatible with the requirement above for the reason it exists:
those calls route through WASI to exactly the pre-opens and env pairs the host configured,
so a tool with no grant sees an empty filesystem and an empty environment. What R-TVM.9
forbids is an interpreter-supplied API that arrives *alongside* the capability table
without appearing in it, together with `exec` and `evalScript`. What `nine:fs` provides is
the capability table's own `fs.read`/`fs.write`, reachable at last from the kind of tool
most people write.

**The confinement is wazero's, and MUST stay wazero's.** `nine:fs` performs no path
validation, and no host function is permitted to grow one: a pre-open is a real capability
primitive, and re-implementing containment as a check of Nine's own would trade an
enforced boundary for a reviewed one.

`nine:fs` **MUST** offer positional and structural operations, not whole-file access alone:
a ranged read, an append, a rename, and a non-recursive remove. A call's memory cap
(R-TVM.5) is smaller than the files a workspace holds, so whole-file reads put the larger
half of any workspace out of reach; and without a rename there is no atomic replacement,
which is what keeps an interrupted write from destroying the file it was editing. Each is
an ordinary libc call inside the pre-open, so none of them widens what a grant reaches.

The interpreter is built from a pinned tag by `internal/toolvm/quickjs/build.sh`,
committed with a recorded SHA-256, and rebuilt only on a deliberate bump
(`make quickjs-wasm`). CI re-checks the hash (`make quickjs-verify`). An ordinary
`make build` needs no wasi-sdk, no clang, and no clone, and **the runtime image gains no
toolchain**.

---

## R-TVM.10 — loading (the R-PLUG.9 sequence)

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

## R-TVM.11 — visibility, reload, and reporting

A newly-loaded or removed tool reaches every agent loop at that loop's **next turn**,
including a loop that already exists, such as the session that wrote the tool. Each turn
starts by comparing the host's catalog with the one the loop last saw; on a change it
re-syncs the loop's dispatch handlers and advertised tool list. Turns already in flight
keep the tool set they started with, because a tool set that mutated mid-turn would make
the turn unreplayable, and `adr/event-log.md` depends on replay. No push machinery is
involved: the loop pulls at its turn boundary. Plugin tools differ: a plugin started by
`plugins reload` reaches only subsequently-built loops (R-PLUG.9).

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
so a tool added by `nine tools reload` would be listed but uncallable. Agent loops meet the
same need by re-syncing from the host at the start of each turn, which *is* the next-turn
visibility above.

The skipped entries are why the reporting surface is required: R-TVM.6 makes a capability
mismatch a load failure rather than a degraded tool, and that promise is only kept if the
operator can read the failure.

`nine tool validate` deliberately does **not** resolve capabilities. A grant lives in
`nine.toml` on the host, so validating against the local config would report a confident
answer that does not transfer. It checks what the developer owns: manifest shape,
entrypoint presence, schema validity, and ABI exports.

---

## R-TVM.12 — `net.http`

> **Amendment.** `allow_hosts` **MAY** be a bare `*`, meaning any host. It previously
> could not, on the reasoning that an operator wanting unrestricted egress should write a
> native plugin instead — but that escape hatch pointed at *less* safety: a plugin is a
> subprocess with the daemon's uid and none of the checks below. Nine's own fetching tools
> are the case in point, since they exist to retrieve whatever URL a model chose, which no
> host list expresses.
>
> The wildcard grants any **host**. It does not grant any **address**. Every connection is
> checked at dial time, so loopback, link-local (cloud instance metadata), private ranges
> and multicast remain refused whatever the allowlist says — and being at dial time rather
> than on the URL, that check also survives redirects and DNS rebinding. An implementation
> **MUST NOT** let the allowlist short-circuit the address checks.
>
> A tool whose hosts *are* knowable **MUST** name them. `web_search` talks to three search
> endpoints and is granted exactly those; the wildcard is for the tools that genuinely
> cannot be constrained, not a default.


The filesystem is easy: a wazero pre-open is a capability primitive wazero enforces
without our help. The network has none — wazero has no network at all — so `net.http` is
entirely a host function, and its security is entirely Nine's problem. Getting it wrong
turns every sandboxed tool into an SSRF primitive with a manifest.

**The guest never touches a socket and never learns an IP.** It calls `nine.http` with a
JSON request; the daemon makes the request. In a `js` tool this is surfaced as a `fetch`
subset (no streaming, no AbortController, no cookie jar, no Request/Headers classes).

**Bodies are text or bytes, never text pretending to be bytes.** The transport is UTF-8
JSON, so a body that is not valid UTF-8 cannot travel in a JSON string:

| Direction | Field | When |
|---|---|---|
| response | `body` | the body is valid UTF-8 |
| response | `body_b64` | it is not — set **instead of** `body`, never both |
| request | `body` | sending text |
| request | `body_b64` | sending bytes; wins over `body` if both are set |

The host decides the response direction, because the host is the last place the original
bytes exist. Encoding them into `body` would replace every invalid byte with U+FFFD before
any guest could observe it, leaving `ok` true and the corruption silent. A `js` tool reads
this through `bytes()`/`arrayBuffer()`, and `text()`/`json()` **MUST** throw on a binary
body rather than return the replacement-character rendering.

Both fields are additive, and a guest that ignores `body_b64` behaves as it did before it
existed — hence no `ABIVersion` bump (adr/rich-js-tools.md §8).

The host module also exports `caps`, which returns the calling tool's resolved grant as
JSON — guest paths only, never the operator's host paths. It exists so a guest can say
`fs.read is not granted to this tool` rather than surface an `ENOENT` for a file that
plainly exists, and it **MUST NOT** be read back by anything making an enforcement decision
(I-TVM.8).

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

> **On item 8, precisely.** Every call is audited, **whatever its outcome** — a request
> refused at the host allowlist, at the SSRF gate, or on a redirect hop is recorded with
> its reason, exactly like one that returned 200. An audit that recorded only successes
> would answer "what did this tool fetch" while leaving "did this tool try to reach the
> instance-metadata endpoint" unanswerable, which is the question an operator has.
>
> Each call goes to the daemon log with structured fields, unconditionally, and to the
> `session_events` journal as a `tool_http` event attributed to the session, the turn, and
> the span of the tool call that made it. The hook travels on the **context**, installed by
> whoever is running the turn (`toolvm.WithHTTPAudit`), because a Host is daemon-wide and
> built once at boot while an audit record belongs to a session — a hook configured at Open
> could not know one.

**A bare `"*"` is permitted** and grants any host, never any address: the dial-time
check above is not subject to the allowlist (see the amendment at the head of this
requirement). A tool whose hosts are knowable **MUST** name them.

---

## R-TVM.14 — generated tools (the tier Nine authors)

A **generated tool** is one Nine wrote itself, through the core-intercepted `tool_write`
(R-DISP.3). It is a row in the store's `tools` table (`spec/contracts/memory-store.md`),
holding the tool's `js` source, its `input_schema`, and its capability **declaration** — and
deliberately **no grant**. It runs through the exact host, ABI (R-TVM.1), instance model
(R-TVM.3), bounds (R-TVM.4), and audit (R-TVM.12) a developer tool does. Kind is always
`js`: the agent cannot supply a `.wasm` blob, because a binary is not reviewable.

The tier is **on unless `[tools.agent] enabled = false`**, and is gated by `[tools] enabled`
above it: the generated tier runs on the host, so with the host off the tier is off whatever
it says. With the tier off, `tool_write`, `tool_delete`, and `js_eval` are neither registered
nor advertised, and a loop is identical to one built before the tier existed (I-TVM.6 extends
to it).

The default is on because the ceiling, not this switch, is the control that bounds the tier:
a tool is granted only what it declares and only what the operator has conferred, so a tier
that is on with a narrow ceiling is not a tier that is unbounded. The **default ceiling is the
workspace, read and write**, derived from `[workspace].root` at the `ShippedWorkspaceGuest`
path the shipped tools use — the same directory `shell` already runs in. An explicit
`[tools.agent.capabilities]` fs grant **replaces** that derived default rather than adding to
it, so narrowing the ceiling narrows it.

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
  turn of every loop, the writing session's included. `tool_write`'s result says when: a name
  new to the loop waits for the next turn; a tool the loop already carries is callable now, and
  a rewrite takes effect on its next call.
- **Refused writes.** `tool_write` **MUST** refuse a write byte-identical to the stored tool —
  nothing is written, and reporting success is what lets a model rewrite the same source turn
  after turn — and **MUST** refuse an `input_schema` that is not a JSON object, which would
  otherwise become the tool's `parameters` and fail every turn that advertises it. A row stored
  before that check is skipped at load rather than projected.
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

A generated tool appears in `nine tools` (R-TVM.11) with **no manifest path**, a provenance
marker, and its dependency set; `SandboxedToolStatus.Generated` and `.Deps` carry these on
the wire (`spec/contracts/wire-protocol.md`).

`tool_write`, `tool_delete`, and `js_eval` are ordinary dispatched tools, so each call —
its source, declared capabilities, and result — is already recorded in the `session_events`
journal (`spec/contracts/event-journal.md`), attributed to the session and turn. The daemon
**additionally** logs each write/delete with the tool name, its declared reach, and its
resolved packages, as a greppable operator breadcrumb that survives a journal scrub.

---

## R-TVM.15 — external dependencies and the `nine:*` stdlib

A generated tool may import two kinds of module, both resolved **before** the call — never by
the guest, never at call time.

### The `nine:*` stdlib (§4.2)

A small, pinned, vendored set of pure-ES2023 modules (`nine:csv`, `nine:date`, `nine:diff`),
embedded in the binary and served host-side from the module map (R-TVM.8). It is the
generated tier's import allowlist: a generated tool may import any `nine:` module and nothing
else without deps. The modules are authored in-house rather than pulled from npm, so each is
known to run under the trimmed blob (R-TVM.9) and carries no transitive surface; the binary
always matches the stdlib of its version.

### External npm dependencies (§4.4)

**Off by default** (`[tools.agent.deps].mode = "off"`), and the single riskiest switch in the
design. When an operator enables it, `tool_write` resolves the tool's external imports **in
the daemon, at write time, once**, and inlines them into the stored source via esbuild
in-process. By call time the tool is one self-contained module with no imports but `nine:*`
and no network. The pipeline **MUST**:

1. **Policy-gate** every package. `allowlist` admits only operator-named packages, **transitive
   deps included** (else the list is decoration); `open` admits anything within the budgets.
2. **Verify integrity** — sha512 from the registry's `dist.integrity` — on the tarball
   **before** its contents are read. A mismatch is refused.
3. **Run no install scripts, ever.** The pipeline reads files out of a tarball; there is no
   install step, so npm's dominant attack vector structurally does not exist.
4. **Refuse Node builtins.** A package importing `fs`/`http`/`child_process`/`crypto` fails to
   bundle (esbuild `PlatformNeutral`), with an actionable error.
5. **Enforce budgets** — `max_packages` (incl. transitive), `max_bundle_kb`, `max_depth` — so a
   small allowlist cannot expand without bound.
6. **Record a lockfile** — name, version, integrity, requester per package — stored beside the
   tool and printed by `nine tools show` / `nine tools deps`.
7. **Resolve only from the cache when `frozen`**, never the network — the reproducible /
   air-gapped posture.

> **The interlock.** A tool that both declares `net.http` **and** resolves an external
> dependency is **refused** unless `[tools.agent] allow_network_deps = true`. A package that
> can reach the network can exfiltrate whatever the tool sees; the two features are
> individually reasonable and jointly a data-exfiltration primitive.

The containment argument: a malicious package is bounded by the tool's capabilities, and a
tool that declares none has none (R-TVM.6, I-TVM.1). The blast radius of "arbitrary npm" is
exactly the reach the operator already granted. The interlock, which keeps egress off the
table, is what holds that argument up.

New Go dependency: `github.com/evanw/esbuild/pkg/api` (pure Go, vendored). No Node, no npm
binary, no toolchain enters the runtime image — the resolver and bundler are in-process.

---

## R-TVM.18 — durable state

A tool granted `state` has a **host-owned key/value store**, scoped to itself. It is the
amendment to R-TVM.3 and the whole of it: nothing else about the instance model changes.

The store is a `tool_state` table keyed `(tool, scope_key, key)`
(`spec/contracts/memory-store.md`) — deliberately **not** the `kv` table, which is Nine's
own namespace and whose prefixes Nine reads its bookkeeping from.

### Scope

`scope` is a **required** grant parameter with **no default**, following the precedent
`methods` sets in R-TVM.12.

| `scope` | `scope_key` | Meaning |
|---|---|---|
| `tool` | `""` | one namespace shared by every caller of the tool |
| `conversation` | the owning conversation id | a separate namespace per conversation |

> **`scope = "tool"` is a cross-session information channel that needs no other
> capability.** A tool's arguments come from the model, and the model's arguments can carry
> anything in that session's context, so a call in session A can write them down and a call
> in session B can read them back — with no `net.http`, no `fs`, and no egress of any kind.
> The exfiltration is *into another conversation's context*, where none of the network
> controls look. This is documented rather than prevented; it is why scope is required and
> why the roster reports it.

A `conversation`-scoped grant reaching the host with **no conversation on the context MUST
be refused**, never served from the shared namespace. The fallback would confer exactly the
cross-conversation visibility the operator chose the scope to deny, and it would do so on
the paths that have no turn.

### Quotas

Per `(tool, scope_key)`: `max_keys` (default 128), `max_value_kb` (default 64),
`max_total_kb` (default 1024), and an optional `ttl`.

Exceeding one **MUST** be an error the guest can catch, never a silent drop — the same rule
R-TVM.12 sets for a refused request. A tool that believes it remembered something it did
not surfaces the fault a call later, as a cache that never hits.

Expiry **MUST** be applied on read as well as swept, so a `ttl` means what it says between
sweeps; and the sweep, not the read, is what reclaims the row, so a tool cannot keep a
value alive by never looking at it.

### The guest surface

`nine:state` exports `get`, `set`, `remove`, `keys`, `swap`, `scope`, and the
`getJSON`/`setJSON` conveniences. Values are strings.

`swap` (compare-and-set) is **required**, not a convenience: a module is instantiated per
call and two turns calling one tool at once is ordinary, so a read-modify-write spanning
two host calls is racy by construction.

### What it is not

- **Not a filesystem.** Flat keys, string values, no directories, no streaming.
- **Not a channel between tools.** The namespace is keyed by tool name; two tools cannot
  see each other's state and there is no shared prefix.
- **Not reachable by the model.** There is no `state_get` tool. The store is a tool's own
  bookkeeping.
- **Not audited per operation.** `net.http` audits every call because each reaches the
  outside world; a `get` reaches a row the tool already owns. What is recorded is the grant
  at load and the quota refusals.

---

## R-TVM.19 — long-running tools

A tool declaring `resumable = true` may be run as a **job**: a sequence of ordinary calls,
each carrying the cursor the last one returned.

**Nothing about the instance model changes.** Each call is created and destroyed exactly as
R-TVM.3 requires, under the same deadline and memory cap as any other call, with the same
audit. The work outlives the turn because the *host* keeps the state — never because
anything outlives the instance.

The unit is deliberately **a call**, not a step: "step" is already a workflow's durable unit
of delegated work (`docs/glossary.md`), and the two would be confused on sight.

### The job context

A resumable tool receives, alongside its arguments, `{cursor, call}` — the cursor it
returned last time and a 1-based call number. A `js` tool gets it as a second parameter; a
`wasm` tool gets it under the reserved `nine_job` key — a resumable `wasm` tool whose input
schema declares that property **MUST** be refused at load, so the reservation is enforced
rather than assumed. Arguments do not change between calls; the cursor is the only thing
that moves.

A conversation-scoped `state` grant (R-TVM.18) **MUST** resolve to the job's owner on every
call. Call one runs inside the turn that started the job and later calls do not, so without
this a tool granted state at conversation scope works exactly once.

### One registry, two backends

Jobs live in the `jobs` table (`spec/contracts/memory-store.md`), which serves both this and
long-running plugin work (`docs/plugin-capabilities.md` §5). The model-facing surface —
`job_wait`, `job_check`, `job_list`, `job_cancel` — is **unchanged and backend-agnostic**;
a tool job and a plugin job are the same thing to an agent, which is correct, since the
difference is an implementation detail of where the work runs.

The two backends are **not symmetrical**, and the differences are normative:

| | Plugin backend | Tool backend |
|---|---|---|
| The sweeper | **polls** work the plugin is doing | **is** the executor |
| Cadence | age-based poll backoff | the delay the tool asked for, floored |
| At boot | **MUST** be marked `lost` — the process is gone | **MUST** resume — the cursor is the row |
| Cancel | best-effort request | exact: no further call is made |
| Admission | post-hoc cancel; the plugin already started | **MAY** be refused before the first call |

`lost` is a plugin-backend state. A tool job **MUST NOT** be marked lost at boot: its entire
live state is its row, so there is nothing unreachable about it.

### Bounds

Per call, R-TVM.4 applies unchanged. Per job:

| Bound | Config | Default |
|---|---|---|
| Total calls | `[tools] job_max_calls` | 720 |
| Minimum delay between calls | `[tools] job_min_delay_ms` | 250 |
| Concurrent calls the sweeper makes | `[tools] job_workers` | 4 |
| Lifetime | `[plugins] job_max_seconds` | 1h |
| Per conversation | `[plugins] max_jobs_per_conversation` | 8 |
| Daemon-wide outstanding jobs | `[plugins] max_jobs_total` | 32 |

The call cap is **required**, not defensive. Each call is individually legal; a tool
returning `continue` with `after_ms: 0` forever converts a bounded CPU story into an
unbounded one one legal call at a time. The delay floor exists for the same reason.

The cap **MUST** be checked before a call is spent, so a job at its limit fails without one
last call.

**A job MUST NOT be called twice concurrently.** The sweeper may run distinct jobs in
parallel up to `job_workers` — each is a separate wasm instantiation holding up to
`[tools] memory_mb`, which is what that bound is really sizing — but two calls of one job
would run against the same cursor and lose whichever finished first. The reference
implementation gets this by waiting for the batch before returning, so a slow call cannot
still be running when the next sweep finds its row due.

The **daemon-wide** cap bounds the machine where the per-conversation cap bounds one agent.
It matters more for this backend than for plugins: a plugin job is work another process
performs, a tool job is work the daemon performs.

### The generated tier

`[tools.agent] allow_long_running` (default **false**) gates a generated tool declaring
itself resumable. It is deliberately **not** part of the capability ceiling: a ceiling
bounds what a tool may *reach*, and duration is not reach. A capability-free tool that never
stops is inert per call and unbounded in aggregate — precisely what a ceiling cannot
express.

`js_eval` **MUST** refuse a continuation. It persists nothing by definition (R-TVM.14), and
a job is persistence: there is no row to carry a cursor and nothing to resume.

### Delivery

Unchanged from the plugin backend and from `adr/reactive-events.md`: a finished job posts a
notification to its owner and **never wakes anything**. It is read on the owner's next turn.

---

## R-TVM.20 — standing tools

A resumable tool (R-TVM.19) **MAY** also be run **standing**: indefinitely, on its own
cadence, started by configuration rather than by a turn.

It is a second *run mode*, not a second kind of tool. Same sandbox, same capability
resolution, same per-call deadline, same `continue` envelope, same driver. A tool author
writes one kind of resumable tool and the operator decides how it runs.

| | Job (R-TVM.19) | Standing run |
|---|---|---|
| Started by | the model, mid-turn | configuration, at boot |
| Owner | the conversation | the operator |
| Ends | when the tool returns a result | when stopped |
| A returned result means | the job is done | **one cycle** is done |
| Bounds | `job_max_calls`, `job_max_seconds` | health limits, not a call budget |

### Cycles

Calls run until the tool returns a result instead of asking to continue. That completes one
**cycle**: the cursor **MUST** reset, and the trigger decides when the next cycle begins.

So a standing run has **two cadences** — the trigger between cycles, and `after_ms` within
one. A tool that finishes in a single call simply has one-call cycles.

### The trigger

`interval` **xor** `schedule`, reusing the parsing standing agents use
(`docs/scheduling.md`) with its stated limits. Setting both, or neither, **MUST** be a
config error rather than a skipped block: a standing tool runs unattended, so one the
operator wrote and Nine silently ignored is the worst available outcome.

### Ownership

**Configuration owns the definition — tool, args, trigger. The runtime owns the run
state.** This is the split `docs/predefined-agents.md` already settles for standing agents,
and it holds here for the same reason: a standing tool an operator stopped **MUST** stay
stopped across a restart and across a reconcile.

Changing `args` **MUST** restart the cycle — the cursor was produced under the old
arguments. Removing a block stops Nine reconciling it and **MUST NOT** delete its row.

### Reporting

A cycle's output goes to the **human feed** and nowhere else.

Empty output **MUST** be silent. A watcher that runs every ten seconds and speaks only when
it finds something is useful; one that announces every pass is a notification storm.

A standing tool **MUST NOT** be able to address an agent or wake anything *of its own
accord*. The first keeps deterministic tool code from steering an autonomous agent with no
human in between; the second is R-SUB.3's enrich-don't-interject, unchanged.

> **The one exception, and why it is not one.** A standing run **MAY** carry a wake target,
> making it a standing agent's **condition trigger** (`docs/scheduling.md`): its findings
> wake that agent instead of reaching the human feed. The target is written by an operator
> in their own configuration and can be requested by neither the tool nor the agent, so the
> human is in the loop when the link is made rather than each time it fires. A finding that
> cannot be delivered — the agent is not running, or is mid-turn — **MUST** fall back to the
> human feed rather than being dropped.

### Health

The failure this design expects is not a crash — it is a tool that throws on every call for
a week while nobody notices. So:

- consecutive failures **MUST** back off from the tool's own cadence, to a cap;
- a threshold of consecutive failures **MUST** move the run to `failing`, which is visible
  in the roster and carries the last error;
- **only the transitions** into and out of `failing` notify. A flapping tool must not
  produce a storm.

`failing` is still running: it means "retrying on a backed-off cadence", not "given up".
There is no terminal state — a standing run is stopped or it is going.

### Observability

**A standing run's ordinary calls MUST NOT be journal events.** A tool on a
ten-second cadence is 8,640 calls a day; journalling each would swamp
`session_events`, distort the retention scrub, and bury what `nine trace` exists
to show.

What **MUST** be journaled is transitions and output: started, stopped, entering
and leaving `failing`, and any cycle that produced something. Volume is then
proportional to things happening rather than to time passing. Events are
attributed to the standing tool's id as the agent, so `nine trace <id>` reads a
standing tool's history the way it reads a session's.

The counterpart rule is unchanged: **anything a standing tool does that reaches
the world is audited as usual.** Every `net.http` call it makes is journaled per
R-TVM.12, unamended. A standing tool's heartbeat is not an event; what it does
is.

Per-call detail lives in counters on the row and in a bounded, in-memory ring
buffer — deliberately lossy and deliberately not durable.

### The operator surface

| | |
|---|---|
| `nine tools standing` | the roster, with state, cycles and trigger |
| `nine tool status <id>` | one run in full, with recent activity |
| `nine tool logs <id> [-n N]` | the ring buffer |
| `nine tool stop\|start <id>` | exact: stopping means no further call is scheduled |
| `nine tool call <name> ['<json>'] [--live-state]` | one call, for testing |

`nine tool call` **MUST** run against a **scratch state namespace** unless
`--live-state` is given, and **MUST** return the whole envelope including a
`continue`. A test call sharing a live standing run's store could overwrite its
cursor, and an operator who "just tested it" would have silently corrupted the
production run. Capabilities are **not** sandboxed: a test that cannot make the
tool's real calls tests nothing. The isolation is of state alone.

### The generated flavour

`[tools.agent] allow_standing` (default **false**) gates a generated tool asking
to be run standing; `max_standing` (default 4) bounds how many may exist, counting
generated runs only.

**A standing promotion MUST route through the HITL gate, including when
`require_approval` is `never`.** That setting says the capability ceiling is the
only control, and a ceiling bounds *reach* — a capability-free tool that runs
forever is inert per call and unbounded in aggregate, which is precisely what a
ceiling cannot express. Arguments that cannot be parsed **MUST** be treated as a
promotion (fail closed).

A **generated** standing tool that fails repeatedly **MUST** be disabled. A
config-declared one **MUST NOT** be: an operator's declaration is a standing
instruction, and silently switching it off would be the more surprising
behaviour, where a tool Nine wrote has no author to answer to.

### Concurrency

Standing calls share the job driver's worker budget (`[tools] job_workers`). What both
bound is the same scarce thing: concurrent wasm instantiations, each holding up to
`[tools] memory_mb`. One standing tool **MUST NOT** be called twice concurrently, for the
reason R-TVM.19 gives.

---

## R-TVM.13 — fully built

Every feature `docs/sandboxed-tools.md` specifies is implemented (stages 1–6). The
`nine:*` stdlib (§4.2), external npm dependencies (§4.4), and the `deps` + `net.http`
interlock are R-TVM.15; the generated tier is R-TVM.14. Nothing in that design remains
stubbed or refused-by-name.

Durable state (R-TVM.18) and long-running tools (R-TVM.19) are **beyond** that design
rather than part of it: R-TVM.18 amends R-TVM.3, which stages 1–6 took as fixed, and
R-TVM.19 adds a second lifecycle to the same tool. Both halves of
`adr/durable-and-long-running-tools.md` are built, and the standing-tool lifecycle
(`adr/standing-tools.md`) is built — R-TVM.20, all five phases.

---

## Invariants

- **I-TVM.1** — The default capability set is empty. A tool that declares nothing gets
  nothing, regardless of what any other tool was granted.
- **I-TVM.2** — No agent-reachable tool or path writes a **grant**. `tool_write` writes a
  generated tool's *code and declaration* (R-TVM.14); the operator writes every grant and the
  generated ceiling, in `nine.toml`. Nine cannot grant itself capabilities (R-PLUG.7,
  unchanged) — the manifest of a developer tool and the declaration of a generated one are
  descriptions, not grants.
- **I-TVM.3** — No state survives a call **implicitly**. The guest's globals, heap, and
  interpreter realm are destroyed at return, so two calls cannot observe each other through
  the machine. Anything that persists does so through a host-owned surface the operator
  granted, named by the tool, bounded by quota, and visible in the roster (R-TVM.18).
- **I-TVM.4** — A tool name resolves to exactly one implementation. Sandboxed tools never
  override built-ins or plugin tools.
- **I-TVM.5** — The committed interpreter links neither `std` nor `os`. The narrow
  `nine:fs`/`nine:env` surface it does expose reaches only the operator's pre-opens and
  granted env keys, enforced by wazero rather than by a path check of Nine's own, and
  exposes no `exec`, no `urlGet`, and no `evalScript` (R-TVM.9).
- **I-TVM.8** — `nine.caps` describes a grant and never confers one. Nothing reads it back
  to make an enforcement decision.
- **I-TVM.6** — `[tools] enabled = false` ⇒ no host, no tools, and loops identical to those
  built before this subsystem existed. The flag is a pointer in config precisely so that an
  operator declining the default is distinguishable from one who said nothing.
- **I-TVM.7** — A sandboxed tool cannot reach a loopback, link-local, or private address,
  whatever its `allow_hosts` says and whatever any hostname resolves to.

---

## R-TVM.17 — `fs.write` includes directory creation

The filesystem host functions **MUST** include creating a directory and its missing
parents, gated by the same `fs.write` grant as writing a file.

It is not a convenience. Without it a granted tool can write `a.txt` and cannot write
`notes/a.txt`, because the write primitive creates a file and not a path — and the tool
cannot recover, since there is no other way to make the directory either. That gap is what
kept the first-party `files` tools on the plugin transport: their `write_file` promises to
create parent directories, and a sandboxed replacement that could not would have been a
downgrade for every role carrying the tool.

Creation is **recursive and idempotent**: an existing directory is success, so a tool may
call it before every write. Containment remains the pre-open's — the implementation walks
path components and each resolves inside the mount because the guest has nothing else to
resolve against. Nothing in the guest enforces this, and nothing in the guest could.

## R-TVM.16 — the shipped tier (first-party tools in the binary)

A third source tier, after developer (R-TVM.10) and generated (R-TVM.14): tools whose
source is **compiled into the daemon binary**. They run through the same host, the same
ABI (R-TVM.1), the same instance model (R-TVM.3), and the same bounds (R-TVM.4) as every
other tool.

It exists so that capabilities Nine ships with are subject to the capability model. Before
it, the built-in capabilities were **plugins** — subprocesses inheriting the daemon's uid,
so a tool that read a clock had, in principle, the reach to read the operator's home
directory. Not because anyone wanted that, but because a subprocess inherits it.

A shipped tool is **granted what it declares**. That is the one way this tier differs from
the other two, and it is a reduction rather than a new trust: a developer tool is granted
by an operator who did not write it, and a generated tool is capped by a ceiling because
Nine wrote it, but a shipped tool is first-party code the operator already ran with
*strictly more* authority. The grant **MUST** still appear in the tool roster, and the host
**MUST** refuse a declaration it cannot actually enforce rather than registering a tool
whose capability silently does nothing.

Shipped tools **MUST** load **before** the other tiers. The namespace rule is
first-registered-wins, so loading them last would let a developer or generated tool take a
first-party name and silently replace its behavior.

A shipped tool that declares `fs` is mounted at the operator's workspace
(`[workspace].root`), under the fixed guest path `/work` — the alias the `files`
plugin already accepted, so a model that learned `/work/notes.txt` keeps working. A tool
declaring `fs` with no workspace configured **MUST** fail to load rather than register with
a capability that silently does nothing. The daemon creates the root if it is absent.

**Deleting is recoverable.** A shipped tool that removes or replaces a workspace file
**MUST** move the previous contents under `.nine/trash/` rather than destroy them, and
**MUST** offer a way to list and restore them. Approval gates arm only for interactive
sessions (`hitl.md` R-HITL.5), so a goal session, a standing agent, or any sub-agent
deletes with nobody to stop it; recoverability is what stands in for the supervision those
runs do not have. The trash lives inside the workspace because a path outside it is one the
operator never offered, and because a rename within the mount costs nothing while a copy
across volumes costs the whole file. Identical content **MUST NOT** be trashed on
overwrite, or a tool that rewrites a file unchanged fills the trash with copies of it.

`.nine/` is Nine's own bookkeeping inside the operator's directory. The write tools
**MUST** refuse it, so the record of what was deleted cannot be rewritten by the agent
that deleted it, and it **MUST** be excluded from a repository the operator mounted.

A conforming implementation **MUST** bound the trash by **both** age and total size. Age
alone lets a week of large deletions outgrow the volume; size alone keeps a single stale
file for ever on a quiet system. Age **MUST** be taken from the trash entry, not the file's
mtime: a rename preserves mtime, so a trashed file otherwise reports when it was last
written, which may be long before anyone deleted it.

**A change is reviewable.** A conforming implementation **MUST** be able to render a
workspace change as a diff at three points: before it happens (the write tools take a
`preview` argument that returns the diff and writes nothing), inside an approval prompt,
and after the fact against the trashed previous version. An approval gate that names only
the path asks a human to approve a change it has not shown them.

The prompt's diff **MUST** come from the tool's own preview rather than a second
implementation: what the human approves has to be what the tool then performs. A preview
that fails **MUST NOT** fail the approval — the gate falls back to naming the path, since a
prompt with less detail is better than a tool that cannot run. Both the diff and the prompt
**MUST** be bounded; an oversized change **MUST** still report how many lines it touches,
because a prompt that scrolls is one nobody reads, and a truncated diff that renders as
empty hides the largest changes exactly when they matter most.

The workspace has two names: the guest mount and the host directory `shell` runs in and
prints. A conforming implementation **MUST** accept the host path in a shipped fs tool's
`path` argument and map it onto the mount, because a model that copies a path out of shell
output otherwise names a file the guest has no name for. The mapping is argument
normalization performed by the host, which knows both names; the guest is still told only
its mount (R-TVM.8), and a path outside the root is passed through unchanged for the
pre-open to refuse.

**A shipped tool has no more access to host state than any other.** This is the tier's
sharpest constraint and the easiest to forget, because the code is first-party. The
reference's `time` reports **UTC**, where the plugin it replaced reported the daemon's
local time and zone: the guest has no timezone database, and a tool cannot know the host's
zone unless the host confers it. Migrating a built-in to this tier therefore **MAY** change
what it returns, and that change **MUST** be documented rather than papered over.
