#!/usr/bin/env bash
#
# Stop hook: nag to re-sync the eval harness after the shared daemon assembly
# surface changes.
#
# Production (cmd/nine/daemon.go) and the in-process eval harness
# (tests/evals/runner/harness.go) both build a daemon by calling
# runtime.Assemble with an AssemblyConfig. That refactor removed the duplicated
# *wiring* — anything added inside Assemble now reaches both callers for free.
#
# What it did NOT remove is drift in the config surface itself. AssemblyConfig is
# a struct with named fields, so adding a field and passing it from
# cmd/nine/daemon.go still compiles cleanly at the harness call site, which
# silently gets the zero value. That is the remaining silent gap, and it is why
# this hook was retargeted rather than deleted.
#
# Fires when internal/runtime/assembly.go changed in the working tree but
# harness.go did not. See /sync-evals for the workflow.

set -euo pipefail

input="$(cat)"

# Don't loop: if we already blocked once for this stop, let Claude stop.
if [ "$(printf '%s' "$input" | jq -r '.stop_hook_active // false')" = "true" ]; then
  exit 0
fi

# Hooks may run from an arbitrary cwd; anchor to the project root.
root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
cd "$root" 2>/dev/null || exit 0

# The shared assembly surface: the config struct both call sites fill in.
assembly_changed="$(git status --porcelain -- internal/runtime/assembly.go 2>/dev/null || true)"
# Whether the harness was touched in the same working tree.
harness_changed="$(git status --porcelain -- tests/evals/runner/harness.go 2>/dev/null || true)"

if [ -n "${assembly_changed//[[:space:]]/}" ] && [ -z "${harness_changed//[[:space:]]/}" ]; then
  jq -n '{
    decision: "block",
    reason: "internal/runtime/assembly.go changed but tests/evals/runner/harness.go did not. Both cmd/nine/daemon.go and the harness fill in an AssemblyConfig, and because it is a named-field struct, a NEW field passed only by production compiles fine at the harness call site and silently takes the zero value there. If this change added or altered an AssemblyConfig field, decide explicitly what the harness should pass and update it; then re-run `make eval-replay`. If the change was confined to the body of Assemble, it already reaches both callers — say so and stop."
  }'
fi

exit 0
