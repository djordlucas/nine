# Your sandboxed tools

Drop your own tools here. Nine discovers this directory at boot and runs them in
a **wasm sandbox**, alongside the tools built into the binary and the ones
plugins provide. Built-ins and plugins are unaffected — this is purely additive.

```
tools.d/
  csvstats.toml    the manifest (the gate)
  csvstats.js      the code
  sha256.toml
  sha256.wasm
  nine.h           the C ABI header, for wasm tools
```

Both of those ship here as working examples: `csvstats` is the `js` kind, `sha256`
the `wasm` kind.

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

Under Docker this directory is mounted at `/tools.d` (see the Makefile's
`up`/`up-hot` targets), but you still need `enabled = true` in the `nine.toml`
you mount — whether the subsystem runs at all is a deliberate decision, not
something an environment variable should flip on.

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

`csvstats.*` in this directory is a complete working example — copy it.

## What you get, and what you don't

The runtime is **QuickJS-NG: ES2023 and nothing else**. There is no Node standard
library — no `fs`, `http`, `path`, `Buffer`, `process`, `crypto` — and no
`fetch`, `setTimeout`, or `require`.

**Bundle your dependencies yourself**, at development time:

```console
$ npx esbuild tool.js --bundle --format=esm --platform=neutral --outfile=csvstats.js
```

Nine never resolves a dependency: no package manager, no lockfile, no network at
load time. `import` resolves against a closed allowlist which, for tools in this
directory, is empty — so a tool that still contains an `import` will fail.

You can also ship a `.wasm` built from Rust, TinyGo, Zig, or C (`kind = "wasm"`),
which links no interpreter and gets full speed. `sha256.*` here is a worked
example in C — hashing being precisely what a language model cannot do by
reasoning about it. The `.wasm` is committed, so copying it needs no C
toolchain; `make tools-wasm` rebuilds it if you edit `sha256.c`.

In C, include **`nine.h`**: it is the ABI as a header — the two exports, the
`(offset << 32) | length` packing, the `nine.log` and `nine.http` host imports,
and envelope builders that escape your output so a stray quote cannot corrupt
the JSON the host is about to parse.

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

**`fs` and `env` do not work from a `js` tool yet.** They are WASI facilities a
`wasm` tool reaches through `fopen` and `getenv`, and the interpreter has no
binding for them — so a `js` tool declaring either loads, reports the
capability, and finds no API to use it. Write it as `kind = "wasm"` until that
closes (`nine docs rich-js-tools`).

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
