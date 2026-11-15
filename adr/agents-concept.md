# Design note — Conversations become agents

**Status:** **Proposed** (skeleton — sub-requirements are added one by one) ·
**Date:** 2026-11-15 · **Related:** `adr/agent-boundary.md`, `adr/concept-consolidation.md`, `adr/process-sessions.md`, `docs/glossary.md`

The unit a user drives in Nine moves from the **conversation** concept — an
interactive thread held in the web UI or through the API (`docs/glossary.md`) —
to an **agents** concept: the thing a user creates, opens and talks to is an
agent. Sub-requirements (what an agent is relative to sessions, sub-agents and
process sessions; what changes in the API, the web UI, the store and the
glossary) are recorded below as they are decided, one by one.

## Decision

### A1 — Reuse what exists; renaming is free

The agents concept is built on the existing conversation machinery, not beside
it: the `conversations` table and checkpoints, the wire protocol, the daemon
handlers and the web UI flow stay as they are. The default is renaming only —
concept, identifiers, API paths, UI labels — because Nine is functional today
and stays functional through the move. A behavior change is allowed when it
truly needs to change; each such change is recorded here with the reason it
could not be a rename.

## Limits

- So far only `A1` is recorded; later entries extend or amend it.
- Related ADRs cover overlapping ground (`agent-boundary.md` splits runtime from
  agent; `process-sessions.md` already separates conversation sessions from
  process sessions); this note decides the user-facing concept, not the internals.
