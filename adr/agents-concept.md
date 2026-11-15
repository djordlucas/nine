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

Pending — no sub-requirement has been recorded yet. Each one lands here as a
numbered entry (`A1`, `A2`, …) with the decision it settles.

## Limits

- Skeleton only: the reframe above is the whole decision so far.
- Related ADRs cover overlapping ground (`agent-boundary.md` splits runtime from
  agent; `process-sessions.md` already separates conversation sessions from
  process sessions); this note decides the user-facing concept, not the internals.
