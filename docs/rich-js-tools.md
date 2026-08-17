# Design note — Richer sandboxed tools: the JS environment, and what belongs beneath it

**Status:** Design agreed (§8), nothing built · **Roadmap:** the "missing JS globals",
"FS/env gaps", "binary data support", and "structured tool errors" parts of *Improve
sandboxed tools* · **Precedes:** durable state, long-running tools

This note is about what a tool author can actually *call*. It changes no capability
boundary and asks for no new operator trust: every gap below is either a surface that was
granted and cannot be reached, or an API absent for no reason other than that nobody has
written it yet.

It started as a JS-only investigation and did not stay one. Roughly half of what looked
like a JavaScript problem is really a problem in the ABI or the host, where the fix serves
the `wasm` kind equally — and the `wasm` kind turns out to have gaps of its own, in the
opposite direction. **The organizing rule of this note is: fix each gap at the lowest layer
that serves both kinds.**

Everything asserted here was measured — against the committed blob (quickjs-ng v0.16.1,
`internal/toolvm/quickjs/VERSION`) for the JS side, and against purpose-built C modules
compiled with the wasi-sdk for the wasm side. Nothing is read off a compatibility table.

---

## 1. The finding, in one table

Which capabilities can a tool of each kind actually *use*, once granted?

| Capability | `wasm` | `js` |
|---|---|---|
| `log` | ✅ `nine.log` host import | ✅ `console.*` |
| `clock`, `random` | ✅ WASI | ✅ `Date`, `Math.random` |
| `net.http` | ✅ `nine.http` host import — **undocumented** | ✅ `fetch` |
| `fs.read` / `fs.write` | ✅ libc → WASI pre-opens | ❌ **no API exists** |
| `env` | ✅ `getenv` | ❌ **no API exists** |

Verified end to end. A raw C module, granted everything, in one call:

```text
WASM CAPS => log=called fs.read=OK(from-host) readdir=OK(3) fs.write=OK
             env=Europe/Paris ungranted_env=absent
```

The same probes from JavaScript:

```text
STATUS reader  loaded=true caps=fs.read /tmp/xxx=>/data
FS PROBE  out="reachable file APIs: NONE"

STATUS envtool loaded=true caps=env DEMO_TZ
ENV PROBE out="reachable env APIs: NONE"
```

**`fs.read`, `fs.write`, and `env` are declarable, grantable, validated at load, and
printed by `nine tools` — and unreachable from the kind nearly every tool is written in.**
Both JS tools above load successfully and report their capability. Nothing warns anyone:
not the author, not the operator, not `nine tool validate`.

The cause is structural rather than an oversight. `moduleConfig` (`host.go:385`) implements
`fs` as a wazero pre-open and `env` via `WithEnv` — **WASI-level** facilities that a raw
`.wasm` tool reaches through `fopen` and `getenv`. The `js` kind is the shared QuickJS
blob, which deliberately links neither `std` nor `os` (`I-TVM.5`), where quickjs-libc keeps
its filesystem bindings. The capability lands on the module and the interpreter has no
JavaScript-visible way to touch it.

The scoping, at least, is correct on the side that works: a key the operator did *not*
grant is genuinely invisible (`ungranted_env=absent`), and an ungranted tool calling
`nine.http` from C gets `blocked: this tool was not granted the net.http capability` rather
than a connection.

---

## 2. The three layers

Sorting the gaps by where they live is what keeps this from becoming two parallel piles of
work.

| Layer | What lives there | Serves |
|---|---|---|
| **L1 — the ABI envelope** | `nine_alloc`/`nine_run`, the JSON crossing in and out, `Result{ok,output,error}` | **Both kinds** |
| **L2 — host imports** | `nine.log`, `nine.http`, and anything added beside them | **Both kinds** |
| **L3 — guest environment** | Web-platform APIs for JS; libc for wasm | One kind each |

Read that way, the roadmap's "missing JS globals" splits cleanly:

- **Binary data, structured errors, the oversized-result message, and knowing your own
  grant are L1/L2 problems** that present as JS problems only because JS is where people
  hit them first. Fixing them in the harness would fix them for one kind and leave the
  other broken in exactly the same way.
- **`TextEncoder`, `URL`, `structuredClone`, timers, `crypto`** are genuinely L3 and
  genuinely JS-only. A wasm author has libc and needs none of them.
- **`fs` and `env` for JS are L3**, and the right shape for them is "make JavaScript see
  what C already sees," not "invent a Nine filesystem API."

---

## 3. What the `wasm` kind is missing

The kind that can reach everything is also the kind nobody can find out how to use.

**The host imports are real, work, and are undocumented.** A raw C module can import
`nine.log` and `nine.http` and use both — verified:

```text
WASM HTTP (granted)   => raw-wasm saw: {"status":200,"headers":{…},"body":"{\"hi\":\"there\"}"}
WASM HTTP (ungranted) => raw-wasm saw: {"error":"blocked: this tool was not granted the net.http capability"}
```

Meanwhile `docs/writing-sandboxed-tools.md` tells a wasm author they get "No runtime, no
imports, no capabilities you did not declare," and documents only `nine_alloc`/`nine_run`.
Both `fetch`-equivalent networking and logging are available to them and effectively
undiscoverable. An author reading the guide would reasonably conclude that a wasm tool
cannot log, and write a pure function that cannot tell them why it failed.

**There is no header.** A wasm author reconstructs the import attributes, the packing
convention, and the result envelope from prose. The two-export ABI is small enough that
this is survivable and large enough that everyone will get the `(offset << 32) | length`
packing wrong once.

**A wasm tool cannot ask what it was granted.** For a `js` tool the input is an envelope
with room to add to; for a `wasm` tool `input()` (`js.go:87`) passes the model's arguments
through *verbatim* — there is nowhere to put a capability summary without changing what
every existing module parses. This is the one place where the two kinds cannot be fixed
the same way, and §6.3 proposes the way out.

---

## 4. Three places the environment is silently wrong

Absence is survivable — an author hits `TextEncoder is not defined` and works around it.
These are worse, because the tool returns a confident wrong answer. **The first two are L1,
and hit both kinds.**

**Binary HTTP responses are corrupted, not refused.** `httpResponse.Body` is a Go `string`
(`nethttp.go:61`) marshalled into JSON, so every byte that is not valid UTF-8 becomes
U+FFFD before any guest sees it. Fetching a PNG:

```text
sent:     [137 80 78 71 13 10 26 10 255 254 0 1]
received: [65533 80 78 71 13 10 26 10 65533 65533 0 1]
```

Three bytes destroyed, `res.ok` true, no error. This happens on the *host* side of
`nine.http`, so a raw wasm tool reading `body` out of that JSON gets the identical damage —
it is not a harness bug, and cannot be fixed in the harness.

**Binary request bodies are stringified.** `harness.js:62` does `String(init.body)`, so
`fetch(url, { body: new Uint8Array([1,2,3,255]) })` puts the nine characters `1,2,3,255` on
the wire. Verified against a real server. The JS half is a harness bug; the *envelope*
having no way to express bytes is the L1 half, and a wasm tool has the same problem in the
other direction.

**`toLocaleString` accepts options it ignores** (JS only). Without `Intl`, QuickJS falls
back to a non-localized implementation that still accepts the arguments:

```text
new Date(0).toLocaleString("en-US", { timeZone: "Europe/Paris" })  =>  "01/01/1970, 12:00:00 AM"
(1234567.891).toLocaleString("de-DE")                              =>  "1234567.891"
```

The first is UTC — the requested zone dropped, an hour off, no diagnostic. A
timezone-conversion tool built on this is wrong in a way that only shows up in production.
`Intl.DateTimeFormat` at least fails loudly.

---

## 5. The JS guest environment (L3)

**Present, and more current than the docs claim.** The authoring guide says "ES2023, and
nothing else." It undersells the blob: every TypedArray including `Float16Array`,
`SharedArrayBuffer`, `WeakRef`, `FinalizationRegistry`, `Proxy`, `Reflect`, iterator
helpers, `DisposableStack`, `Object.groupBy`, `Array.prototype.toSorted`,
`Promise.withResolvers`, `RegExp.escape` (ES2025), unicode property escapes, `Error`
`cause`, and `normalize` all work — as do `atob`/`btoa`, `performance`, and
`queueMicrotask`.

**Absent:**

| Missing | Consequence |
|---|---|
| `TextEncoder` / `TextDecoder` | No UTF-8 ↔ bytes. With `btoa` being Latin-1 only, base64 of any non-ASCII string is impossible: `btoa("中")` throws *String contains an invalid character*. |
| `URL` / `URLSearchParams` | Every tool that builds a query string does it by hand, wrongly. |
| `crypto` | No `getRandomValues`, no `randomUUID`. `Math.random()` is correctly seeded per call (verified) but is not a CSPRNG. |
| `setTimeout` / `clearTimeout` / `setInterval` | Any bundled dependency that debounces, retries with backoff, or polls fails at call time. |
| `structuredClone` | Deep copy via `JSON.parse(JSON.stringify(x))`, with its usual lies about `Date` and `undefined`. |
| `Intl` | §4 — not merely absent, silently wrong. |
| `Blob`, `AbortController`, `process`, `Buffer`, `Temporal` | Absent, and mostly correctly so (§7). |

None of these are ECMAScript. They are the web platform layer, which QuickJS has never
claimed to provide. **A newer interpreter fixes none of it.**

### Four JS papercuts

**Module-level `console.log` throws.** `harness.js` installs `console` and `fetch` in its
own module body but reaches the tool through a *static* `import tool from "nine:tool"` —
and ES semantics evaluate an imported module **before** the importing module's body:

```text
typeof console at module scope was: undefined
console.log("hi") at module scope  =>  tool "m": console is not defined
```

A top-of-file `console.log` is the first thing anyone writes when debugging, and it fails
with an error implying the sandbox forbids logging — which it does not.

**The `nine:*` stdlib is withheld from developer tools.** `nine:csv`, `nine:date`, and
`nine:diff` are embedded, pure-ES, and importable only by *generated* tools, because
`imports` is populated from `stdlibModules()` in `generated.go` alone. The comment
explaining this (`js.go:77`) says the stdlib "exists for the *generated* tier, which does
not yet exist here" — written before that tier landed. A leftover, not a decision, and
backwards: the hand-writing author is the one who cannot ask Nine to write them a CSV
parser.

**Harness internals are writable globals.** `__nine_log`, `__nine_http`, `__nine_args`, and
`__nine_result` sit on `globalThis`, enumerable and replaceable. Not a security boundary —
the host enforces every policy on its own side — but they collide with author code and
invite tools to bind to internals we want to keep changing.

**An oversized result reports `out of memory`** (L1, both kinds). Returning 8 MiB under the
default 16 MiB cap fails with a bare *tool "big": out of memory*, naming neither the limit,
nor `memory_mb`, nor the fact that the JSON envelope roughly doubles a string on its way
out. 1 MiB works, 8 MiB does not, and nothing says where the line is.

---

## 6. The design

### 6.1 L1 — the envelope, for both kinds

**Binary data.** `httpResponse` gains `body_b64`, set instead of `body` when the response
is not valid UTF-8; the decision is the host's, where the bytes still exist. Request bodies
gain `body_b64` in the same shape. A tool of either kind may return
`{"ok":true,"output_b64":"…"}` to hand back bytes. On the JS side the harness builds
`fetch`'s `arrayBuffer()`/`bytes()` on top, makes `text()` throw on a binary body rather
than return mojibake, and accepts `Uint8Array`/`ArrayBuffer` for `init.body`. **The wasm
author gets the same capability for free, by reading one more field.**

**Structured errors.** `Result` gains an optional structured error — `name`, `code`, and a
`cause` chain — so the model can distinguish "your argument was malformed, fix it" from
"the upstream is down, do not retry." Today everything flattens to a bare string; verified
that `code`, `retryable`, `cause`, and thrown non-`Error` objects all collapse to
`.message`. The harness fills it from the `Error` object it already holds; a wasm tool
fills it by writing two more JSON fields.

**The oversized-result message** maps the guest OOM to something naming `memory_mb` and the
observed size.

### 6.2 L2 — host imports, for both kinds

Whatever is added beside `nine.log` and `nine.http` should be added *there*, not in the
harness, so both kinds get it at once. That is the argument against putting the filesystem
here, though — see §6.4.

### 6.3 Letting a tool ask what it was granted

Both kinds currently guess. A JS tool granted nothing gets `ENOENT`-shaped confusion; a
wasm tool has no envelope to carry a summary in.

Add **`nine.caps`**, a host import returning the calling tool's resolved grant as JSON.
It works for both kinds, needs no ABI change, and — because a module that does not import
it is completely unaffected — breaks no existing tool. The harness uses it to say
`fs.read is not granted to this tool` before attempting anything, and a wasm author can
call it directly.

**It is for error messages and self-description only. Enforcement stays exactly where it
is:** in wazero's pre-opens and in the host's per-call grant lookup. A tool learning its
own grant learns nothing it could not already discover by trying.

### 6.4 L3 — `nine:fs` and `nine:env` for JavaScript

The goal is to make JavaScript see **what C already sees**, so there is one mental model
rather than two:

```js
import { readFile, writeFile, readDir, stat } from "nine:fs";
import { get } from "nine:env";

export default ({ name }) => {
  const raw = readFile(`/data/${name}`);       // the guest path, exactly as a wasm tool uses it
  return { bytes: raw.length, tz: get("TZ") };
};
```

Implemented as C functions in `qjs_host.c` over ordinary libc — **not** as new host
imports. This is the most important call in the note, and it rests on something easy to
miss: **wasi-libc is already linked.** The blob is built with the wasi-sdk against
`wasi-sysroot` as a reactor; what is *not* linked is quickjs-libc's `std`/`os` JS bindings.
So `fopen`, `readdir`, `getenv`, and `getentropy` are available to `qjs_host.c` today and
route through WASI to exactly the pre-opens `moduleConfig` already configures.

That preserves the property the code currently boasts about (`host.go:416`) — *"the one
capability wazero enforces itself: a pre-open is a real capability primitive, and a tool
scoped to /srv/data cannot walk out of it without our writing a single check."* Routing
files through a `nine.fs_read` host function instead would make containment **our**
path-checking code, reviewed by us, bug-for-bug ours. It is the one case where the
serves-both-kinds instinct gives the wrong answer: wasm already has this through libc, and
JS should get it the same way rather than both being rebuilt on a weaker foundation.

Rules: guest paths only; `readFile` returns a `Uint8Array` and `readFileText` decodes UTF-8
(defaulting to bytes is what stops §4's corruption class reappearing one layer down);
`nine:env` sees only granted keys for free, since `WithEnv` passes nothing else.

`I-TVM.5` is untouched — no `std`, no `os`, no `exec`, no `urlGet`, no `evalScript`.

### 6.5 L3 — the web-platform layer

`TextEncoder`, `TextDecoder` (UTF-8 only), `URL`, `URLSearchParams`, `structuredClone`, and
UTF-8-safe base64 helpers: pure JS in the harness, no blob rebuild. Plus `crypto`
(`getRandomValues` in C over `getentropy`, `randomUUID` layered in JS; no `subtle`).

### 6.6 L3 — the JS papercuts

Replace the static `import tool from "nine:tool"` with `await import("nine:tool")` *after*
the globals are installed; populate `imports` from `stdlibModules()` for developer tools
too; capture `__nine_log`/`__nine_http` into closures and `delete` them from `globalThis`.

### 6.7 Documenting the `wasm` kind

**Done (M1).** `nine.h` — the two exports, the import attributes for `nine.log` and
`nine.http`, the packing macros, argument reach-in, and escaping envelope builders — is
embedded in the binary and written out by `nine tool header`. It comes from the binary
rather than a repository file for the reason docs/ and spec/ do: a header describing the
ABI must match the build implementing it. `nine.caps` is deliberately absent from it until
§6.3 lands, since declaring an import the host does not export fails at instantiation.

Then correct the guide: a wasm tool *does* get imports, and can log and make HTTP requests.

---

## 7. What we deliberately do not add

- **No Node compatibility.** No `require`, no `process`, no `Buffer`. Anything reaching for
  a Node builtin fails to bundle today, and that rules out a large share of npm before
  policy enters the picture — a feature.
- **No `Intl`.** ICU is megabytes of tables against a 1 MB interpreter compiled into every
  `nine` binary. §4's problem is *silence*, not absence.
- **No `crypto.subtle`.** Large asynchronous surface; `getRandomValues` and `randomUUID`
  cover what tools need, and a tool wanting AES-GCM can bundle a pure-JS implementation.
- **No real timers.** §8.
- **No general filesystem.** `nine:fs` is `readFile`/`writeFile`/`readDir`/`stat` over the
  granted mounts. No `chmod`, no symlink games, no `..` traversal — none of which we
  implement, because WASI does not hand them to us.

---

## 8. Decisions

Settled 2026-08-16. Each kept its reasoning, because the reasoning is what a later reader
will want to argue with.

1. **Timers: a virtual-time queue.** A tool must not sleep, so `setTimeout(fn, 1000)` cannot
   mean a second. Callbacks queue and drain in *deadline order* after the tool's promise
   settles, so relative ordering holds and no wall clock is burned. This is the most
   compatible with bundled dependencies that debounce or back off, and the most surprising
   to anyone who measures elapsed time and sees zero — so it must be documented loudly
   rather than quietly shimmed. The one genuine semantic choice in this note.
2. **`toLocaleString` throws** on options it cannot honor. It converts a silent wrong answer
   into a loud failure the model can read and route around. It breaks any tool currently
   getting away with it, which is the point: those tools are already wrong, and today they
   have no way to find out.
3. **No `ABIVersion` bump** for `body_b64`/`output_b64`. `ABIVersion` versions the
   two-export contract, which does not change, and the new envelope fields are additive — a
   wasm tool that ignores them behaves exactly as it does now. Bumping would fail every
   existing `abi = 1` manifest to buy nothing.
4. **Still open: whether `nine:fs` should exist when ungranted.** Importable-and-refusing
   matches how `net.http` already behaves and keeps one code path; absent-entirely is
   arguably clearer. It only becomes urgent at M5. **Leaning importable.**

**Build order: M1 first** — `nine.h` and the guide correction (§6.7). Shipped; M2 and M3
are next and are independent of each other.

---

## 9. Milestones

| | Scope | Layer | Blob rebuild | Serves |
|---|---|---|---|---|
| **M1** ✅ done | `nine.h` + correcting the wasm guide (§6.7) | docs | No | wasm |
| **M2** | Structured errors, OOM message (§6.1) | L1 | No | both |
| **M3** | Binary data end to end (§6.1) | L1 + harness | No | both |
| **M4** | Web-platform layer + papercuts (§6.5, §6.6) | L3 | No | js |
| **M5** | `nine:fs`, `nine:env`, `crypto`, `nine.caps` (§6.3, §6.4) | L2 + L3 | **Yes** | both |

M1 is an afternoon and unblocks the kind that currently has the most capability and the
least documentation. M2–M4 need no blob rebuild and are mutually independent. M5 is the
only entry that touches `qjs.wasm`, so everything requiring a rebuild is deliberately
pooled into one carefully-reviewed artifact change.

---

## 10. Spec and doc impact

- `spec/contracts/toolvm.md` — `R-TVM.9`/`I-TVM.5` need re-wording, not weakening: the
  interpreter still links no `std`/`os`, but it now exposes a narrow, capability-gated fs
  and env surface. Worth stating as a new invariant: *`nine:fs` reaches only the operator's
  pre-opens, enforced by wazero, not by a path check of ours.*
- The host-import surface (`nine.log`, `nine.http`, and `nine.caps`) should be specified as
  part of the guest contract for **both** kinds, not described only in the JS narrative.
- `docs/sandboxed-tools.md` §4.1 — the "they were never built" paragraph must distinguish
  quickjs-libc's `std`/`os` from our own narrow modules.
- `docs/writing-sandboxed-tools.md` — "**QuickJS-NG — ES2023, and nothing else**" is
  already inaccurate (§5); the wasm section's "no imports" is actively misleading (§3).
- The capability table in both guides should stop implying `fs`/`env` work for `js` tools
  until M5 lands. **This is worth doing immediately, ahead of any code** — the documentation
  currently promises a working surface that does not exist.
