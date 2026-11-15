# Design note — Conversations become agents

**Status:** **Proposed** (requirements `A1`–`A8` recorded) ·
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

### A8 — The web UI gains a Builder UI

The web UI (`web/`) gains a **Builder** view where a user authors, edits and
removes roles, skills and data, and authors, changes and removes agents — the
same operations the `A5` API exposes, driven from the UI. The agent
import/export controls (per `A3`) live in this Builder UI too. Additive on top
of the existing web UI and API (`A1`).

## Analysis

Where the codebase stands (`main` at `52a10c1`, 2026-11-15) against each
requirement, what has to change, and the questions left open. Second pass:
mechanics named at file/symbol level.

**Cross-cutting mechanics** (per `AGENT.md`): an API change touches
`internal/api/openapi.yaml` → regenerated `internal/api/apigen` and strict
handlers → `web/src/api/schema.d.ts` (openapi-fetch client). A wire change
touches the `Msg` fields + constructor + doc block in
`internal/protocol/protocol.go`, a client method in `client.go`, the dispatch
switch in `internal/runtime/daemon.go`, the handler, and the tables in
`spec/contracts/wire-protocol.md`. `A5`, `A7` and `A8` all ride these paths.

### A1 — Reuse what exists; renaming is free

**Stands:** one conversation path end to end — wire messages `new_conversation`
/`conversation_id` (`internal/protocol/protocol.go:26,97`, documented in
`spec/contracts/wire-protocol.md`), `POST /api/v1/conversations`
(`internal/api/handlers_conversations.go:63` → `cl.NewConversationWithRole`),
the `conversations` table and checkpoints (`internal/memory/conversations.go`),
the per-conversation `AgentWorker` (`internal/runtime/agent_worker.go`), and
the web UI — where "agent" already exists as a label: `web/src/pages/Agents.tsx`
is the roster of conversations, processes and goals (`adr/web-ui.md` §5).
52 non-test Go files mention "conversation".

**Required changes:** a naming map deciding per surface whether the identifier
becomes "agent" (wire types, API paths, table name, CLI flags, UI labels,
glossary) or stays; aliases where external clients exist — plugins speak the
wire protocol, so an incompatible rename bumps `plugin.ProtocolVersion`.
Internal symbols (`AgentWorker`, `AgentInfo`) already read as "agent".

**Questions:** do the `/api/v1/conversations` paths change or alias? Is the
`conversations` table renamed (costs a migration) or kept? Which names are
user-facing versus internal-only?

### A2 — Modifications are allowed when needed

**Stands:** nothing to build — this is the governance rule the other entries
invoke.

**Required changes:** every behavior change under `A3`–`A8` is recorded in this
document with its reason, as each analysis below already does.

**Questions:** none.

### A3 — Agents are self-contained and importable/exportable

**Stands:** all four are daemon-wide and shared by every session. Role:
resolved per session from one registry — `NewRoleRegistry(rs,
cfg.DefaultLeafRole)` (`internal/runtime/roles.go:225` region,
`internal/runtime/builder.go:225`); roles are skills with a role block in
frontmatter (`skills/`, `[skills].user_dir`; `docs/roles.md`); the system prompt
is composed from `role.SystemPrompt` (`internal/runtime/builder.go:697`).
Skills: seeded into one daemon-wide vector namespace (`internal/skills`,
`internal/runtime/docs_seed.go`). Self-model: one `Assembler` built once,
`Build(ctx, queryVec)` per turn (`internal/selfmodel/assembler.go:48`), served
at `GET /self-model`, bootstrapped from `[bootstrap] self_model_path`.
Self-reflection: one fixed session `self-reflection` owned by the shipped
`reflect` process (`internal/runtime/bootstrap.go:16`). Export exists only
whole-instance: `GET /backup` → `internal/backup/backup.go` (tar of the db plus
the workspace), CLI `nine backup`/`restore` (`internal/cli`).

**Required changes:** per-agent role (the agent carries a role name or an
inline role definition), per-agent skill set (its own namespace or a subset),
per-agent self-model source (its own file/namespace instead of the single
`Assembler`), per-agent reflection (a session per agent, not the single
`self-reflection`); an export bundle = agent definition + its database (`A4`) +
optionally workspace files, reusing the `internal/backup` tar mechanics; an
import path that optionally copies the role/skills as generalized daemon-wide
copies.

**Questions:** does an imported agent get a fresh database or adopt an existing
one? What stays daemon-wide — board, processes, notifications, the event
journal? Bundle format: same tar as backup, or manifest + db? Who may export an
agent, given its memory may be private?

### A4 — Each agent is stored in a dedicated SQLite database

**Stands:** one `Store` over one file — `memory.Open(path)` with the "single
gateway" invariant (one writer-connection pool; `internal/memory/db.go:138,156`),
path from `[memory].path` (default `~/.nine/nine.db`;
`internal/config/factory.go:169`). One schema holds every table: `kv`, `files`,
`workspace_files`, `vectors`, `conversations`, `goals`, `notifications`,
`user_notifications`, `skills`, `tools`, `tool_state`, `workflows`,
`human_requests`, `interactive_sessions`, `capability_requests`,
`capability_grants`, `session_events`, `event_cursors`, `related_sessions`,
`jobs`, `processes`, `board_tasks`.

**Required changes:** open one `Store` per agent (same engine and schema
machinery) under a per-agent directory; keep a daemon-level registry database
for cross-agent state; route each `AgentWorker` at its agent's store; a
migration that splits an existing `nine.db`; `internal/backup` and restore
adjusted to walk per-agent databases.

**Questions:** the table split — `conversations`/`goals`/`kv`/`vectors` per
agent, `board_tasks`/`processes`/`notifications` daemon-wide? Layout
(`~/.nine/agents/<id>.db`)? The event journal feeds the web UI inbox — does it
stay in the registry db? Auto-split on boot or an explicit migration command?

### A5 — An API builds and runs agents in a running Nine

**Stands:** `POST /api/v1/conversations` (`internal/api/
handlers_conversations.go:63`) forwards `role` + `interactive` over the wire
(`cl.NewConversationWithRole`) and the daemon spawns the `AgentWorker`; the CLI
passes `--role`. No stored agent object exists — a conversation is created
directly with a role name. `GET /roles` lists the registry.

**Required changes:** `POST /api/v1/agents` to compose and persist an agent
definition (name, role, skills, self-model, reflection config — the `A3`
bundle, stored per `A4`), CRUD on that definition, and `POST
/api/v1/agents/{id}/run` to start a session from it; CLI (`nine agent …`) and
web UI as clients of the same endpoints. Rides the cross-cutting mechanics:
openapi.yaml + apigen + a wire message + daemon handler + spec contract.

**Questions:** is "run" a conversation created with the agent's bundle, or a
distinct session kind? Do agent definitions live in the agent's own database or
in the registry? Are definitions versioned?

### A6 — Board workers take a configurable role

**Stands:** the model side is partly there — `run_agent`/`run_agents` accept a
`role` argument, empty resolving to `DefaultLeafRoleName` "executor"
(`internal/agent/register_subagents.go:128`, `internal/runtime/roles.go:100`);
the registry validates names (`role_list`/`role_search`). The board side has
no role: `board_post({title, spec, priority})` — spec is free text
(`internal/agent/register_board.go:104`) — `board_tasks` rows carry no role
column, and a `boardworker` process claims the oldest queued task on its clock
and runs under its own configured process role (`internal/runtime/
process_live.go:417`, `liveprog.go:250` `boardWorkerProgram`). The web UI role
picker sets a new conversation's role only.

**Required changes:** a `role` on the board task — schema column, `board_post`
argument, claim path — resolved against the registry with leaf-only
enforcement; a web UI control to set or override a task's/worker's role.

**Questions:** role named at post time (orchestrator) or claim time (worker)?
Leaf roles only, since leaves are barred from delegating? Does the
boardworker's own process role remain the fallback? Do depth and capability
admission checks apply to task-named roles?

### A7 — Plugins installable into a running Nine

**Stands:** plugins start at boot — built-ins, `[[mcp.server]]`, and
`[plugins].user_dir` scanned at boot (`internal/plugin/userplugins.go:11`);
tool handlers register at loop-build via `RegisterPlugin`, and tool
*definitions* are composed per session at build time under `[context]
tool_exposition` (`internal/runtime/builder.go:563,654`). Runtime *reload*
already exists and crosses the wire: `POST /plugins/reload` and
`POST /tools/reload` → `cl.ReloadPlugins()` (`internal/protocol/client.go:861`)
→ daemon rescans the user dir, restarts user plugins, re-registers their tools
(`internal/plugin/userplugins.go:23`, `internal/api/handlers_tools.go:129,180`).
Missing: an *install* step (no endpoint takes plugin files into the workspace),
a health gate before a plugin's tools are exposed (`socketReadyTimeout` exists
at `internal/plugin/manager.go:35`), and delivery of new tools to sessions
already running (definitions are per-loop-build today). `[plugins] disabled` is
read at boot only.

**Required changes:** an install endpoint (upload or fetch plugin files, place
them under the user dir, trigger the existing reload), a health check through
the manager before tools are exposed, a live-grant path — either rebuild
running loops' tool definitions on reload or resolve definitions per turn — and
a disabled-list and name-collision check at install (`firstToolCollision`,
`internal/plugin/userplugins.go:134`).

**Questions:** distribution format — binary, `[[mcp.server]]` entry, or URL?
Operator-only install? Do new tools reach running conversations without a
restart today, or only new ones? What happens to in-flight sessions when a
plugin is replaced mid-run?

### A8 — The web UI gains a Builder UI

**Stands:** pages (`web/src/app.tsx`): Agents (roster + chat), Tools with
Plugins and Skills views (`web/src/pages/Tools.tsx:25,377,449` — the plugins
view already has a reload button wired to `POST /tools/reload`; the skills view
is read-only), Data (`/data`, including raw `/database/tables` browsing),
Inbox, Journal, System, Processes. Backing APIs: `GET /skills`,
`GET /skills/{name}`, `GET /roles`, `GET /plugins`, read-only `/database/*` and
`/memory` K/V, `GET /backup`; skill authoring is model-side only
(`skill_write`/`skill_modify`). The web client is openapi-fetch over the
generated `web/src/api/schema.d.ts` (`web/src/api/client.ts:41`). No Builder
page, no role/skill write endpoints, no agent CRUD, no import/export controls.

**Required changes:** a `/builder` route and page with CRUD sections for roles,
skills, data and agents, plus agent import/export controls (`A3`); the write
endpoints they need (roles, skills, agents — `A5`); regenerated
`schema.d.ts`/`apigen` from `openapi.yaml`.

**Questions:** REST write endpoints for roles/skills, or server-side reuse of
the model tools? Operator-only access (auth role)? Where in the nav — beside
Tools? Import from file upload or URL?

## Limits

- Requirements `A1`–`A8` are recorded; later entries extend or amend them.
- Related ADRs cover overlapping ground (`agent-boundary.md` splits runtime from
  agent; `process-sessions.md` already separates conversation sessions from
  process sessions); this note decides the user-facing concept, not the internals.
