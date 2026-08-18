# Worked sandboxed-tool examples

Two complete tools. **Nothing here is loaded** — this directory is not a
`[tools].user_dir` and Nine never scans it. Copy what you want into your own
`tools.d/`, which ships empty precisely so that what runs there is what you
chose.

```text
csvstats.toml          the manifest
csvstats.schema.json   { csv: string }
csvstats.js            the code

linkcheck.toml         the manifest — declares fs.read and net.http
linkcheck.schema.json  { urls: string[] } or { file: string }
linkcheck.js           the code
```

Both are `kind = "js"`, which is the supported language: no build step, no
toolchain, and since `nine:fs`, `nine:env`, and `crypto` landed there is no
capability a JavaScript tool cannot reach.

## `csv_stats` — the shape most tools should be

A pure transform over its arguments, declaring **no capabilities**. It runs
anywhere, needs nothing from the operator, and cannot do anything surprising.
Reach for this shape first.

```console
$ cp examples/tools/csvstats.* tools.d/
$ nine tool validate ./tools.d
  ok    csv_stats          js     declares: none
```

## `link_check` — the one that needs reach

Reads a list of URLs from a mounted file, fetches each concurrently, and reports
what came back. It is the example to read when you need the rest of the surface:

- **`nine:fs`** — `readFileText` and `exists`, addressing the *guest* path `/data`
- **`fetch`** — the subset, with a blocked request throwing rather than returning
  a non-ok response
- **`URL`** — validating and normalizing before the request
- **`Promise.all`** — concurrent, because the deadline is wall-clock
- **structured errors, both directions** — a bad argument as `E_ARGS` with
  `retryable: false`, which takes the call out of Nine's retry loop; every host
  being unreachable as `E_UPSTREAM` with `retryable: true`, which is the opposite
  instruction

It declares `fs.read` and `net.http`, so it **will not load until the operator
grants both** — that asymmetry is the point, and `nine tools` names whichever is
missing:

```toml
# in the operator's nine.toml
[tool.link_check.capabilities]
[tool.link_check.capabilities.fs]
read = [{ host = "/srv/urls", guest = "/data" }]
[tool.link_check.capabilities.net.http]
allow_hosts = ["example.com", "*.example.org"]
methods     = ["GET"]
```

```console
$ cp examples/tools/linkcheck.* tools.d/
$ nine tool validate ./tools.d
  ok    link_check         js     declares: fs.read, net.http (must be granted in nine.toml before this tool will load)
```

---

Guide: `nine docs writing-sandboxed-tools`. Design: `nine docs sandboxed-tools`.
Contract: `nine spec toolvm`.
