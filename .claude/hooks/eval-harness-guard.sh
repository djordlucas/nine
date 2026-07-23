#!/usr/bin/env bash
#
# Stop hook: nag to re-sync the eval harness after the daemon assembly changes.
#
# The in-process eval harness (tests/evals/runner/harness.go) deliberately
# mirrors the production daemon wiring in cmd/nine/daemon.go (runDaemon) and the
# AgentBuilderConfig/LoopConfig structs in internal/runtime/builder.go. Signature
# changes break the harness build and are caught by the compiler; what the
# compiler does NOT catch is a new dependency/setter added to runDaemon (or a new
# config field) that the harness should also wire but silently doesn't — the
# harness keeps compiling while no longer reproducing production.
#
# This fires when those assembly surfaces changed in the working tree but
# harness.go was NOT touched alongside them. Once harness.go is reviewed/updated
# (or the change genuinely doesn't affect assembly and you say so), it goes quiet.
#
# Wired as a Stop hook in .claude/settings.json. See /sync-evals for the workflow.

set -euo pipefail

input="$(cat)"

# Don't loop: if we already blocked once for this stop, let Claude stop.
if [ "$(printf '%s' "$input" | jq -r '.stop_hook_active // false')" = "true" ]; then
  exit 0
fi

# Hooks may run from an arbitrary cwd; anchor to the project root.
root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$root" 2>/dev/null || exit 0

# The production assembly surfaces the harness mirrors.
assembly_changed="$(git status --porcelain -- cmd/nine/daemon.go internal/runtime/builder.go 2>/dev/null || true)"
# Whether the harness was touched in the same working tree.
harness_changed="$(git status --porcelain -- tests/evals/runner/harness.go 2>/dev/null || true)"

if [ -n "${assembly_changed//[[:space:]]/}" ] && [ -z "${harness_changed//[[:space:]]/}" ]; then
  jq -n '{
    decision: "block",
    reason: "The daemon assembly (cmd/nine/daemon.go or internal/runtime/builder.go) changed, but the in-process eval harness (tests/evals/runner/harness.go) was not updated to match. It mirrors runDaemon; a new dependency, setter, or config field that runDaemon now wires may need mirroring so evals keep reproducing production faithfully. Run the /sync-evals workflow: diff the assembly change, reconcile harness.go (and any assertion/store touchpoints), re-run `make eval-replay`. If the change does not affect the harness (e.g. resume/standing-agent/self-reflection bootstrap, which the harness intentionally omits), say so explicitly and stop."
  }'
fi

exit 0
