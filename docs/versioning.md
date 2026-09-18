# Versioning

Nine has more than one thing that needs versioning, and they change at
different rates. A single number can't carry all of it, so Nine versions four
surfaces independently. Only the first is user-facing; the rest are internal
compatibility contracts that bump *only* when a real break happens.

| Surface | Protects | Scheme | Lives in |
|---|---|---|---|
| **Release version** | "which Nine is this" (user-facing) | SemVer, git-tag driven | git tag `vX.Y.Z` → injected at build |
| **Plugin protocol** | daemon ↔ native plugin wire compat | single integer, bump on break | `plugin.ProtocolVersion` |
| **Sandboxed tool ABI** | daemon ↔ wasm guest compat | single integer, bump on break | `toolvm.ABIVersion` |
| **Config schema** | `nine.toml` shape | not implemented — see [Limits](#limits) | — |
| **Memory DB schema** | SQLite schema | idempotent `CREATE TABLE IF NOT EXISTS` on open; no migration runner — see [Limits](#limits) | `internal/memory.initSchema` |

The release version also names the container image. A `v*` tag builds and
publishes `ghcr.io/djordlucas/nine` and `docker.io/djordlucas/nine` — see
[Container image](docker-image.md) for the tag scheme those versions map to.

## 1. Release version

The release version is the only number humans say out loud. It follows
[SemVer](https://semver.org/). Now that Nine is past `1.0`, standard SemVer
applies: **major** bumps on a breaking change (wire protocol, config shape, CLI
surface, or a public `spec/` contract), **minor** on a backward-compatible
feature, **patch** on a fix. Nine spent its `0.x` phase under the pre-1.0
convention — minor could break, patch was safe — to stay fast before the API and
protocol settled; `v1.0.0` is the point where they became stable enough to
commit to.

It is **not** hardcoded. `main` declares:

```go
var Version = "dev"
```

and the Makefile injects the real value at build time from git:

```makefile
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags "-X main.Version=$(VERSION)"
```

`git describe` resolves to:

- `v1.1.0` — on a tagged commit, clean tree
- `v1.1.0-3-gabc123` — 3 commits past the tag (self-identifying dev build)
- `v1.1.0-dirty` — tagged commit with uncommitted changes
- `dev` — plain `go build` / `go run`, or outside a git checkout

Check it with `nine version` (aliases: `--version`, `-v`).

### Cutting a release

```sh
git tag -a vX.Y.Z -m "Nine vX.Y.Z — <summary>"
make build          # binary now reports vX.Y.Z
git push origin vX.Y.Z
```

Nine's first published version was `v0.1.0`. `v1.0.0` marks the point where the
CLI, wire protocol, and public `spec/` contracts became stable enough to promise
compatibility within the `1.x` line — breaking any of them now requires a major
bump.

### Upgrade notes

**The Go built-in plugins moved into the `nine` binary.** `shell`, `files`,
`http`, and `time` are no longer separate executables under `[plugins].bin`; the
daemon starts each as a `nine plugin serve <name>` child process
(`spec/contracts/plugin.md` R-PLUG.13).

⚠️ **If you withheld a plugin by not shipping its binary, it now starts.** That
was never a documented control, but it worked: `TryStart` skips a plugin whose
binary is absent, so deleting `dist/bin/shell` — or shipping a `bin` directory
without it — kept `shell` out of the roster. A built-in has no binary to omit, so
after upgrading, **`shell` runs unless you say otherwise**, and `shell` executes
arbitrary commands.

Replace the old lever with the explicit one (R-PLUG.14):

```toml
[plugins]
disabled = ["shell"]
```

or `NINE_PLUGINS_DISABLED=shell` in a container. Confirm with `nine plugins`,
which lists a disabled plugin as `off`. A name that matches no plugin disables
nothing and is reported as a warning at boot — check for it, since a typo here
fails open.

## 2. Plugin protocol version

Native plugins are separate processes (one is Node), so the daemon and a plugin
can be built at different times and disagree on the wire contract. The protocol
version catches that at startup instead of letting it surface as a confusing
runtime failure.

`plugin.ProtocolVersion` is a single integer covering the native plugin wire
contract: the `plugin.describe` / `plugin.call` envelope over HTTP on a Unix
socket, its methods, and their semantics. **Bump it on any breaking change to
that contract.** It is independent of the release version — most releases will
not touch it.

How it works:

- A plugin advertises the version it was built against in its
  `plugin.describe` response (`DescribeResult.ProtocolVersion`). Plugins built
  with `plugin.Serve` stamp it automatically.
- On startup the daemon (`Manager.Start`) compares the advertised version with
  its own supported set and **rejects an unsupported version**, with a message
  telling the operator to rebuild the plugin (or upgrade Nine). A plugin reporting
  `0` predates protocol versioning and is treated as incompatible.

The current version is **2**, which added long-running jobs (the `async_jobs`
describe flag, a `job_id` on `plugin.call`, and the `plugin.job_status` /
`plugin.job_cancel` methods — see `spec/contracts/plugin.md` R-PLUG.12). Because
that is purely **additive** — a v1 plugin remains fully functional, just without
jobs — the check (`checkProtocolVersion`) accepts a **set**, `{1, 2}`, rather than
a single version, and treats a v1 plugin as lacking jobs. The `0` (predates
versioning) rejection stays.

This applies only to **native** Nine plugins. MCP plugins negotiate their own
protocol version (`mcpProtocolVersion`, see `mcp`) — a
separate, externally-defined contract.

## 2a. Sandboxed tool ABI version

Sandboxed tools (`spec/contracts/toolvm.md`) are wasm modules, not processes, but
they raise the same problem for the same reason: a `.wasm` file an operator drops
in `[tools].user_dir` was built at some other time, against some other version of
the guest contract.

`toolvm.ABIVersion` is a single integer covering that contract: the `nine_alloc` /
`nine_run` exports, their signatures, and the shape of the JSON crossing between
them. **Bump it on any breaking change.** The current version is **1**.

It is **independent of `plugin.ProtocolVersion`** and of the release version. The
two subsystems share a dispatcher and nothing else — a change to the plugin wire
envelope has no bearing on what a wasm guest exports, and conflating them would
force operators to rebuild one when the other moved.

How it works:

- A tool's manifest may state the ABI it was built against (`abi = 1`). An absent
  value is taken as the current version, since a `js` tool never builds anything
  and asking its author to think about the ABI would be noise.
- At load the host **refuses an unsupported version by name**, and `nine tools`
  reports it — rather than instantiating the module and letting the mismatch
  surface as a mystery trap on first call.

## Limits

Neither remaining versioning axis is implemented. Both are additive work that
must land before the change that needs them, not after.

| Axis | State today | What is needed |
|------|-------------|----------------|
| Config schema | `nine.toml` carries no `schema_version`. An upgraded daemon reading an older file relies on the config shape not having changed incompatibly. | A `schema_version` integer plus migrate-on-load, in place *before* the first incompatible config change. |
| Memory DB schema | `initSchema` applies the schema idempotently on `Open` (`CREATE TABLE IF NOT EXISTS`). Additive changes are safe. `PRAGMA user_version` records a generation, but there is no migration table and no migration runner. | A migrations table in `memory` with sequential numbers applied on open, before the first backward-incompatible schema change. |
