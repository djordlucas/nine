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
and stays functional through the move.

### A2 — Modifications are allowed when needed

Changing something is not forbidden when it is needed: if a behavior truly has
to change, it can. Each such change is recorded in this document with the
reason it could not be a rename, so the reuse default of `A1` stays auditable.

### A3 — Agents are self-contained and importable/exportable

An agent bundles what makes it that agent: its **role**, its **skills**, its
**self-model** and its **self-reflection** are agent-specific, attached to the
agent rather than to the daemon. An agent can be exported as a single unit and
imported into Nine, so it is portable. Optionally, a role or a skill can have a
generalized copy imported into Nine itself, shared daemon-wide. Moving these
from daemon scope to agent scope is a behavior change under `A2`; it cannot be
a rename because self-containment requires them to travel with the agent.

### A4 — Each agent is stored in a dedicated SQLite database

An agent's persistent state lives in its own SQLite file rather than in the
daemon's single database (`[memory].path`, default `~/.nine/nine.db`): the same
engine (`modernc.org/sqlite`) and the same `Store` machinery (`internal/memory`),
one file per agent instead of one per daemon. Also a behavior change under
`A2` — the reason is `A3`: the store must travel with the agent for export and
import to be whole.

### A5 — An API builds and runs agents in a running Nine

The daemon's HTTP API (`internal/api`) covers the agent lifecycle against the
running instance: compose an agent — its role, skills, self-model and
self-reflection per `A3`, stored in its own database per `A4` — and then run
it, which starts the agent and opens the thread the user talks to. The CLI
(`internal/cli`) and the web UI (`web/`) are clients of this same API; neither
composes agents on its own. New endpoints sit beside the existing ones, which
keep working unchanged (`A1`).

### A6 — Board workers take a configurable role

A board worker — the sub-agent session the orchestrator spawns per task
(`run_agent`/`run_agents`, or a `boardworker` process claiming a queued task
from `board_tasks`) — runs under a role that is configurable per worker and
set by either the model (naming the role when it posts or spawns the worker,
discovered via `role_list`/`role_search`) or a person in the web UI. Today a
worker's role is picked at delegation and defaults to the `[roles] default_leaf`
(`executor`); the web UI role picker sets the role of new conversations
(`POST /api/v1/conversations`), not of board workers.

### A7 — Plugins installable into a running Nine

The daemon's HTTP API (`internal/api`), called by the CLI (`internal/cli`) and
the web UI (`web/`), can take plugin files, install them into the workspace and
start them while Nine runs. Once a plugin is running and healthy, its exposed
tools are registered with the dispatcher and can be given to models — including
agents whose sessions are already running, without a restart. Today plugins
start only at boot (`[[mcp.server]]`, `[plugins].user_dir`, build-time
built-ins — `docs/plugins.md`) and register tools at loop-build time, so
runtime install plus live tool grants is a behavior change under `A2`.

## Limits

- So far `A1`–`A7` are recorded; later entries extend or amend them.
- Related ADRs cover overlapping ground (`agent-boundary.md` splits runtime from
  agent; `process-sessions.md` already separates conversation sessions from
  process sessions); this note decides the user-facing concept, not the internals.
