#!/usr/bin/env bash
#
# PreToolUse (Bash) hook: keep the default branch clean.
#
# Blocks `git commit` while HEAD is on the default branch (main). Every change
# goes on a feature branch with a pull request — see CLAUDE.md. Branch first
# (`git checkout -b <type>/<slug>`), commit there, then open a PR.
#
# Wired as a PreToolUse hook (matcher: Bash) in .claude/settings.json.

set -euo pipefail

input="$(cat)"

cmd="$(printf '%s' "$input" | jq -r '.tool_input.command // ""')"

# Only guard git commits. Everything else passes straight through.
case "$cmd" in
  *"git commit"*) ;;
  *) exit 0 ;;
esac

# Hooks may run from an arbitrary cwd; anchor to the project root.
root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$root" 2>/dev/null || exit 0

branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo)"

# The default branch is origin/HEAD's target; fall back to main. Use an `if`
# so a failed symbolic-ref (origin/HEAD unset locally) doesn't trip `set -e`.
default="main"
if ref="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null)"; then
  default="${ref#origin/}"
fi

if [ "$branch" = "$default" ]; then
  jq -n --arg b "$branch" '{
    hookSpecificOutput: {
      hookEventName: "PreToolUse",
      permissionDecision: "deny",
      permissionDecisionReason: ("On \($b) — every change goes on a feature branch with a PR. Create one first (git checkout -b <type>/<slug>, e.g. fix/…, feat/…, chore/…), commit there, then open a pull request. See CLAUDE.md. This block is deliberate; if you truly must commit to \($b), do it in a terminal outside this session.")
    }
  }'
fi

exit 0
