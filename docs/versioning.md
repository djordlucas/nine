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
| **Config schema** | `nine.toml` shape | integer `schema_version` field (planned) | config struct + migrate-on-load |
| **Memory DB schema** | SQLite schema | idempotent `CREATE TABLE IF NOT EXISTS` on open; `PRAGMA user_version` records a generation, but there is no migration runner yet (planned) | `internal/memory.initSchema` |

## 1. Release version

The release version is the only number humans say out loud. It follows
[SemVer](https://semver.org/). Now that Nine is past `1.0`, standard SemVer
applies: **major** bumps on a breaking change (wire protocol, config shape, CLI
surface, or a public `spec/` contract), **minor** on a backward-compatible
feature, **patch** on a fix. Nine spent its `0.x` phase under the pre-1.0
convention — minor could break, patch was safe — to stay fast before the API and
protocol settled; `v1.0.0` is the point where they became stable enough to
commit to.

It is **not** hardcoded. `cmd/nine/main.go` declares:

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
protocol version (`mcpProtocolVersion`, see `internal/plugin/mcp.go`) and the
Anthropic LLM client pins its own API version (`anthropic-version`) — those are
separate, externally-defined contracts.

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

## 3. Config schema version (planned)

When `nine.toml` first changes shape incompatibly, add a `schema_version`
integer to the config and migrate-on-load so an upgraded daemon can read an
older user's file instead of crashing. Not yet implemented — no incompatible
config change has landed yet, but now that Nine is past `1.0` the migration path
must be in place *before* the first one does.

## 4. Memory DB schema (planned)

The SQLite schema is currently applied idempotently on `Open` via
`initSchema` (`CREATE TABLE IF NOT EXISTS`, plus `CREATE EXTENSION IF NOT EXISTS
vector`) — additive changes are safe, but there is **no migration table or
version counter**. When the schema first needs a backward-incompatible change,
add a migrations table to `internal/memory` with sequential migration numbers
applied on open. Not yet implemented — same reasoning as the config schema.
