#!/usr/bin/env bash
#
# Stop hook: nag to sync docs/spec/version after nine source changes.
#
# Fires when the working tree has changes to nine *source* surfaces
# (cmd/, internal/, plugins/, Makefile, go.mod) but docs/ and spec/ were
# NOT touched alongside them — i.e. the /sync-nine workflow hasn't run yet.
# Once docs/spec are updated (or everything is committed + tagged), the
# condition goes false and the hook stays quiet.
#
# Wired as a Stop hook in .claude/settings.json. See /sync-nine for the
# actual workflow this reminds Claude to run.

set -euo pipefail

input="$(cat)"

# Don't loop: if we already blocked once for this stop, let Claude stop.
if [ "$(printf '%s' "$input" | jq -r '.stop_hook_active // false')" = "true" ]; then
  exit 0
fi

# Hooks may run from an arbitrary cwd; anchor to the project root.
root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$root" 2>/dev/null || exit 0

# Source surfaces whose change should be mirrored into docs + spec.
src_changed="$(git status --porcelain -- cmd internal plugins Makefile go.mod 2>/dev/null || true)"
# A pure test-only change doesn't need doc/spec edits.
src_changed="$(printf '%s\n' "$src_changed" | grep -v '_test\.go$' || true)"
docs_changed="$(git status --porcelain -- docs spec 2>/dev/null || true)"

if [ -n "${src_changed//[[:space:]]/}" ] && [ -z "${docs_changed//[[:space:]]/}" ]; then
  jq -n '{
    decision: "block",
    reason: "Nine source changed in the working tree but docs/ and spec/ were not updated to match. Run the /sync-nine workflow: update the relevant docs and spec contracts for what changed, bump plugin.ProtocolVersion if the wire contract broke, then commit and create a git tag bumping the release version per semver (0.x: breaking=minor, safe=patch). If this change genuinely needs no doc/spec update, say so explicitly and stop."
  }'
fi

exit 0
