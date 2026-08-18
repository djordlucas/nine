# Writing a sandboxed tool

A **sandboxed tool** lets you add a permanent tool to Nine without writing a plugin,
building a binary, or rebuilding the image. You drop two files in a directory; the daemon
runs your **JavaScript** in a wasm sandbox with exactly the capabilities the operator
granted it.

JavaScript is the supported language, and there is no build step. A `.wasm` module built
from another language also runs, on a specified but unsupported contract — see
[the `wasm` kind](#the-wasm-kind--unsupported-but-specified).

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

### Returning bytes

A tool can produce something that is not text — a rendered image, a compressed archive.
Return it and say what it is:

```js
export default () => ({ bytes: png, mediaType: "image/png" });   // png is a Uint8Array
```

A bare `Uint8Array`, `ArrayBuffer`, or typed-array view works too; the wrapper only adds the
label.

**The model never sees the bytes.** It cannot read them, and inlining base64 would blow the
output budget while teaching it nothing. They are written to the memory file store and the
model is handed a path plus a description:

```text
[nine: this tool returned 67 bytes of image/png, which is not text and is not shown here.
The bytes are saved in the memory file store, base64-encoded, at this path:
    spill/<session>/make_icon-ee1a9e04.txt
...]
```

It can pass that path to another tool's `*_ref` argument to hand over the whole content, or
read the base64 with `file_fetch` if it genuinely needs the encoding.

They are stored base64-encoded because the file store is a text column that replaces NUL
bytes with U+FFFD — raw bytes would not survive it, and base64 survives exactly. Returning
bytes needs **no `fs.write` grant**: the store is Nine's, not the operator's filesystem.

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

| Capability | How you reach it | Granted by default |
|---|---|---|
| `clock`, `random` | `Date.now()`, `crypto.getRandomValues()` | ✅ |
| `log` | `console.*` | ✅ |
| `net.http` | `fetch()` | ❌ declare + grant |
| `fs.read` / `fs.write` | `import … from "nine:fs"` | ❌ declare + grant |
| `env` | `import { get } from "nine:env"` | ❌ declare + grant |

### The filesystem, from JavaScript

```js
import { readFileText, writeFile, readDir, stat, mounts } from "nine:fs";

export default ({ name }) => {
  const rows = readFileText(`/data/${name}`);      // the GUEST path
  writeFile("/out/summary.txt", `${rows.length} bytes`);
  return { files: readDir("/data"), granted: mounts() };
};
```

You address the **guest** path (`/data`), never the host path — which is what lets the
operator move or narrow the mount without your tool changing, and `mounts()` tells you what
you actually got rather than leaving you to hardcode a guess.

`readFile` returns a `Uint8Array` and `readFileText` decodes UTF-8. Bytes are the default
deliberately: a tool reading a PNG should not have to discover that its data was mangled on
the way in.

**Nothing here is what confines you.** The mount is a wazero pre-open, so a tool scoped to
`/data` cannot climb out of it — `..`, an absolute path, and a symlink all fail — without
Nine writing a single check. The capability checks in `nine:fs` exist only so that an
ungranted call says `fs.read is not granted to this tool` instead of reporting that a file
which plainly exists cannot be found.

### The environment

```js
import { get, keys } from "nine:env";
export default () => ({ tz: get("TZ"), granted: keys() });
```

Only the keys the operator named are passed into the instance at all, so an ungranted key
is not hidden but absent; asking for one throws and names what *was* granted. `NINE_*` and
`*_API_KEY` can never be granted.

### Randomness

`crypto.getRandomValues()` and `crypto.randomUUID()` are real CSPRNG output — WASI's
`random_get`, which the daemon feeds from Go's `crypto/rand` — rather than `Math.random`
with a better name. There is no `crypto.subtle`: it is a large asynchronous surface, and a
tool needing AES-GCM can bundle an implementation.

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

(At the ABI level this is `body_b64` **instead of** `body`, in both directions.)

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
`crypto` is `getRandomValues`/`randomUUID` only, and there is no `Intl` — see below.

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
import { readFileText, writeFile } from "nine:fs";    // needs the fs grant
import { get } from "nine:env";                        // needs the env grant
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

## The `wasm` kind — unsupported, but specified

**JavaScript is the supported language.** Everything above works with no build step, and
since `nine:fs`, `nine:env`, and `crypto` landed there is no capability a `js` tool cannot
reach.

A tool may still be a `.wasm` module you built yourself, from Rust, TinyGo, Zig, or C.
Nine ships no header, no example, and no build tooling for that path — **you are on your
own**, deliberately: maintaining a second language's ergonomics for a case few tools need
is not a good trade. What Nine does provide is a specified contract that will not move
under you (`nine spec toolvm`, R-TVM.3).

The whole of it is two exports and a JSON envelope:

```text
nine_alloc(size i32) -> i32          reserve size bytes; return the offset
nine_run(ptr i32, len i32) -> i64    run; return (offset << 32) | length
```

`nine_run` receives its arguments as UTF-8 JSON and returns UTF-8 JSON:

```json
{"ok": true,  "output": "..."}
{"ok": true,  "output_b64": "...", "media_type": "image/png"}
{"ok": false, "error": "...", "error_detail": {"code": "E_ARGS", "retryable": false}}
```

There is no `free` — the instance is destroyed when the call returns. The host writes one
byte more than the input and NULs it, so the input is both length-delimited and
NUL-terminated. A module may import `nine.log`, `nine.http`, and `nine.caps`; the
filesystem and environment arrive through WASI, so libc's `fopen` and `getenv` work
directly against whatever the operator mounted.

### When it is worth it

One reason, and it is a real one: **CPU-bound work**. Measured on one host, hashing 64 KiB
with the same algorithm either side:

| | per call |
|---|---|
| `wasm` | 3.4 ms |
| `js` | 706 ms |

A `js` tool also pays ~5.7 ms of fixed overhead per call, since each call instantiates a
fresh 1 MB interpreter — irrelevant against a model turn that takes seconds. The ~200×
compute gap is not irrelevant: a JS tool hashing a megabyte would exhaust the five-second
deadline. If your tool does that kind of work, build a `.wasm`. Otherwise write JavaScript.

---

## A worked example

`examples/tools/linkcheck.*` is a complete tool that uses most of what is described above:

```text
examples/tools/
  linkcheck.toml         the manifest, declaring fs.read and net.http
  linkcheck.schema.json  its arguments
  linkcheck.js           the code
```

It reads a list of URLs from a mounted file, fetches each concurrently, and reports what
came back — so it exercises `nine:fs`, `fetch`, `URL`, `Promise.all`, and both kinds of
failure: a bad argument thrown as `E_ARGS` with `retryable: false`, and an all-hosts-down
result thrown as `E_UPSTREAM` with `retryable: true`.

`examples/tools/csvstats.*` is the smaller one — a pure transform, no capabilities, the
shape most tools should be.

Copy either into your `[tools].user_dir`; nothing in `examples/` is loaded.

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
