# Nine — working agreement

## Branching & pull requests

**Every change goes on a feature branch with a pull request. Never commit
directly to `main`.**

1. **Branch first:** `git checkout -b <type>/<slug>` — `fix/…`, `feat/…`,
   `chore/…`, `docs/…`. (`git checkout -b` carries any uncommitted work over, so
   it is safe to branch after you have started editing.)
2. **Commit on the branch**, not on `main`.
3. **Push and open a PR:** `git push -u origin HEAD && gh pr create --fill`.
   Work reaches `main` by merging the PR, never by a local commit to `main`.

A `PreToolUse` hook (`.claude/hooks/branch-guard.sh`, wired in
`.claude/settings.json`) enforces this by **blocking `git commit` while HEAD is
on `main`**. If you hit that block, you are on `main` — branch and retry.

### Release tags (the /sync-nine workflow)

This rule includes `/sync-nine`. Run the doc/spec sync and its commit on a
feature branch, then open a PR. A release tag must point at a commit on `main`,
so cut the `vX.Y.Z` tag **after the PR merges**, on the updated `main` — not on
the branch. Do not push branches, open PRs, or push tags beyond what the user
asked for.

## Architecture at a glance

Nine is a **daemon/client** system, which the directory tree does not make
obvious:

- The **CLI** (`internal/cli`) and **TUI** (`internal/tui`) are thin clients.
- They talk to a long-running **daemon** (`internal/runtime`) over a
  Unix-socket **wire protocol** (`internal/protocol`, newline-delimited JSON).
- The daemon routes each turn to a per-conversation **`AgentWorker`**, which
  drives the agent loop (`internal/agent`).
- **Persistence** — checkpoints, goals, workflows, the event journal — is
  a single SQLite file (`modernc.org/sqlite`, pure Go) in `internal/memory`.
  Config lives in `internal/config` (+ `nine.toml`; the database path is
  `[memory].path`).

Terms like agent / session / conversation / sub-agent / goal / workflow / role
are overloaded and the distinctions matter; see `docs/glossary.md`.

## Changes that cross layers

Two patterns are easy to half-complete — do the whole checklist:

- **Adding a wire message** touches five places in lockstep: the `Msg`
  fields + constructor + the doc-comment block in `internal/protocol/protocol.go`,
  a client method in `client.go`, the dispatch switch in
  `internal/runtime/daemon.go`, the handler in `handlers.go`, and the
  `spec/contracts/wire-protocol.md` tables. If the wire contract changes
  incompatibly, bump `plugin.ProtocolVersion`.
- **`docs/` and `spec/` are the source of truth.** They hold the precise,
  intended behavior — contracts, invariants, wire formats — and are kept in
  sync with the code via `/sync-nine`, so consult them (`nine docs <topic>`,
  `nine spec <topic>`, or the files) before inferring behavior from the
  implementation, and treat a code/doc mismatch as a bug to reconcile, not a
  doc to quietly follow. They are compiled into the binary
  (`docs/embed.go`, `spec/embed.go`), so `nine help` *is* `docs/usage.md` and
  the binary always matches the docs of its version. A behavior change that
  skips the embedded docs silently drifts — reconcile it via `/sync-nine`
  (see above).

## Tests & toolchain

- Go **1.26**. Build with `make build` (`make dev` and `make all` are the same
  thing — nothing ships as its own artifact any more); `make test`, `make lint`,
  `make integration-test`.
- Browser automation is an `[[mcp.server]]`, not a plugin (`docs/browser.md`).
  Its end-to-end test is opt-in and needs `npx` plus an installed browser:
  `NINE_PLAYWRIGHT_TEST=1 go test ./internal/builtins/ -run Playwright`.
- Daemon tests use the harness in `internal/runtime/daemon_test.go`
  (`startDaemon` / `dial` / `seqProvider`) with in-memory stores. Sockets go in
  `/tmp`, not `t.TempDir()`, because macOS caps Unix-socket paths at 104 bytes.
- `TestRegisterPlugin` (`internal/agent`) **was** a known plugin-socket timing
  flake. The amnesty is withdrawn: plugin startup now detects a process that dies
  before listening (instead of waiting out the budget and blaming the socket), and
  the readiness budget is 30s rather than 3s. **Treat a failure there as real.**
