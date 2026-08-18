# Design note — Richer sandboxed tools: the JS environment, and what belongs beneath it

**Status:** Complete. Kept as a decision record, not a plan · **Roadmap:** the "missing JS globals",
"FS/env gaps", "binary data support", and "structured tool errors" parts of *Improve
sandboxed tools* · **Precedes:** durable state, long-running tools

This note is about what a tool author can actually *call*. It changes no capability
boundary and asks for no new operator trust: every gap below is either a surface that was
granted and cannot be reached, or an API absent for no reason other than that nobody has
written it yet.

**This is what is left of a plan that has been carried out.** The milestone tracking, the
measurements that justified each step, and the gap analysis have been removed; what remains
is the reasoning someone will want when they question one of these choices. The work itself
is described where it lives — `nine docs writing-sandboxed-tools` for the surface, `nine
spec toolvm` for the contract.

Two things shaped every decision below and are worth stating once. **Fix each gap at the
lowest layer that serves both tool kinds** — which moved binary data, structured errors, and
grant introspection out of the JS harness and into the ABI or the host, where a `wasm` tool
gets them too. And **JavaScript is the supported language**: since `nine:fs`, `nine:env`,
and `crypto` landed there is no capability a `js` tool cannot reach, so the C header and
examples were removed and other languages run on a specified but unsupported contract.

---

## 1. The three layers

Sorting the gaps by where they live is what keeps this from becoming two parallel piles of
work.

| Layer | What lives there | Serves |
|---|---|---|
| **L1 — the ABI envelope** | `nine_alloc`/`nine_run`, the JSON crossing in and out, `Result{ok,output,error}` | **Both kinds** |
| **L2 — host imports** | `nine.log`, `nine.http`, and anything added beside them | **Both kinds** |
| **L3 — guest environment** | Web-platform APIs for JS; libc for wasm | One kind each |

That rule split what looked like one problem into three:

- **Binary data, structured errors, the oversized-result message, and knowing your own
  grant are L1/L2 problems** that present as JS problems only because JS is where people
  hit them first. Fixing them in the harness would fix them for one kind and leave the
  other broken in exactly the same way.
- **`TextEncoder`, `URL`, `structuredClone`, timers, `crypto`** are genuinely L3 and
  genuinely JS-only. A wasm author has libc and needs none of them.
- **`fs` and `env` for JS are L3**, and the right shape for them is "make JavaScript see
  what C already sees," not "invent a Nine filesystem API."

---

## 2. What is deliberately absent

- **No Node compatibility.** No `require`, no `process`, no `Buffer`. Anything reaching for
  a Node builtin fails to bundle today, and that rules out a large share of npm before
  policy enters the picture — a feature.
- **No `Intl`.** ICU is megabytes of tables against a 1 MB interpreter compiled into every
  `nine` binary. The problem with the Intl-less fallback was *silence*, not absence: it accepted a locale and ignored it. It now throws.
- **No `crypto.subtle`.** Large asynchronous surface; `getRandomValues` and `randomUUID`
  cover what tools need, and a tool wanting AES-GCM can bundle a pure-JS implementation.
- **No real timers.** See the timer decision below.
- **No general filesystem.** `nine:fs` is `readFile`/`writeFile`/`readDir`/`stat` over the
  granted mounts. No `chmod`, no symlink games, no `..` traversal — none of which we
  implement, because WASI does not hand them to us.

---

## 3. Decisions

Each keeps its reasoning, because the reasoning is what a later reader will want to argue
with. The first three were settled before implementation; the fourth during it.

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
4. **Settled at M5: `nine:fs` is importable when ungranted**, matching how `net.http`
   already behaves, and a call throws `fs.read is not granted to this tool`. The refusal is
   a message rather than a mechanism — with no pre-opens there is nothing to open, which a
   test asserts by bypassing the module and calling the primitive directly.

**Build order:** all of M1–M5 and M3b are shipped. Every gap this note opened with is
closed, and §8's four questions are all answered.

---

## 4. Where this is written down

These decisions are enforced and described elsewhere; this file explains *why*, and those
files are what a reader should trust about *what*.

| | |
|---|---|
| The surface an author uses | `nine docs writing-sandboxed-tools` |
| The design of the tool host | `nine docs sandboxed-tools` |
| The normative contract | `nine spec toolvm` — `R-TVM.3` (ABI), `R-TVM.9`/`I-TVM.5` (interpreter surface), `R-TVM.12` (net.http and its audit), `I-TVM.8` (`nine.caps` describes, never confers) |
| Worked examples | `examples/tools/` — `csvstats` (no capabilities) and `linkcheck` (fs.read + net.http) |

The one invariant this work amended rather than added: `I-TVM.5` used to say only that the
interpreter links no `std`/`os`. It still does not — but it now also exposes a narrow,
capability-gated `nine:fs`/`nine:env`, and the invariant says so, along with the property
that makes it acceptable: those reach only the operator's pre-opens, enforced by wazero
rather than by a path check of Nine's own.
