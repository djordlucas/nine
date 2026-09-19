#!/usr/bin/env bash
#
# PreToolUse hook: surface DOCS_STYLE.md whenever a Markdown file is read or
# edited, so the writing rules are in context before the document is.
#
# Fires on Read/Write/Edit/MultiEdit whose file_path is a .md file inside the
# project (vendor/ excluded). Emits the rule summary as additional context and
# never blocks the call.
#
# Wired as a PreToolUse hook in .claude/settings.json. The rules themselves
# live in DOCS_STYLE.md at the project root.

set -euo pipefail

input="$(cat)"

path="$(printf '%s' "$input" | jq -r '.tool_input.file_path // .tool_input.notebook_path // empty')"
[ -n "$path" ] || exit 0

case "$path" in
  *.md|*.markdown) ;;
  *) exit 0 ;;
esac

case "$path" in
  */vendor/*) exit 0 ;;
esac

root="${CLAUDE_PROJECT_DIR:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
guide="$root/DOCS_STYLE.md"
[ -f "$guide" ] || exit 0

# Don't lecture about the style guide while editing the style guide.
case "$path" in
  "$guide") exit 0 ;;
esac

read -r -d '' reminder <<'MSG' || true
DOCS_STYLE.md governs this file. Rules, in order of how often they are broken:
1. Conclusion first — the answer goes in the first screen, not the last section.
2. Every sentence carries a fact — delete metaphor ("load-bearing seam"),
   withheld information ("not the ones you expect"), self-commentary
   ("worth stating plainly"), and empty transitions.
3. Titles name the subject — no teasers, no colon-subtitles, sentence case.
4. Right vehicle — table for multi-attribute items, list for parallel ones,
   diagram for topology, paragraph for one claim plus reasoning.
5. A `## Limits` section goes last, covering what is unbuilt, fragile,
   deliberately out of scope, or slated for removal.
6. Delete superseded content; docs/ and spec/ are present tense only.
7. Use docs/glossary.md terms exactly.
Read DOCS_STYLE.md in full if this edit is more than a typo fix.
MSG

jq -n --arg ctx "$reminder" '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    additionalContext: $ctx
  }
}'

exit 0
