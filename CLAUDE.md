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
