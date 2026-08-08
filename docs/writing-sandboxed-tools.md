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

| Capability | You get | Granted by default |
|---|---|---|
| `clock`, `random` | `Date.now()`, `Math.random()` | ✅ |
| `log` | `console.*` | ✅ |
| `fs.read` / `fs.write` | mounted directories | ❌ declare + grant |
| `env` | named keys only | ❌ declare + grant |
| `net.http` | `fetch()` | ❌ declare + grant |

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
`headers`, `text()`, and `json()`. There is no streaming, no `AbortController`, no cookie
jar, and no `Request`/`Headers`/`Response` classes.

Two behaviors differ from browser `fetch` and are worth knowing:

- **A blocked request throws**, it does not return a non-ok response. A refusal is not a
  response, and letting it look like one invites `if (res.ok)` to quietly swallow a
  decision the operator made. The message says why.
- **Redirects are followed but re-checked.** Every hop must independently satisfy the
  allowlist, and `Authorization`/`Cookie` are stripped when a hop crosses origins.

What you cannot reach, regardless of `allow_hosts`: loopback, link-local (including
`169.254.169.254`, the cloud instance-metadata endpoint), RFC 1918, and the other
non-routable ranges. The check is on the address actually dialed, so pointing a permitted
hostname at one of them does not help. That is deliberate and not configurable.

You also cannot set `Host`, `Content-Length`, or hop-by-hop headers, and only `http` and
`https` are permitted.

---

## What JavaScript you get

QuickJS-NG — **ES2023, and nothing else**. There is no Node standard library: no `fs`,
`http`, `path`, `Buffer`, `process`, or `crypto`, and no `require`. There is also no
`fetch`, `setTimeout`, or `URL`.

Everything you would reach for from ES2023 itself is there: `JSON`, `Map`/`Set`,
`Intl`-free `Date`, regular expressions, generators, `async`/`await`, optional chaining,
`Array.prototype.at`, and so on.

### Dependencies: bundle them yourself

**For a developer tool, Nine never resolves a dependency** — that is your job, at build
time. A `.js` file that still contains an `import` when Nine loads it will fail at call
time. (Generated tools are the exception, and get their own resolver — see below.)

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

A generated tool may always import a small, curated set of pure-JavaScript modules — no
config, no bundling, no network:

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
write JSON out. No runtime, no imports, no capabilities you did not declare.

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
