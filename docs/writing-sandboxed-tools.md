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
read the base64 with `read_file` if it genuinely needs the encoding.

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

**If your tool fails, the last 8 lines it printed come back with the error.** That is what
`console.log` is for here: print the values you would want to see if this call went wrong,
because on a failure they are what the caller reads instead of guessing at your code. A
successful call returns its result and nothing else.

**Do not print credentials.** A failure message is read by the model and written to the
turn, so anything you print on the way to failing goes with it.

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
| `state` | `import { get, set } from "nine:state"` | ❌ declare + grant |
| long-running | `import { again } from "nine:job"` | ❌ `resumable = true` in the manifest |

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

**Files larger than your memory.** A call is capped at 16 MiB of linear memory, and
workspaces hold files bigger than that, so `readFile` is the wrong tool past a few
megabytes:

| Call | Use it for |
|---|---|
| `readRange(path, offset, length)` | A window of a large file, as bytes. `readRangeText` decodes it |
| `appendFile(path, data)` | Adding to the end without reading what is already there |
| `rename(from, to)` | Relocating a file, or replacing one atomically: write a temporary, then rename over the target |
| `remove(path)` | One file, or one empty directory. Never recursive |
| `copyFile(from, to)` | A copy that streams through a fixed buffer rather than going resident |

A paging loop over `readRange` holds one window whatever the file's size, which is how
`edit_file` changes a line in a 200 MB log.

`nine:diff` renders a change for a person to read: `lineDiff` and `unified` compare two
strings line by line, and `hunks(a, b, { context, maxLines })` returns only the changed
regions with surrounding context, plus the added and removed counts. Prefer `hunks` for
anything a human sees — `unified` emits every line of both inputs, which is unreadable for
one line changed in a file of five thousand.

**Nothing here is what confines you.** The mount is a wazero pre-open, so a tool scoped to
`/data` cannot climb out of it — `..`, an absolute path, and a symlink all fail — without
Nine writing a single check. The capability checks in `nine:fs` exist only so that an
ungranted call says `fs.read is not granted to this tool` instead of reporting that a file
which plainly exists cannot be found.

### Remembering between calls

Your tool is built from scratch for every call and thrown away after it. Nothing in the
interpreter survives — not a global, not a cached credential, not a parsed index — and
that is deliberate: two calls cannot observe each other through the machine.

The `state` capability is the one way past it, and it is not a hole in that. What you get
is a store the **host** owns: keys scoped to your tool, bounded by a quota, readable by
nothing else.

```toml
# your manifest — a need, with no parameters attached
[capabilities]
state = true
```

```toml
# the operator's nine.toml — the parameters are theirs
[tool.geocode.capabilities.state]
scope        = "tool"     # required: "tool" or "conversation"
max_keys     = 512
max_value_kb = 8
ttl          = "24h"      # optional; omit for no expiry
```

```js
import { get, set, remove, keys, swap, getJSON, setJSON } from "nine:state";

export default function ({ place }) {
  const hit = getJSON(`geo:${place}`);
  if (hit) return `${hit.lat},${hit.lon} (cached)`;
  const fresh = lookUp(place);
  setJSON(`geo:${place}`, fresh);
  return `${fresh.lat},${fresh.lon}`;
}
```

**Values are strings.** `getJSON` / `setJSON` are the convenience over
`JSON.parse` / `JSON.stringify`; there is no other type.

**Scope is the operator's decision and you cannot change it.** `scope = "tool"` gives one
namespace shared by every caller; `scope = "conversation"` gives a separate namespace per
conversation. A cache is correct under either, but "remember this user's preference"
means something different under each — so if it matters to your tool, read it back:

```js
import { scope } from "nine:state";
const shared = scope() === "tool";
```

**Use `swap` instead of read-modify-write.** Two turns calling your tool at once is
ordinary, so `get` then `set` is a lost update waiting to happen: both calls read the same
value and the second overwrites the first. `swap` is the only operation that closes that
window.

```js
let ok = false;
while (!ok) {
  const cur = get("count");
  ok = swap("count", cur, String(Number(cur ?? 0) + 1));
}
```

**Hitting your quota is a normal thing to handle.** It throws with `code` set to
`E_STATE_QUOTA` and `retryable: true`, which is deliberately distinguishable from the
store being unreachable — evict something and try again.

```js
try {
  set(key, value);
} catch (e) {
  if (e.code !== "E_STATE_QUOTA") throw e;
  for (const k of keys("cache:").slice(0, 10)) remove(k);
  set(key, value);
}
```

### Work too long for one call

Every call runs under a wall-clock deadline — five seconds by default. If your tool needs
longer, it does **not** get a longer call. It does a bounded slice, hands back a cursor,
and gets called again.

```toml
# your manifest
resumable = true
```

```js
import { again } from "nine:job";

export default function (args, job) {
  const at = Number(job?.cursor ?? 0);
  const end = Math.min(at + 100, args.total);
  processRows(at, end);
  if (end >= args.total) return `done: ${args.total} rows`;
  return again({
    cursor: String(end),
    progress: `${end}/${args.total}`,
    afterMs: 1000,
  });
}
```

The tool runs as a **background job**. The turn that started it gets a handle straight
away; the result arrives on a later turn, and the model can follow up with `job_check`,
`job_wait`, `job_list`, or `job_cancel` — the same four tools long-running plugin work
already uses. Nothing about that surface is tool-specific.

Three rules that are not optional:

- **Bounded work per call.** Each call is an ordinary call under the ordinary deadline.
  `again()` is how you get more time; blocking is how you get killed.
- **Everything you need to resume goes in the cursor** (or in `nine:state`). The instance
  does not survive, so a variable you set will not be there. `args` is handed back
  unchanged every call — only the cursor moves.
- **`resumable = true` in the manifest, or the envelope is refused.** A tool cannot acquire
  a lifecycle by returning a field.

`afterMs` is a request. The host floors it (`[tools] job_min_delay_ms`, default 250ms) and
rounds up to its sweep, so 0 does not mean "spin".

A job is bounded in total, too: `[tools] job_max_calls` (default 720) and `[plugins]
job_max_seconds` (default 1h). A tool that always returns `again()` eventually fails,
naming the bound. There are caps on how many jobs can be outstanding at once as well —
per conversation and daemon-wide — so starting one can be refused with a message saying so.

**Your tool is never called twice at once for the same job.** Distinct jobs may run in
parallel, so a tool must still be safe to run concurrently *with itself on different
jobs* — which is the same requirement any tool already has, since two turns can call one
tool at once. What you are guaranteed is that call N+1 of a given job never starts before
call N has returned, so its cursor is always the one you last handed back.

**Two things a tool job does that a plugin job cannot.** It **resumes after a daemon
restart** — its whole live state is the cursor in the registry, so there is nothing to
lose. And **cancelling is exact**: the daemon simply does not make the next call.

### Running indefinitely

The same `resumable` tool an operator can start as a job, they can also declare **standing**
— run on its own cadence, forever:

```toml
[[standing_tool]]
id       = "corpus"
tool     = "corpus_index"
interval = "10s"
args     = { root = "/srv/corpus" }
```

Nothing about your code changes. One difference in meaning: **returning a result ends a
cycle, not the run.** Your cursor resets and the interval decides when the next pass
starts, so a standing tool is `again()` within a pass and a plain return between them.

Two things to write for:

- **Return nothing when there is nothing to say.** An empty result is silent; a non-empty
  one posts to the human feed. A watcher that announces every pass is a notification storm.
- **Use `nine:state` for what must outlive a cycle.** The cursor resets between passes;
  state does not. "What have I already seen" belongs in state, "where am I in this pass"
  belongs in the cursor.

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
| `console.*` | Goes to the daemon log; the last 8 lines also come back attached to a failure. Do not print credentials. |
| `fetch` | A subset, with the `net.http` grant. See above. |
| `TextEncoder` / `TextDecoder` | **UTF-8 only.** Another label throws rather than quietly producing UTF-8. |
| `URL` / `URLSearchParams` | A pragmatic subset — absolute URLs and resolution against a base. No IDNA, no full WHATWG state machine. |
| `structuredClone` | Handles cycles, `Date`, `Map`/`Set`, `RegExp`, TypedArrays. |
| `setTimeout` / `setInterval` / `clear*` | **Virtual time** — see below. |
| `atob` / `btoa`, `performance`, `queueMicrotask` | As you expect. `btoa` is Latin-1, so base64 UTF-8 via `TextEncoder`. |

**No Node standard library**: no `fs`, `http`, `path`, `Buffer`, `process`, or `require`.
`crypto` is `getRandomValues`/`randomUUID` only, and there is no `Intl` — see below.

### Timers do not sleep

A tool runs inside one turn under a wall-clock deadline, so it must never sleep. Timers therefore run in **virtual time**: the queue
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
- **Five seconds, 16 MiB, 50M operations.** A `js` tool is bounded by elapsed time, memory,
  and work done — the last of those counts loop iterations and calls, is enforced by an
  uncatchable interrupt, and cannot be caught. All three are operator-tunable
  (`[tools] timeout`, `memory_mb`, `max_ops`), and the deadline and the budget can both be
  set for one tool alone:

  ```toml
  [tool.slow_report]
  timeout = "30s"       # this tool only; everything else keeps [tools] timeout
  max_ops = 500000000   # likewise for the work budget
  ```

  Worth asking for if your tool legitimately needs it, and worth *not* asking for otherwise:
  a global value has to accommodate the most demanding tool, and naming yours is what keeps
  that allowance from applying to everything. An outbound
  HTTP request is bounded at four fifths of whatever the call has left, so raising the
  deadline raises that too.
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

---

## Limits

| Limit | Detail |
|-------|--------|
| The `wasm` kind is specified, not supported | The ABI is defined and documented, but raw `.wasm` tools are not a supported authoring path today. Write JS. |
| Nothing survives a call | A module is instantiated fresh per call and torn down after it. No globals, no cached credentials, no parsed index. Persist through a granted `fs` path, or wait for durable state. |
| Bundle your own dependencies | The shipped file must contain no `import`. External npm dependencies are a separate, off-by-default tier. |
| A trimmed JS surface | QuickJS is deliberately narrowed. Globals a Node or browser author expects are absent, and the import surface is closed — see *What JavaScript you get*. |
| Wall-clock deadline only | There is no CPU or memory metering; wazero has no fuel. A tool is bounded by its timeout alone. |
| Adding a tool needs a reload | Tools are discovered from `[tools].user_dir`. Run `nine plugins reload`, or restart the daemon. |
| Capabilities are the operator's | Declaring a capability in the manifest does not grant it. The operator grants it by name in `nine.toml`, and a declaration with no matching grant fails the load. |
