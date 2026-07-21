---
description: Sync docs + spec contracts to recent nine source changes and tag a version bump
allowed-tools: Bash(git *), Bash(grep *), Bash(make build*), Read, Edit, Write, Grep, Glob
---

You are running the **nine doc/spec/version sync** workflow. The goal: after a
change to nine's source, bring `docs/` and `spec/` back in line with reality and
cut a version bump. Work concretely off the actual diff — never invent changes.

## 1. Establish the change set

Run these and read the output:

- `git describe --tags --abbrev=0` — the latest release tag (e.g. `v0.1.0`).
- `git log <latest-tag>..HEAD --stat` — commits since the last tag.
- `git status --porcelain` and `git diff` (and `git diff --staged`) — uncommitted work.

Summarize, in one short paragraph, **what actually changed** across `cmd/`,
`internal/`, `plugins/`, `Makefile`, and config. If nothing source-level
changed, stop and say so — there is nothing to sync.

## 2. Update docs and spec to match

Map each real change to the documents that describe that surface, then edit them
so they describe the new behavior. Touch only what the change affects:

- `docs/*.md` — user- and developer-facing docs (architecture, plugins,
  configuration, versioning, usage, glossary, etc.).
- `spec/contracts/*.md` and `spec/*.md` — the normative contracts. If a wire
  format, method, config field, or interface changed, the matching contract
  MUST change too.

Match the surrounding prose style, density, and headings. Don't add docs for
things that didn't change. If a change makes an existing doc statement false,
fix it rather than appending a note.

## 3. Decide the version bump (semver, git-tag driven)

Per `docs/versioning.md`, the release version is **not** a file — it lives in
git tags and is injected at build time. Nine is past `1.0`, so standard SemVer
applies:

- **Breaking** change (wire protocol, config shape, CLI surface, a public
  contract in `spec/`) → bump the **major**: `v1.1.0` → `v2.0.0`.
- **Backward-compatible feature** (additive) → bump the **minor**: `v1.1.0` → `v1.2.0`.
- **Fix** (no new surface) → bump the **patch**: `v1.1.0` → `v1.1.1`.

Separately, if the **native plugin wire contract** changed, also bump
`plugin.ProtocolVersion` in `internal/plugin/contract.go` (its own integer,
independent of the release version — see versioning.md §2).

State the chosen next version and one line of justification before tagging.

## 4. Branch, commit, PR — then tag

Every change goes on a feature branch with a pull request (see `CLAUDE.md`), so
never commit the sync straight to `main`. If you are not already on a feature
branch, create one first — `git checkout -b` carries the uncommitted work over:

```sh
git checkout -b docs/sync-<summary>   # skip if already on a feature branch
git add -A
git commit -m "<concise summary of the change>

Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>"
git push -u origin HEAD
gh pr create --fill
```

A release tag must point at a commit on `main`, so cut it **after the PR merges**,
on the updated `main` — not on the branch:

```sh
git checkout main && git pull
git tag -a vX.Y.Z -m "Nine vX.Y.Z — <summary>"
git push origin vX.Y.Z   # only when explicitly asked
```

State the chosen version in the PR, and do **not** push the branch, open the PR,
or push the tag beyond what the user asked for.

## 5. Report

Finish with a short summary: what changed, which docs/spec files you updated,
the new tag, and whether `plugin.ProtocolVersion` moved. Note anything you were
unsure mapped to a doc so the user can double-check.
