# Your sandboxed tools

Drop your own tools here. Nine discovers this directory at boot and runs them in
a **wasm sandbox**, alongside the tools built into the binary and the ones
plugins provide. Built-ins and plugins are unaffected — this is purely additive.

**This directory ships empty, and that is deliberate.** Anything in it becomes a
tool your model can call, so what lives here should be what *you* put here. The
worked examples are in [`examples/tools/`](../examples/tools/) — copy one in when
you want it.

```
tools.d/
  csvstats.toml    the manifest (the gate)
  csvstats.js      the code
  mytool.toml
  mytool.wasm
```

A `.js` or `.wasm` file with **no manifest beside it is never loaded**.

Unlike a plugin, a sandboxed tool is **not a subprocess and not a binary you
build**. It is a file the daemon runs in-process, inside a sandbox, with exactly
the capabilities the operator granted it — by default, **none**.

## Turning it on

Off unless you say otherwise. In `nine.toml`:

```toml
[tools]
enabled  = true
user_dir = "./tools.d"
```

Under Docker this path is wired as `/tools.d` (`NINE_TOOLS_USER_DIR`) but **not
mounted** — add `-v /my/tools.d:/tools.d:ro` to get your own tools in, so the
container runs the tools you chose rather than whatever the repo carried.

This directory is scanned at boot; re-scan a running daemon with `nine tools
reload`. An absent or empty directory just means "no sandboxed tools".

## Seeing what happened

```console
$ nine tools
  ok    csv_stats          js     none
  ok    weather            js     net.http GET api.weather.example
  SKIP  scraper            js     capability fs.read is declared by the tool but not granted
```

A tool that fails to load is **always reported with its reason** — that is the
whole point of the surface. A capability mismatch is deliberately a load failure
rather than a tool that half-works in ways neither you nor the operator
predicted.

Check a tool before you deploy it, with no daemon running:

```console
$ nine tool validate ./tools.d
  ok    csv_stats          js     declares: none
```

## Writing one

```js
// csvstats.js
export default ({ csv }) => ({ rows: csv.trim().split("\n").length });
```

```toml
# csvstats.toml
name        = "csv_stats"
kind        = "js"
entrypoint  = "./csvstats.js"
description = "Summary statistics over a CSV string."
```

Default-export a function. Return a string (passed through untouched) or any
value (JSON-stringified). `async` works. A thrown error reaches the model as an
ordinary tool failure carrying your message.

`examples/tools/` holds two complete examples — `csvstats` (no capabilities)
and `linkcheck` (fs.read + net.http). Copy one.

## What you get, and what you don't

The runtime is **QuickJS-NG plus a small platform layer**: `console`, `fetch`,
`TextEncoder`/`TextDecoder`, `URL`/`URLSearchParams`, `structuredClone`, and
timers that run in *virtual time* (they order correctly but never sleep). There
is no Node standard library — no `fs`, `http`, `path`, `Buffer`, `process`, or
`require` — and no `crypto` or `Intl`. Passing a locale to `toLocaleString`
throws rather than silently ignoring it. Full list: `nine docs
writing-sandboxed-tools`.

**Bundle your third-party dependencies yourself**, at development time (the
embedded `nine:csv`, `nine:date`, and `nine:diff` are importable as-is):

```console
$ npx esbuild tool.js --bundle --format=esm --platform=neutral --outfile=csvstats.js
```

Nine never resolves a dependency: no package manager, no lockfile, no network at
load time. `import` resolves against a closed allowlist which, for tools in this
directory, is empty — so a tool that still contains an `import` will fail.

**JavaScript is the supported language.** A `.wasm` module built from Rust,
TinyGo, Zig, or C also runs (`kind = "wasm"`) and is worth it for CPU-bound work
— roughly 200× on a hashing benchmark — but Nine ships no header, example, or
build tooling for it, and you are on your own. The contract is specified and
stable: `nine spec toolvm`, R-TVM.3.

## Capabilities

The default is **nothing**: no filesystem, no network, no environment. Most good
tools need nothing — a pure transform over its arguments runs anywhere.

If you do need reach, *declare* it in your manifest:

```toml
[capabilities]
fs = ["read"]
```

…and the **operator grants it**, by name, in their `nine.toml`:

```toml
[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]
```

Your code sees the guest path (`/data`). Declaring something ungranted fails to
load — and so does being granted something you did not declare. A manifest never
grants anything; only the operator's config does.

`clock`, `randomness`, and logging are always available. They leak nothing.

From JavaScript, reach a granted capability through the embedded modules:
`import { readFileText } from "nine:fs"` and `import { get } from "nine:env"`.
You address the guest path (`/data`), and confinement is the wazero pre-open
rather than anything in those modules. From C, `fopen` and `getenv` work
directly.

For network access, declare `net = ["http"]` and have the operator grant the hosts; then
`fetch` works. It is a subset — `status`, `ok`, `headers`, `text()`, `json()` — and a
blocked request throws rather than returning a non-ok response. Loopback, link-local
(`169.254.169.254`, where clouds serve instance credentials), and private addresses are
refused whatever `allow_hosts` says and whatever a hostname resolves to.

## Limits

Every call gets a **fresh instance** — no global survives, and two calls cannot
observe each other, so write a pure function rather than trying to cache. Calls
are bounded at **5 seconds** and **16 MiB** by default (`[tools] timeout`,
`memory_mb`).

A newly-added tool is picked up by the next turn; turns already in flight keep
the tool set they started with.

---

Full guide: `nine docs writing-sandboxed-tools`. Design rationale:
`nine docs sandboxed-tools`. Contract: `nine spec toolvm`.
