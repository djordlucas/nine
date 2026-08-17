# Writing a sandboxed tool

A **sandboxed tool** lets you add a permanent tool to Nine without writing a plugin,
building a binary, or rebuilding the image. You drop two files in a directory; the daemon
runs your code in a wasm sandbox with exactly the capabilities the operator granted it.

> **Who grants what.** You write the code and *declare* what it needs. The operator
> running Nine writes the *grant*. These are never the same person, and a manifest that
> asks for more than it was granted does not load. That asymmetry is the whole point — see
> [Capabilities](#capabilities).

---

## The two files

```text
/etc/nine/tools.d/
  csvstats.toml     # the manifest — the gate
  csvstats.js       # your code
```

A `.js` or `.wasm` file with **no manifest beside it is never loaded**. Turn the
subsystem on in `nine.toml`:

```toml
[tools]
enabled  = true
user_dir = "/etc/nine/tools.d"
```

Then `nine tools` shows the roster and `nine tools reload` re-scans it.

---

## Your first tool

```js
// csvstats.js
export default ({ csv }) => {
  const rows = csv.trim().split("\n").map((line) => line.split(","));
  return { rows: rows.length, columns: rows[0]?.length ?? 0 };
};
```

```toml
# csvstats.toml
name        = "csv_stats"
kind        = "js"
entrypoint  = "./csvstats.js"
description = "Summary statistics over a CSV string."
input_schema = "./csvstats.schema.json"
```

Default-export a function. Return whatever you like:

- a **string** is passed through untouched — it is your own formatting, and it is what the
  model reads;
- **anything else** is JSON-stringified;
- an **async function** works with no ceremony;
- a **thrown error** reaches the model as an ordinary tool failure carrying your message,
  which is exactly what you want for `throw new Error("date is not ISO-8601")` — the model
  can read it and retry with better arguments.

### Say whether it is worth retrying

"The upstream is down" and "your argument was malformed" read the same as prose and call for
opposite behavior. Attach that to the error and the model is told rather than left guessing:

```js
const e = new Error("weather API timed out");
e.code = "E_UPSTREAM";   // your own stable identifier; survives rewording
e.retryable = true;      // false means "trying again cannot help"
throw e;
```

`name`, `code`, `retryable`, and the ES2022 `cause` chain are all carried through and
rendered into what the model reads:

```text
tool "weather": weather API timed out (code E_UPSTREAM, retryable)
tool "parse": date is not ISO-8601 (code E_ARGS, not retryable); caused by: unexpected token
```

**`retryable: false` does more than inform the model: Nine stops retrying.** A failed tool
call is normally attempted three times, on the assumption that a failure is a flake. Saying
the failure is not retryable takes your tool out of that loop the way a human's refusal of
an approval already is — one attempt, then the model is told:

```text
Tool "weather" failed after 1 attempt(s): tool "weather": city must be a string
(code E_ARGS, not retryable). The tool reports this cannot succeed on retry;
change the arguments or use a different tool.
```

**Omitting `retryable` is not the same as `false`** — one says you have no opinion, the
other says trying again will not work, and only the second changes the retry behavior. A
plain `throw new Error("…")` still produces exactly the message it always did and is still
retried three times, so none of this is required.

`console.log` works and goes to the daemon log. It is the only way out of the sandbox you
have without a grant.

### The manifest

| Field | Required | Notes |
|---|---|---|
| `name` | ✅ | What the model calls. Must match `[a-z0-9_]+`. Shares one namespace with built-ins and plugin tools — a collision **skips your tool entirely**. |
| `kind` | ✅ | `"js"` or `"wasm"`. |
| `entrypoint` | ✅ | Path to your file, relative to the manifest. |
| `description` | ✅ | The whole basis on which the model decides to call your tool. Write it for a reader who has never seen your tool. |
| `input_schema` | | Path to a JSON Schema file. Omit for a tool taking no arguments. |
| `display_name` | | Human-friendly label for the TUI. Never reaches the model. |
| `abi` | | Guest ABI version. Omit; it defaults correctly. |
| `[capabilities]` | | What you need. See below. |

**The manifest is authoritative** — it is where the model's view of your tool comes from,
so `name`, `description`, and `input_schema` all live here. An unknown key is an error
rather than a warning: in a file whose job is declaring capabilities, a typo'd key silently
meaning nothing is the worst possible outcome.

Check it before you deploy — no daemon needed:

```console
$ nine tool validate /etc/nine/tools.d
  ok    csv_stats          js     declares: none
```

---

## Capabilities

**The default is nothing.** No filesystem, no network, no environment variables. Most
tools need nothing — a pure transform over its arguments is the ideal shape, and it is the
one that runs anywhere without an operator having to think.

If you do need reach, declare it:

```toml
[capabilities]
fs  = ["read"]
env = ["TZ"]
```

…and the operator grants it, by name, in *their* `nine.toml`:

```toml
[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]
```

Your code sees the **guest** path (`/data`), which is what lets the operator narrow or
move the mount without your tool changing.

**The two must agree exactly.** Declaring something ungranted fails to load; being granted
something you did not declare *also* fails to load. Both are loud, by design — a tool that
half-works in ways neither of you predicted is worse than one that refuses to start. `nine
tools` shows you the reason:

```console
$ nine tools
  ok    csv_stats          js     fs.read /srv/data=>/data
  SKIP  weather            js     net.http declared but not granted; add [tool.weather.capabilities.net.http]
```

| Capability | In a `js` tool | In a `wasm` tool | Granted by default |
|---|---|---|---|
| `clock`, `random` | `Date.now()`, `Math.random()` | WASI | ✅ |
| `log` | `console.*` | `nine.log` | ✅ |
| `net.http` | `fetch()` | `nine.http` | ❌ declare + grant |
| `fs.read` / `fs.write` | **not yet reachable** | `fopen`, `readdir` | ❌ declare + grant |
| `env` | **not yet reachable** | `getenv` | ❌ declare + grant |

> **`fs` and `env` do not work from JavaScript yet.** They are implemented as WASI
> facilities, which a `wasm` tool reaches through libc and the QuickJS interpreter has no
> binding for. A `js` tool that declares either will *load*, and `nine tools` will report
> the capability, and no API will exist to use it. Write such a tool as `kind = "wasm"`
> until this is closed — the design and the plan are in `nine docs rich-js-tools`.

`NINE_*` and `*_API_KEY` environment keys can never be granted: the daemon's environment
holds the LLM provider credentials.

### Network access

Declare `net = ["http"]`, and the operator grants the hosts:

```toml
[tool.weather.capabilities.net.http]
allow_hosts = ["api.weather.example"]
methods     = ["GET"]
max_bytes   = 1048576
```

Then `fetch` works:

```js
export default async ({ city }) => {
  const res = await fetch(`https://api.weather.example/v1?q=${encodeURIComponent(city)}`);
  if (!res.ok) throw new Error(`weather API returned ${res.status}`);
  return res.json();
};
```

It is a **subset** of the `fetch` you know, not a polyfill. You get `status`, `ok`,
`headers`, `text()`, `json()`, `bytes()`, and `arrayBuffer()`. There is no streaming, no
`AbortController`, no cookie jar, and no `Request`/`Headers`/`Response` classes.

Three behaviors differ from browser `fetch` and are worth knowing:

- **A blocked request throws**, it does not return a non-ok response. A refusal is not a
  response, and letting it look like one invites `if (res.ok)` to quietly swallow a
  decision the operator made. The message says why.
- **Redirects are followed but re-checked.** Every hop must independently satisfy the
  allowlist, and `Authorization`/`Cookie` are stripped when a hop crosses origins.
- **`text()` on a binary body throws** rather than returning mojibake. See below.

### Binary bodies

Everything crossing the sandbox boundary is UTF-8 JSON, and a JSON string cannot hold
arbitrary bytes — so bytes are handled explicitly rather than squeezed through a text field:

```js
const res = await fetch(url);
const bytes = await res.bytes();        // Uint8Array, whatever the body was
await fetch(other, { method: "POST", body: bytes });   // sent as bytes, not "1,2,3"
```

`bytes()` and `arrayBuffer()` work on any response. `text()` and `json()` work on a
response that is valid UTF-8 and **throw** on one that is not, naming the alternative:

```text
response body is not valid UTF-8 text; use bytes() or arrayBuffer()
```

That is deliberate. Previously a PNG arrived through `text()` with every invalid byte
replaced by U+FFFD, `res.ok` true and nothing reporting it — so a tool that hashed or
forwarded a binary body produced garbage and called it success. A throw puts the failure
where the mistake is.

A `Uint8Array`, `ArrayBuffer`, or any typed-array view passed as `init.body` is sent as
bytes. A string is still sent as text.

From C, the same split appears in the JSON: a non-UTF-8 response comes back as `body_b64`
**instead of** `body`, and `nine_b64_decode` in `nine.h` decodes it. Send bytes by setting
`body_b64` on the request.

What you cannot reach, regardless of `allow_hosts`: loopback, link-local (including
`169.254.169.254`, the cloud instance-metadata endpoint), RFC 1918, and the other
non-routable ranges. The check is on the address actually dialed, so pointing a permitted
hostname at one of them does not help. That is deliberate and not configurable.

You also cannot set `Host`, `Content-Length`, or hop-by-hop headers, and only `http` and
`https` are permitted.

---

## What JavaScript you get

**QuickJS-NG, plus a small platform layer, and no Node.**

The language is current — more current than "ES2023" suggests. Alongside the obvious
(`JSON`, `Map`/`Set`, regular expressions, generators, `async`/`await`) you get every
TypedArray, `Proxy`/`Reflect`, `WeakRef`, iterator helpers, `Object.groupBy`,
`Array.prototype.toSorted`, `Promise.withResolvers`, `Error` `cause`, unicode property
escapes, and `RegExp.escape`.

Around it, the host objects a tool actually reaches for:

| | Notes |
|---|---|
| `console.*` | Goes to the daemon log. |
| `fetch` | A subset, with the `net.http` grant. See above. |
| `TextEncoder` / `TextDecoder` | **UTF-8 only.** Another label throws rather than quietly producing UTF-8. |
| `URL` / `URLSearchParams` | A pragmatic subset — absolute URLs and resolution against a base. No IDNA, no full WHATWG state machine. |
| `structuredClone` | Handles cycles, `Date`, `Map`/`Set`, `RegExp`, TypedArrays. |
| `setTimeout` / `setInterval` / `clear*` | **Virtual time** — see below. |
| `atob` / `btoa`, `performance`, `queueMicrotask` | As you expect. `btoa` is Latin-1, so base64 UTF-8 via `TextEncoder`. |

**No Node standard library**: no `fs`, `http`, `path`, `Buffer`, `process`, or `require`.
No `crypto` yet, and no `Intl` — see below.

### Timers do not sleep

A tool runs inside one turn under a wall-clock deadline that is also the only CPU bound
there is, so it must never sleep. Timers therefore run in **virtual time**: the queue
executes in deadline order, and no real time passes.

```js
setTimeout(() => order.push("second"), 200);
setTimeout(() => order.push("first"), 100);
await new Promise((r) => setTimeout(r, 300));   // returns immediately
```

Ordering — which is what a bundled dependency that debounces or backs off actually depends
on — works. What does not is measuring elapsed time: `Date.now()` will show roughly zero
across that 300 ms wait, because the clock is real and the timer is not. A `setInterval`
that never stops fails with a clear message rather than hanging until the deadline.

### `Intl` is absent, and formatting says so

ICU is megabytes of tables against a 1 MB interpreter compiled into every `nine` binary.
The problem with the fallback is not that it is missing but that it *lied*: it accepted a
locale and an options bag and ignored them, so

```js
new Date(0).toLocaleString("en-US", { timeZone: "Europe/Paris" })   // was "01/01/1970, 12:00:00 AM"
```

returned UTC, an hour off, with no diagnostic. **Passing a locale or options now throws.**
Calling it with no arguments still works, because the default format is not a lie. For real
formatting, `import { formatISODate } from "nine:date"`.

### Dependencies: bundle them yourself

**For a developer tool, Nine never resolves a dependency** — that is your job, at build
time. A `.js` file that still imports a *package* when Nine loads it will fail at call
time. (Generated tools are the exception, and get their own resolver — see below.)

The one exception is the `nine:*` standard library, which any tool may import. It is
embedded in the binary, pure ES, and dependency-free, so there is nothing to resolve:

```js
import { parse, format } from "nine:csv";
import { parseDate, isoWeek, formatISODate } from "nine:date";
import { lineDiff, unified } from "nine:diff";
```

Bundle at development time instead:

```console
$ npx esbuild tool.js --bundle --format=esm --platform=neutral --outfile=csvstats.js
```

This is the right place for the risk to live. You already have a `package.json`, a
lockfile, `npm audit`, and code review; the bundle lands in your repo as a reviewable
artifact. Nine inherits none of that machinery and none of that responsibility.

Pure-ESM, zero-dependency packages bundle fine. Anything touching a Node builtin will not
— which rules out a large share of npm before policy even enters the picture.

---

## Generated tools: the tier Nine writes itself

Everything above is about **developer** tools — files you install. Nine can also write its
own tools at runtime, when the operator turns the generated tier on. These live in Nine's
store rather than on disk, but they run in the identical sandbox under the identical rules,
and they get two import routes a developer tool does not.

Turn the tier on (off by default, and independent of `[tools] enabled`):

```toml
[tools.agent]
enabled          = true
eval             = true            # also allow one-off snippets that persist nothing
max_tools        = 64             # cap the catalog; least-recently-used are evicted
require_approval = "on_capability" # prompt a human only when a tool asks for reach

# The ceiling: the MOST any generated tool may be granted — never automatic. A tool that
# declares nothing still gets nothing. Narrow the mount if your workspace holds secrets.
[tools.agent.capabilities.fs]
read = [{ host = "${NINE_WORKSPACE}", guest = "/workspace" }]
```

### The `nine:*` standard library

Available to every tool, generated or hand-written (above) — a small, curated set of
pure-JavaScript modules, with no config, no bundling, and no network:

```js
import { parse, format } from "nine:csv";
import { parseDate, isoWeek, formatISODate } from "nine:date";
import { lineDiff, unified } from "nine:diff";

export default ({ csv }) => ({ rows: parse(csv, { header: true }).length });
```

### External npm packages

Off by default, and the single riskiest switch here. When an operator enables it, a
generated tool can `import` a real package and Nine resolves it — once, at write time, in
the daemon — verifies the download against its published checksum, runs no install scripts,
and inlines it into the tool. By the time the tool runs it has no imports left but `nine:*`
and no way to reach the network.

The operator names what may be imported, and the versions they stand behind:

```toml
[tools.agent.deps]
mode  = "allowlist"        # off (default) | allowlist | open
allow = [
  { name = "date-fns", version = "^4.1.0" },
  { name = "papaparse", version = "^5.4.1" },
]
frozen = false            # true = resolve only from cache, never the network
```

A generated tool then simply imports it, and the write inlines it:

```js
import { formatISO, addDays } from "date-fns";
export default ({ from, days }) => ({ due: formatISO(addDays(new Date(from), days)) });
```

Two rules worth knowing:

- **A package plus `net.http` is refused** unless the operator explicitly sets
  `allow_network_deps = true`. A dependency that can reach the network could send anything
  the tool sees anywhere — the sandbox stops being a boundary. Leave it off.
- **Packages needing Node built-ins won't bundle.** Anything reaching for `fs`, `http`, or
  `crypto` fails with a clear error, which rules out a large share of npm up front.

Inspect what got pulled in:

```console
$ nine tools deps
  due_date          date-fns@4.1.0

$ nine tools show due_date
  due_date
    kind          generated (js)
    status        loaded
    capabilities  none
    dependencies  date-fns@4.1.0
```

---

## The `wasm` kind

If you want full speed or another language, ship a `.wasm` module directly. Export two
functions:

```text
nine_alloc(size i32) -> i32     # reserve size bytes; return the offset
nine_run(ptr i32, len i32) -> i64   # run; return (offset << 32) | length
```

`nine_run` receives your arguments as UTF-8 JSON and returns UTF-8 JSON:

```json
{"ok": true,  "output": "..."}
{"ok": false, "error":  "..."}
```

There is no `free` — the instance is destroyed when the call returns, so everything is
reclaimed at once and you need not track lifetimes.

The same tiny shape works from C, Rust, TinyGo, or Zig: allocate a buffer, read JSON in,
write JSON out. No runtime, and no capabilities you did not declare.

### In C, use the header

The binary emits that ABI as a C header — the exports, the `(offset << 32) | length`
packing, the host imports below, and envelope builders that escape your output properly:

```console
$ nine tool header > nine.h
```

It comes from `nine` rather than from a file in the repository for the reason the docs you
are reading are also inside the binary: a header describing the ABI must match the build
that implements it, and a copy on disk drifts silently. Include it and write one function:

```c
#include "nine.h"

NINE_TOOL(args, len) {
    char name[256];
    if (nine_arg_str(NINE_ARGS(args), len, "name", name, sizeof(name)) < 0)
        return nine_fail("expected a string argument 'name'");
    nine_log("greeting someone");
    return nine_ok(name);
}
```

`nine_ok` and `nine_fail` exist because hand-building the envelope with `sprintf` breaks
the moment your output contains a quote or a newline — a failure that depends on your data
rather than your code, which is the worst kind to debug.

For the retry distinction described above, `nine_fail_code` carries it from C:

```c
return nine_fail_code("weather API timed out", "E_UPSTREAM", NINE_RETRY_YES);
return nine_fail_code("date is not ISO-8601",  "E_ARGS",     NINE_RETRY_NO);
return nine_fail_code("something specific",    "E_ODD",      NINE_RETRY_UNSET);
```

`NINE_RETRY_UNSET` is distinct from `NINE_RETRY_NO` for the same reason omitting
`retryable` is distinct from setting it false. `examples/tools/sha256.c` uses this for its
missing-argument path.

### The two host imports

A wasm tool is not import-free. It may import one module, `nine`, holding exactly two
functions — the same two the `js` kind's `console` and `fetch` are built on:

| Import | Capability | In `nine.h` |
|---|---|---|
| `nine.log(ptr, len)` | `log`, granted to everyone | `nine_log(msg)` |
| `nine.http(ptr, len) -> packed` | `net.http`, declared + granted | `nine_http(req, &len)` |

```c
uint32_t n;
const char *resp = nine_http("{\"url\":\"https://api.example/v1\",\"method\":\"GET\","
                             "\"headers\":{},\"body\":\"\"}", &n);
```

The request and response are the JSON shapes documented in `nine.h`. Every policy decision
— the method and host allowlists, SSRF rejection on the resolved address, per-redirect
revalidation, the response cap — is enforced on the host side of that call, so an ungranted
tool gets `{"error":"blocked: this tool was not granted the net.http capability"}` back
rather than a connection. The import exists either way, because a wasm module's imports are
fixed at compile time; the permission is checked per call.

**A response body is a JSON string, so a response that is not valid UTF-8 does not survive
it intact.** Binary bodies are a known gap for both tool kinds — see
`nine docs rich-js-tools` — not something to work around here.

`fs.read`, `fs.write`, and `env` need no import at all: they are WASI facilities, so
`fopen`, `readdir`, and `getenv` work directly against whatever the operator mounted or
named. Your code sees the guest path (`/data`), never the host path.

### A worked example

`examples/tools/sha256.*` is a complete one — SHA-256 in dependency-free C, written
against the header, in the files a wasm tool ships as:

```text
examples/tools/
  sha256.toml          the manifest — kind = "wasm"
  sha256.schema.json   { text: string }
  sha256.c             the source
  sha256.wasm          the artifact, 11 KiB, committed
```

`examples/tools/` is not a tool directory — nothing there is loaded. Copy what you want
into your own `[tools].user_dir`, which ships empty so that what runs in it is what you
chose.

Hashing is the honest demonstration of why this tier exists: it is exactly the work a
language model cannot do by reasoning about it, and the answer is checkable to the byte.

```console
$ nine "What is the SHA-256 hash of the exact string: hello nine"
The SHA-256 hash of "hello nine" is
50ce1f9527a47956e94d826d924578d9717c755b14a300ff85a517884d52d035

$ printf 'hello nine' | shasum -a 256
50ce1f9527a47956e94d826d924578d9717c755b14a300ff85a517884d52d035  -
```

`nine replay <agent-id> --turn 1` shows the model reaching for it rather than reciting it,
which is the part worth seeing:

```text
response: stop=tool_use
  → call sha256 {"text":"hello nine"}

tool sha256  (0ms, 1 attempt(s), ok)
  output: 50ce1f9527a47956e94d826d924578d9717c755b14a300ff85a517884d52d035
```

**The `.wasm` is committed, so copying the example needs no C toolchain.** Only editing the
C does, and `make tools-wasm` then rebuilds it with the SDK `make quickjs-wasm` fetches:

```console
$ make tools-wasm
```

The build line itself is no more than this, if you would rather not go through `make`:

```console
$ SDK=internal/toolvm/quickjs/.build/wasi-sdk-33
$ "$SDK/bin/clang" --target=wasm32-wasip1 --sysroot="$SDK/share/wasi-sysroot" \
    -mexec-model=reactor -Os -o examples/tools/sha256.wasm examples/tools/sha256.c \
    -Wl,--export=nine_alloc -Wl,--export=nine_run -Wl,--strip-all -Wl,--gc-sections
```

Two details in `sha256.c` generalize to any language. It reads its argument straight out of
the input JSON via `nine_arg_str` rather than linking a parser — which is what keeps a
raw-wasm tool a few KiB instead of a few hundred — and it never frees anything, because
there is no `free` in the ABI and the instance is destroyed when the call returns.

---

## What to expect at runtime

- **Every call is a fresh instance.** No global survives, no module-level cache works, and
  two calls cannot observe each other. Do not try to memoize across calls — write a pure
  function.
- **Five seconds, 16 MiB.** Both are operator-tunable (`[tools] timeout`, `memory_mb`).
  There is no CPU metering, so an infinite loop is killed by the wall clock, not by a work
  budget.
- **Large results are spilled**, not lost — a result over the per-call token cap is written
  to the file store and replaced with a short preview the model can read back by path.
- **A new tool is visible next turn.** Loops already in flight keep the tool set they
  started with.

---

## Checklist

- [ ] Manifest and entrypoint sit beside each other in `[tools].user_dir`.
- [ ] `description` reads well to someone who has never seen the tool.
- [ ] `input_schema` matches the arguments the code actually reads.
- [ ] No `import` left in the shipped file (bundle first).
- [ ] Declared capabilities are the minimum, and the operator has granted exactly them.
- [ ] `nine tool validate` passes.
- [ ] `nine tools` shows it as `ok` after a reload.
