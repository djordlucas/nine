# Nine evals

End-to-end behavioral tests for Nine, per [docs/evals.md](../../docs/evals.md).
Two tracks:

- **Track R — replay** (deterministic, no live model). A recorded session journal
  is re-executed against a recorded provider/dispatcher and the reproduced answers
  are asserted equal to the recorded ones. Guards the loop / dispatcher / context
  code — the parts we own. Infra-free; runs on every PR.
- **Track L — live** (behavioral). The same case run against a real model (or a
  matrix), graded over N runs with tolerant, side-effect- and trajectory-based
  assertions. Needs plugin binaries + a model.

## Layout

```
cases/        *.yaml — the case corpus (schema: docs/evals.md §2)
replay/<id>/  recorded Track-R fixtures (journal.json)
runner/       the Go harness: loads cases, isolates state, drives turns, grades
reports/      JSON + rendered grid, one per live run
```

## Running

```sh
make eval-replay      # Track R + schema validation. No model, no database.
NINE_EVAL_MODELS=qwen3.5:4b make eval-live   # Track L matrix
make eval-generate    # re-record the committed Track-R fixtures
```

The runner drives turns **in-process**: it stands up the real daemon (the
production wiring from `cmd/nine/daemon.go`) over a per-run SQLite database file
and an ephemeral workspace, so a case sees a clean world and grading reads the
same durable journal (`session_events`) production writes.

### Environment

| Var | Purpose |
|-----|---------|
| `NINE_EVALS_LIVE=1` | enable the Track-L matrix (`TestLiveMatrix`) |
| `NINE_EVAL_MODELS` | comma-separated model list for the matrix |
| `NINE_EVAL_TIER` | run only one tier (e.g. `smoke`) |
| `NINE_BINARY` | path to a built nine binary — the built-in plugins are served out of it (default from `make eval-live`) |
| `NINE_LLM_ENDPOINT` | Ollama endpoint for the matrix models |

## Keeping the harness faithful

`runner/harness.go` hand-mirrors the production daemon assembly (`runDaemon` in
`cmd/nine/daemon.go`). Signature changes break its build; **additive** changes (a
new dependency/setter/config field) can leave it compiling but no longer
reproducing production. Two guards keep it honest:

- **Stop hook** (`.claude/hooks/eval-harness-guard.sh`): nags when
  `cmd/nine/daemon.go` or `internal/runtime/builder.go` changed in the working
  tree but `harness.go` wasn't touched alongside.
- **`/sync-evals` command** (`.claude/commands/sync-evals.md`): the reconciliation
  workflow — diff the assembly change, classify each part (mirror / intentionally
  skip / already covered), update the harness, and re-run `make eval-replay`.

## Turn snapshots

`runner/snapshot_test.go` runs six kinds of session (orchestrator, interactive,
delegating, executor, reflection, pursue) through the harness with a scripted
provider, and compares what the model received and what the journal recorded
with `runner/testdata/snapshots/`. It needs no model and runs with `make test`.

A change that alters a prompt, a tool, enrichment or the journal fails it.
When the change is intended, re-record and review the diff:

```bash
go test -mod=vendor ./tests/evals/runner/ -run TestTurnSnapshots -update
git diff tests/evals/runner/testdata/snapshots
```

## Adding a case

Write a `cases/<id>.yaml` per the schema in docs/evals.md §2, forcing the target
behavior via `prompts` and asserting with the **strongest** available check
(side-effect > trajectory > answer). For a Track-R case, record its fixture once
(`make eval-generate`, or capture a live run's journal).

### Cases that need an MCP server

`setup.mcp_servers` brings up one bridge per entry, the same way the daemon's
`startMCPServers` does — that is how a case reaches any capability Nine does not
implement itself, a browser included. `command` and `args` are environment-expanded,
so a case names a fixture the suite builds rather than a temp path:

```yaml
setup:
  mcp_servers:
    - name: fixture
      command: "${NINE_EVAL_MCP_FIXTURE}"
```

`NINE_EVAL_MCP_FIXTURE` is the in-repo server under
`internal/builtins/testmcpserver`, compiled by `TestLiveMatrix` on the live path
only — `make eval-replay` stays infra-free. It is hermetic where a real `npx`
server would drag network, a minute-plus package resolve, and someone else's
release cadence into a suite meant to grade the model; the daemon cannot tell the
two apart. Fidelity against a real server is covered out of band by
`internal/builtins/mcp_playwright_test.go` (see [browser.md](../../docs/browser.md) §7).

`mcp-tool-call` is the worked example: it asserts the model calls the *prefixed*
tool (`fixture__mcp_echo`) and that its output reaches the answer.

### Cases that need a skill catalog

The built-in skills are seeded into every run's store, exactly as the daemon seeds them
at boot, so a case sees the same catalog production does.

`setup.skills` adds more. Give each a **description**: `skill_search` ranks the `skills`
vector namespace by embedded description, so a seeded skill without one is stored but
invisible to semantic discovery — it can only be reached by name.

```yaml
setup:
  skills:
    pg-restore:
      description: Recover the production Postgres database after data loss
      content: "..."
```

`skill-search` is the worked example, and shows why this matters: a case that has the
*model* write the catalog cannot then test search, because writing it puts the names in
the conversation and the model reads by name instead.

### Cases that need real infrastructure

A fixture proves the bridge works; it cannot prove a *browser* is usable by an agent.
`browser-read-page` runs the real upstream Playwright MCP server and grades whether the
agent finds and drives it without being told the tool name — the seam `mcp-tool-call`
does not reach.

Such a case declares `requires_env`, and is reported as **skipped** when the variable is
unset rather than run and failed, so `make eval-live` does not start requiring a browser
on every machine. A skip is never fatal, and it is recorded in the report rather than
dropped — a case that never ran must not read as a pass.

```sh
npx @playwright/mcp@0.0.79 install-browser chrome-for-testing
NINE_EVAL_BROWSER=1 NINE_EVAL_MODELS=qwen3.5:4b make eval-live
```
