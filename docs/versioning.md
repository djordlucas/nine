# Versioning

Nine has more than one thing that needs versioning, and they change at
different rates. A single number can't carry all of it, so Nine versions five
surfaces independently. Only the first is
user-facing; the rest are internal compatibility contracts that bump *only* when
a real break happens.

| Surface | Protects | Scheme | Lives in |
|---|---|---|---|
| **Release version** | "which Nine is this" (user-facing) | SemVer, git-tag driven | git tag `vX.Y.Z` → injected at build |
| **Plugin protocol** | daemon ↔ native plugin wire compat | single integer, bump on break | `plugin.ProtocolVersion` |
| **Sandboxed tool ABI** | daemon ↔ wasm guest compat | single integer, bump on break | `toolvm.ABIVersion` |
| **Memory DB schema** | SQLite schema | sequential forward migrations, applied on open | `PRAGMA user_version`, currently **12** |
| **Config schema** | `nine.toml` shape | single integer, bump on break | `schema_version` in `nine.toml`, currently **1** |

The release version also names the container image. A `v*` tag builds and
publishes `ghcr.io/djordlucas/nine` — see [Container image](docker-image.md) for
the tag scheme those versions map to.

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

## 3. Memory DB schema version

The store carries its version in `PRAGMA user_version`, and a migration runner
brings an older database forward on `Open`. The current version is **11**.

The version is **derived from the step list**, not declared beside it: step *i*
moves a database from version *i* to *i+1*, so the version is how many steps
exist. A hand-maintained constant would be a second source of truth for the same
fact, and the failure when the two disagree is silent — a step that never runs, or
an index past the end of the list. Appending a step is the only way to bump the
version.

Four rules hold for a step:

- **Forward only.** There is no down-step. A rollback that has to reverse a
  destructive change cannot restore the data the change dropped, so the honest
  recovery from a bad migration is to restore the file and run a corrected forward
  step. Every step runs in a transaction, which is what makes that recovery clean.
- **Append only.** Renumbering an existing entry silently re-runs or skips it
  against databases already in the field.
- **Safe at its "from" version, including against a database an earlier binary
  patched ad hoc** — several of these steps were unconditional statements on every
  `Open` before the runner existed. A step need *not* be safe against a fresh
  database: `initSchema` creates the current shape and stamps it at the current
  version without running any step, so a step like a column rename would fail
  there.
- **All work through the handle it is given.** The writer pool holds exactly one
  connection and the running transaction owns it, so reaching for the enclosing
  database instead deadlocks.

What the eleven steps have done: added a column `CREATE TABLE IF NOT EXISTS` could
not add to an existing table, dropped two tables whose shape was wrong
(`reflections` had no agent id), renamed `stages` to `aspects` and then to
`routines`, renamed `plugin_jobs` to `jobs`, and added the standing-tool, queued-
message and workspace-index columns.

## 4. Config schema version

`nine.toml` carries an optional top-level `schema_version` naming the **shape
of the file** — not the version of Nine that wrote it.

```toml
schema_version = 1
```

| Value | Behavior |
|---|---|
| absent | Treated as 1. Every file written before the field existed is a schema-1 file, so omitting it stays correct rather than becoming an error on every existing deployment. |
| ≤ current | Loaded, migrating forward through each intervening step. |
| > current | **Refused**, and the daemon does not start. |

The refusal is a hard stop rather than a fallthrough, and that distinction is
the whole reason the field is useful. Nine tries four config paths in order
(`docs/configuration.md`), and every other load failure logs a warning and
moves to the next one — ending, if nothing loads, on an empty config. A newer
config treated that way would boot the daemon on defaults: no workspace, no
model override, no API auth token, and a clean-looking startup line. Refusing
outright is the only outcome that does not turn a version check into an
outage you cannot see.

Migration is **in-memory**: an older shape is read into the current struct.
`nine.toml` is never rewritten, so the operator's file stays the operator's —
the same rule that forbids a runtime config-rewrite tool.

### Unrecognised keys

A key the file carries that nothing reads is logged at load:

```
WARN config keys were not recognised and had no effect; check for a typo
     path=nine.toml keys="llm.provdier"
```

The decoder always knew; the answer was previously discarded. A typo now
announces itself instead of silently leaving a setting at its default.

---

## Limits

| Limit | Detail |
|-------|--------|
| No config migrations written yet | The mechanism is in place and the list is empty, which is the point: it landed *before* the first incompatible change rather than after, when it could no longer tell an old file from a new one. |
| Unknown config keys are reported, not refused | A key nothing reads is logged as a warning at load. It does not fail the boot, because a key from a newer Nine is a reason to tell the operator rather than to refuse an otherwise usable file. |
| No down-migrations | A newer binary migrates a database forward; an older binary against a migrated database is not supported and is not detected. Restoring the file is the only path back. |
