---
description: Reconcile the in-process eval harness with the current daemon assembly
allowed-tools: Bash(git *), Bash(grep *), Bash(go build*), Bash(make eval-replay*), Bash(gofmt*), Read, Edit, Write, Grep, Glob
---

You are running the **eval harness sync** workflow. The goal: after a change to
nine's daemon assembly, bring the in-process eval harness back into faithful
correspondence with production, so evals keep testing what really runs. Work
concretely off the actual diff — never invent changes.

## Why this exists

`tests/evals/runner/harness.go` stands up the real daemon **in-process** by
mirroring `runDaemon` in `cmd/nine/daemon.go`: it constructs the same stores,
supervisor, `AgentBuilder`, `Daemon`, event sink, and wiring setters, but injects
an isolated store + workspace + swappable provider and **intentionally omits** the
production-only bootstrap (resume, standing agents, self-reflection, scrub,
instance-name resolution). Because it is a hand-maintained parallel of that
assembly, an additive change to `runDaemon` (a new dependency, a new
`daemon.Configure*/Set*` call, a new `AgentBuilderConfig`/`LoopConfig` field) can
leave the harness compiling but no longer reproducing production. This workflow
closes that gap.

## 1. Establish the change set

Run and read:

- `git status --porcelain -- cmd/nine/daemon.go internal/runtime/builder.go internal/runtime/daemon.go`
- `git diff -- cmd/nine/daemon.go internal/runtime/builder.go internal/runtime/daemon.go`
  (add `--staged` if the work is staged)

Summarize, in one short paragraph, **what changed in the assembly**. If nothing
assembly-level changed, stop and say so — there is nothing to sync.

## 2. Classify each change

For each assembly change, decide which bucket it falls in:

- **Mirror it.** A new dependency or wiring the harness must reproduce for evals
  to stay faithful — e.g. a new `AgentBuilderConfig`/`LoopConfig` field the loop
  reads, a new `daemon.Configure*`/`Set*` call that affects a turn, a new store
  or supervisor hookup. Add the equivalent to `harness.go`, keeping the harness's
  injected store/workspace/provider and per-case overrides.
- **Intentionally skip it.** Production-only bootstrap the harness omits by design
  (resume, standing agents, self-reflection, journal/workflow scrub, instance
  name, subscribers that need an embedder). Leave the harness alone.
- **Already covered.** A signature change the compiler forces you to update, or
  something the harness already threads through.

State the classification before editing.

## 3. Reconcile the harness

Apply the "mirror it" changes to `tests/evals/runner/harness.go`. While there,
check the adjacent surfaces that track production:

- New journal event types or payload fields → does the assertion engine
  (`assert.go`, the `trace` digest) need to read them for `trajectory` checks?
- New store side-effects a case might assert → is there a getter in `assert.go`'s
  `gradeSideEffects`, and does the schema in `case.go` expose it?
- New role/tool gating → is it reachable via the forced-role factory?

Only touch what the change affects. Match surrounding style.

## 4. Verify

- `go build ./tests/evals/...` — the harness and runner compile.
- `make eval-replay` — schema validation + Track R (deterministic, no infra).
- If Postgres is up, `go test ./tests/evals/...` runs the harness end-to-end.

If a seed case or fixture is now stale because recorded behavior legitimately
changed, re-record with `make eval-generate` and note it.

## 5. Branch, commit, PR

Per `CLAUDE.md`, never commit to `main`. If not already on a feature branch,
`git checkout -b chore/sync-evals-<summary>` (it carries uncommitted work over),
then commit, `git push -u origin HEAD`, and `gh pr create --fill` — only as far as
the user asked.

## 6. Report

Finish with a short summary: what assembly changed, which harness/assertion
surfaces you updated, what you intentionally skipped and why, and whether any
fixture was re-recorded.
