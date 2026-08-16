# Design note — Rich JavaScript for sandboxed tools

**Status:** Draft, nothing built · **Roadmap:** the "missing JS globals" and "FS/env
gaps" halves of *Improve sandboxed tools* · **Precedes:** durable state, long-running
tools

This note is about the `js` tool kind's *guest environment* — what a tool author can
actually call. It changes no capability boundary and asks for no new operator trust: every
gap below is either a surface that was granted and cannot be reached, or an API absent for
no reason other than that nobody has written it yet.

Everything asserted here was measured against the committed blob (quickjs-ng v0.16.1,
`internal/toolvm/quickjs/VERSION`), not read off a compatibility table.

---

## 1. The finding

**The language is current. The platform around it is missing, and three capabilities the
operator can grant cannot be used from JavaScript at all.**

That second half is the part that should be uncomfortable. `fs.read`, `fs.write`, and
`env` are declarable in a manifest, grantable in `nine.toml`, validated at load, and
printed by `nine tools` — and a `js` tool handed any of them has no API with which to use
them. The grant is real, the enforcement is real, the surface does not exist.

```text
$ # a js tool granted fs.read over a real directory
STATUS reader loaded=true caps=fs.read /tmp/xxx=>/data
FS PROBE out="reachable file APIs: NONE"

$ # a js tool granted env DEMO_TZ
STATUS envtool loaded=true caps=env DEMO_TZ
ENV PROBE out="reachable env APIs: NONE"
```

Both tools load. Both report their capability. Neither can do anything with it. Nothing
warns anyone — not the author, not the operator, not `nine tool validate`.

The cause is structural rather than an oversight. `moduleConfig` (`internal/toolvm/host.go:385`)
implements `fs` as a wazero pre-open and `env` via `WithEnv`, which are **WASI-level**
facilities: a raw `.wasm` tool reaches them through `fopen` and `getenv`. The `js` kind is
the shared QuickJS blob, and that blob deliberately links neither `std` nor `os`
(`I-TVM.5`), which is where quickjs-libc keeps its filesystem bindings. So the capability
lands on the module, and the interpreter has no JavaScript-visible way to touch it.

The `wasm` kind can use all five capability types. The `js` kind — the one nearly every
tool will be written in — can use two.

---

## 2. What is actually there

Measured by enumerating `globalThis` inside a live call:

**Present, and more current than the docs claim.** The authoring guide says "ES2023, and
nothing else." It undersells the blob. Every TypedArray including `Float16Array`,
`BigInt64Array`, `SharedArrayBuffer`, `WeakRef`, `FinalizationRegistry`, `Proxy`,
`Reflect`, iterator helpers, `DisposableStack`/`AsyncDisposableStack`, `SuppressedError`,
`DOMException`, `Object.groupBy`, `Array.prototype.toSorted`, `Promise.withResolvers`,
`RegExp.escape` (ES2025), unicode property escapes, `Error` `cause`, and
`String.prototype.normalize` all work. `atob`/`btoa`, `performance`, and `queueMicrotask`
are there too, as are `console` and `fetch` from the harness.

**Absent:**

| Missing | Consequence |
|---|---|
| `TextEncoder` / `TextDecoder` | No UTF-8 ↔ bytes. With `btoa` being Latin-1 only, base64 of any non-ASCII string is impossible: `btoa("中")` throws *String contains an invalid character*. |
| `URL` / `URLSearchParams` | Every tool that builds a query string does it by hand, wrongly. |
| `crypto` | No `getRandomValues`, no `randomUUID`. `Math.random()` is correctly seeded per call, but is not a CSPRNG and should not be used as one. |
| `setTimeout` / `clearTimeout` / `setInterval` | Any bundled dependency that debounces, retries with backoff, or polls fails at call time. |
| `structuredClone` | Deep copy by `JSON.parse(JSON.stringify(x))`, with its usual lies about `Date` and `undefined`. |
| `Intl` | See §3 — it is not merely absent, it is silently wrong. |
| `Blob`, `AbortController`, `process`, `Buffer`, `Temporal` | Absent, and mostly correctly so (§6). |

The shape of that list is the point: **none of these are ECMAScript.** They are the web
platform layer, which QuickJS has never claimed to provide and which nobody has written
for this blob. The fix is not a newer interpreter.

---

## 3. Three places the current environment is silently wrong

Absence is survivable — an author hits `TextEncoder is not defined` and works around it.
These are worse, because the tool returns a confident wrong answer.

**Binary HTTP responses are corrupted, not refused.** `httpResponse.Body` is a Go `string`
(`internal/toolvm/nethttp.go:61`) marshalled into JSON, so every byte that is not valid
UTF-8 becomes U+FFFD before the guest sees it. Fetching a PNG:

```text
sent:     [137 80 78 71 13 10 26 10 255 254 0 1]
received: [65533 80 78 71 13 10 26 10 65533 65533 0 1]
```

Three bytes destroyed, `res.ok` true, no error anywhere. A tool that hashes, decodes, or
forwards a binary body produces garbage and reports success.

**Binary request bodies are stringified.** `harness.js:62` does `String(init.body)`, so
`fetch(url, { body: new Uint8Array([1,2,3,255]) })` puts the nine characters `1,2,3,255`
on the wire. Verified against a real server.

**`toLocaleString` accepts options it ignores.** Without `Intl`, QuickJS falls back to a
non-localized implementation that still accepts the arguments:

```text
new Date(0).toLocaleString("en-US", { timeZone: "Europe/Paris" })  =>  "01/01/1970, 12:00:00 AM"
(1234567.891).toLocaleString("de-DE")                              =>  "1234567.891"
```

The first is UTC — the requested zone was dropped, an hour off, with no diagnostic. The
second ignores the locale entirely. A timezone-conversion tool built on this is wrong in a
way that only shows up in production, and `Intl.DateTimeFormat` at least fails loudly
(*Intl is not defined*).

---

## 4. Four papercuts with the same root

**Module-level `console.log` throws.** `harness.js` installs `console` and `fetch` in its
own module body, but reaches the tool through a *static* `import tool from "nine:tool"` —
and ES semantics evaluate an imported module **before** the importing module's body. So at
tool module scope both globals are still undefined:

```text
typeof console at module scope was: undefined
typeof fetch at module scope was: undefined
console.log("hi") at module scope  =>  tool "m": console is not defined
```

A top-of-file `console.log` is the first thing anyone writes when debugging. It fails with
an error that suggests the sandbox forbids logging, when logging is granted by default.

**The `nine:*` stdlib is withheld from developer tools.** `nine:csv`, `nine:date`, and
`nine:diff` are embedded, pure-ES, and dependency-free — and importable only by generated
tools, because `imports` is populated from `stdlibModules()` in `generated.go` alone. A
hand-written tool gets *module "nine:csv" is not available to this tool*. The comment
explaining this (`js.go:77`) says the stdlib "exists for the *generated* tier, which does
not yet exist here" — written before that tier landed. The restriction is a leftover, not
a decision, and it is exactly backwards: the developer tool is the one whose author cannot
ask Nine to write a CSV parser for them.

**Harness internals are writable globals.** `__nine_log`, `__nine_http`, `__nine_args`, and
`__nine_result` all sit on `globalThis`, enumerable and replaceable. Not a security
boundary — the host enforces every policy on its own side, and a tool forging its own
output gains nothing — but they are visible to `Object.keys(globalThis)`, collide with
author code, and invite tools to bind to internals we want to keep changing.

**An oversized result reports `out of memory`.** Returning 8 MiB under the default 16 MiB
cap fails with a bare *tool "big": out of memory*, naming neither the limit, nor
`memory_mb`, nor the fact that the JSON envelope roughly doubles a string on its way out.
1 MiB works, 8 MiB does not, and nothing tells you where the line is.

---

## 5. The lever: two extension points, very different costs

This is the fact that should drive sequencing.

| | `harness.js` | `qjs_host.c` |
|---|---|---|
| Ships as | Embedded source (`js.go:20`) | The 1 MB committed `qjs.wasm` |
| To change | `make build` | `make quickjs-wasm`, new `qjs.wasm.sha256`, `VERSION` bump, reviewed as a binary diff |
| Can add | Anything expressible in pure JS | Anything reaching libc/WASI |

**Most of what is missing is pure JavaScript.** `TextEncoder`/`TextDecoder`, `URL`,
`URLSearchParams`, `structuredClone`, UTF-8-safe base64, the module-order fix, and the
internals cleanup are all a few hundred lines in the harness, with no blob rebuild and no
new host import. That is a cheap, reviewable, self-contained first change.

The genuinely host-shaped items — the filesystem, the environment, and a real CSPRNG —
need the blob. And they need less of it than expected, because of something easy to miss:
**wasi-libc is already linked.** The blob is built with the wasi-sdk against
`wasi-sysroot` as a reactor; what is *not* linked is quickjs-libc's `std`/`os` JS bindings.
So `fopen`, `readdir`, `getenv`, and `getentropy` are all available to C in `qjs_host.c`
right now, and they route through WASI to exactly the pre-opens and env pairs
`moduleConfig` already configures.

That matters for more than convenience. It means the filesystem capability keeps the
property the design note currently boasts about (`host.go:416`) — *"the one capability
wazero enforces itself: a pre-open is a real capability primitive, and a tool scoped to
/srv/data cannot walk out of it without our writing a single check."* Exposing files to
JavaScript through libc preserves that. Routing them through a new `nine.fs_read` host
function would not: containment would become our path-checking code, reviewed by us,
bug-for-bug ours. **We should not add a host function for the filesystem.**

---

## 6. What we deliberately do not add

Worth stating, because each will be proposed eventually.

- **No Node compatibility.** No `require`, no `process`, no `Buffer`, no `fs` module by
  that name. Anything reaching for a Node builtin fails to bundle today, and that rules out
  a large share of npm before policy enters the picture — which is a feature.
- **No `Intl`.** ICU is megabytes of tables against a 1 MB interpreter, and the blob is
  compiled into every `nine` binary. §3's problem is *silence*, not absence, and is fixed by
  making the fallback loud plus pointing at `nine:date`.
- **No `crypto.subtle`.** A large asynchronous surface. `getRandomValues` and `randomUUID`
  cover what tools actually need; a tool wanting AES-GCM can bundle a pure-JS implementation.
- **No real timers.** See the open question in §8 — `setTimeout` should exist, but a
  sandboxed pure function under a 5-second wall clock must not be able to *sleep*.
- **No general filesystem.** `nine:fs` is `readFile`/`writeFile`/`readDir`/`stat` over the
  granted mounts. No `chmod`, no symlink games, no `..` traversal — none of which we
  implement, because WASI does not hand them to us.

---

## 7. The design

### 7.1 `nine:fs` and `nine:env` — closing the granted-but-unreachable gap

Two new C-implemented modules in `qjs_host.c`, over ordinary libc calls:

```js
import { readFile, writeFile, readDir, stat } from "nine:fs";
import { get } from "nine:env";

export default ({ name }) => {
  const raw = readFile(`/data/${name}`);       // guest path, as mounted
  return { bytes: raw.length, tz: get("TZ") };
};
```

Design rules, each doing real work:

- **Guest paths only.** A tool sees `/data`, never the host path. The operator can move or
  narrow the mount without the tool changing — the property the grant table already promises.
- **`readFile` returns a `Uint8Array`; `readFileText` decodes UTF-8.** Defaulting to bytes
  is what stops §3's corruption class from being reintroduced one layer down.
- **Ungranted is a clear refusal, not an obscure errno.** With no pre-opens, `fopen`
  returns `ENOENT` and an author sees "no such file" for a file that plainly exists. The
  envelope should carry a capability *summary* — for message wording only, never for
  enforcement — so the module can say `fs.read is not granted to this tool` before trying.
- **`nine:env` sees only granted keys**, for free: `WithEnv` passes nothing else, so
  `getenv` cannot observe a key the operator did not name. No filtering code to get wrong.

Enforcement stays entirely in wazero. We add no path checks, and `I-TVM.5` is untouched —
we link no `std`, no `os`, and expose no `exec`, no `urlGet`, no `evalScript`.

### 7.2 Binary data, end to end

- `httpResponse` gains `body_b64`, set instead of `body` when the response is not valid
  UTF-8. The decision is the host's, where the bytes still exist.
- `fetch` gains `arrayBuffer()` and `bytes()`; `text()` on a binary body throws rather than
  returning mojibake.
- `init.body` accepts `Uint8Array`/`ArrayBuffer` and base64-encodes it for the host, instead
  of `String()`-ing it into nonsense.
- A tool may return a `Uint8Array`; `render()` base64-encodes it under a declared envelope
  rather than serializing an object with numeric keys.

### 7.3 The web-platform layer, in the harness

`TextEncoder`, `TextDecoder` (UTF-8 only), `URL`, `URLSearchParams`, `structuredClone`, and
UTF-8-safe `btoa`/`atob` helpers — pure JS, no blob rebuild.

### 7.4 `crypto`

`getRandomValues` in C over `getentropy` (WASI `random_get`, already fed from
`crypto/rand` by `WithRandSource`), with `randomUUID` layered in JS. No `subtle`.

### 7.5 The papercuts

- **Module order:** replace the static `import tool from "nine:tool"` with
  `await import("nine:tool")` *after* the globals are installed. Pure harness change, and it
  makes module-scope `console.log` work as every author expects.
- **Stdlib for developer tools:** populate `imports` from `stdlibModules()` for the
  developer tier too. Roughly a one-line change; the reasoning that excluded it has expired.
- **Internals:** capture `__nine_log`/`__nine_http` into closures and `delete` them from
  `globalThis`; move `__nine_args`/`__nine_result` behind a `Symbol` or a single
  non-enumerable internal object.
- **Result size:** map the guest OOM to a message naming `memory_mb` and the observed
  output size.

### 7.6 Structured errors

The harness already holds the `Error` object and throws away everything but `.message`.
Verified: `code`, `retryable`, `cause`, and thrown non-Error objects all flatten to a bare
string. `Result` should carry an optional structured error — `name`, `code`, `cause` chain
— so the model can distinguish "your argument was malformed, fix it" from "the upstream
service is down, do not retry." This is listed separately on the roadmap; it belongs here
because the harness is where the information is lost.

---

## 8. Open questions — decisions needed before building

1. **Timers.** A tool must not sleep, so `setTimeout(fn, 1000)` cannot mean a second. The
   honest options are (a) a *virtual-time* queue drained in deadline order after the tool's
   promise settles, so ordering semantics hold and no wall clock is burned; (b) treat every
   delay as zero, i.e. `queueMicrotask`; (c) leave it absent. (a) is the most compatible with
   bundled dependencies and the most surprising if someone measures elapsed time. **Leaning
   (a), documented loudly.** This is the one genuine semantic choice in the note.
2. **Whether `toLocaleString` should throw** when handed options it cannot honor. It
   converts a silent wrong answer into a loud failure, at the cost of breaking any tool
   currently getting away with it. **Leaning yes.**
3. **Whether `nine:fs` should exist when ungranted.** Importable-and-refusing matches
   `fetch` and keeps one code path; absent-entirely is arguably clearer. **Leaning
   importable**, for consistency with §8's existing treatment of `net.http`.
4. **Whether any of this warrants an `ABIVersion` bump.** Everything above is additive to
   the guest environment, and `ABIVersion` versions the two-export contract, which does not
   change. **Leaning no** — but `body_b64` changes the *host↔guest JSON*, which is worth an
   explicit decision rather than an assumption.

---

## 9. Milestones

| | Scope | Blob rebuild | Notes |
|---|---|---|---|
| **M1** | Harness-only: §7.3, §7.5, §7.6 | No | The cheap, self-contained majority. Ships alone. |
| **M2** | `nine:fs` + `nine:env` (§7.1) | **Yes** | Closes the granted-but-unreachable gap. The substantive change. |
| **M3** | Binary data (§7.2) | No | Host-side JSON + harness. Independent of M2. |
| **M4** | `crypto` (§7.4) | **Yes** | Fold into M2's rebuild rather than paying for a second one. |
| **M5** | Docs + spec reconciliation | No | See §10. |

M1 and M3 are independent of the blob and of each other. M2 and M4 should be one PR, since
the expensive, carefully-reviewed artifact in both is the same `qjs.wasm`.

---

## 10. Spec and doc impact

- `spec/contracts/toolvm.md` — `R-TVM.9`/`I-TVM.5` need re-wording, not weakening: the
  interpreter still links no `std`/`os`, but it now exposes a narrow, capability-gated fs
  and env surface. A new invariant is worth stating: *`nine:fs` reaches only the operator's
  pre-opens, enforced by wazero, not by a path check of ours.*
- `docs/sandboxed-tools.md` §4.1 — the "they were never built" paragraph needs to
  distinguish quickjs-libc's `std`/`os` from our own narrow modules.
- `docs/writing-sandboxed-tools.md` — "**QuickJS-NG — ES2023, and nothing else**" is
  already inaccurate (§2) and would become more so. It should name what is present, what is
  absent, and why `Intl` is absent on purpose.
- The capability table in both guides should stop implying `fs`/`env` work for `js` tools
  until M2 lands. **This is worth doing immediately, ahead of any code** — the documentation
  currently promises a working surface that does not exist.
