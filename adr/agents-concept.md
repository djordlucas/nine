# Design note — Conversations become agents

**Status:** **Proposed** (complete: requirements `A1`–`A8`, analysis, sequencing, test plan, all questions resolved) ·
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
one file per agent instead of one per daemon. Idle agent Stores are LRU-closed (default 32 concurrently open) with a configurable max-agents-per-daemon limit (Q35), so per-agent files do not exhaust file descriptors. Also a behavior change under
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
52 non-test Go files under `internal/` mention "conversation".

**Required changes:** a naming map deciding per surface whether the identifier
becomes "agent" (wire types, API paths, table name, CLI flags, UI labels,
glossary) or stays; aliases where external clients exist — plugins speak the
wire protocol, so an incompatible rename bumps `plugin.ProtocolVersion`.
Internal symbols (`AgentWorker`, `AgentInfo`) already read as "agent".

**Questions:** `Q1`–`Q3` — resolved in [Resolved questions](#resolved-questions).

### A2 — Modifications are allowed when needed

**Stands:** nothing to build — this is the governance rule the other entries
invoke.

**Required changes:** every behavior change under `A3`–`A8` is recorded in this
document with its reason, as each analysis below already does.

**Questions:** none.

### A3 — Agents are self-contained and importable/exportable

**Stands:** all four are daemon-wide and shared by every session. Role:
resolved per session from one registry — `NewRoleRegistry(rs,
cfg.DefaultLeafRole)` (`internal/runtime/roles.go:101`, used at
`internal/runtime/builder.go:225`); roles are skills with a role block in
frontmatter (`skills/`, `[skills].user_dir`; `docs/roles.md`); the system prompt
is composed from `role.SystemPrompt` (`internal/runtime/builder.go:697`).
Skills: seeded at boot into one daemon-wide vector namespace
(`internal/runtime/skills_seed.go` `SeedSkills`). Self-model: one `Assembler` built once,
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

**Questions:** `Q4`–`Q7`, `Q30` — resolved in [Resolved questions](#resolved-questions).

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

**Questions:** `Q8`–`Q11`, `Q28`–`Q29`, `Q31` — resolved in [Resolved questions](#resolved-questions).

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

**Questions:** `Q12`–`Q14`, `Q27` — resolved in [Resolved questions](#resolved-questions).

### A6 — Board workers take a configurable role

**Stands:** the model side is partly there — `run_agent`/`run_agents` accept a
`role` argument, empty resolving to `DefaultLeafRoleName` "executor"
(`internal/agent/register_subagents.go:128`, `internal/runtime/roles.go:100`);
the registry validates names (`role_list`/`role_search`). The board side has
no role: `board_post({title, spec, priority})` — spec is free text
(`internal/agent/register_board.go:104`) — the `board_tasks` schema
(`internal/memory/db.go:615`: id, title, spec, posted_by, claimed_by, status,
result, source, priority, depth, timestamps) has no role column, and a
`boardworker` process claims the oldest queued task on its clock
and runs under its own configured process role (`internal/runtime/
process_live.go:417`, `liveprog.go:250` `boardWorkerProgram`). The web UI role
picker sets a new conversation's role only.

**Required changes:** a `role` on the board task — schema column, `board_post`
argument, claim path — resolved against the registry with leaf-only
enforcement; a web UI control to set or override a task's/worker's role.

**Questions:** `Q15`–`Q18`, `Q28` — resolved in [Resolved questions](#resolved-questions).

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

**Questions:** `Q19`–`Q22`, `Q32` — resolved in [Resolved questions](#resolved-questions).

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

**Questions:** `Q23`–`Q26`, `Q34` — resolved in [Resolved questions](#resolved-questions).

## Sequencing

Proposed order, derived from the dependencies in the analysis above. Each phase
lands as rename-or-additive where possible (`A1`); the behavior changes (`A3`
scoping, `A4` storage, `A7` live grants) are recorded under `A2` as they land.
The wire/openapi/spec mechanics are established once in phase 1 and reused after.

| Phase | Entries | Work | Depends on |
|-------|---------|------|-----------|
| 1 | `A1` naming map, `A5` API skeleton | Glossary/naming decisions; `POST /agents` CRUD + `POST /agents/{id}/run` with definitions stored in the existing single store; CLI `nine agent …` | — |
| 2 | `A3` self-containment | Per-agent role/skills/self-model/reflection resolution in the builder; definition export/import (manifest only) | Phase 1 |
| 3 | `A4` per-agent storage | One SQLite file per agent, registry db reduced to `processes`+`board_tasks` (Q31), migration, backup/restore, LRU open-store cap + max-agents limit (Q35); the export bundle gains the agent's db | Phase 2 |
| 4 | `A6` board worker roles | `board_tasks.role` column, `board_post` argument, claim-time resolution, web UI control | Phase 1 (parallel with 2–3) |
| 5 | `A7` runtime plugin install | Install endpoint → user dir → existing reload, health gate, live tool grants | Phase 1 (parallel with 2–4) |
| 6 | `A8` Builder UI | `/builder` page: CRUD for roles/skills/data/agents plus import/export controls; write endpoints for roles and skills | Phases 1–5 |

**Why this order:** the API skeleton (phase 1) fixes the wire/openapi/spec
conventions every later phase reuses, and gives the CLI and web UI something to
call before any behavior changes. Self-containment (phase 2) needs somewhere to
store the agent definition, so it rides phase 1's store; storage (phase 3) is
the most invasive phase — migration, backup/restore — so it comes before
export-with-data, and export/import completes there, when a bundle can carry the
agent's database. `A6` and `A7` are leaf features with no dependency on `A3` or
`A4`; they slot in parallel once the API conventions exist. The Builder UI
(phase 6) is last because it surfaces all of the above and needs the write
endpoints they add.

## Resolved questions

The 35 questions raised in the Analysis, answered. `Q27`–`Q30` record decisions from the completeness review (agent-API auth, worker-session storage, migration mapping, import trust); `Q31`–`Q35` were confirmed in the same review's follow-up on 2026-11-16 (table split, cross-db writes, live grants, backup snapshot, deletion, resource caps). No proposed defaults remain. "Basis" cites the evidence
behind each decision; anything not directly evidenced is a judgment call and
can be reopened.

| # | Asked | Decision | Basis |
|---|-------|----------|-------|
| Q1 | `/api/v1/conversations` paths: change or alias? | Add canonical `/api/v1/agents/*`; old paths stay as aliases | `A1`; the web nav already labels `/conversations` as "Agents" (`web/src/sections.ts:44`) |
| Q2 | Rename the `conversations` table? | No — rename is user-facing only (API, UI, CLI, glossary); internal names follow later | a table rename costs a migration for no behavior gain |
| Q3 | Which names are user-facing? | API paths, web UI labels, CLI flags, `docs/glossary.md` terms; Go symbols and table names are internal | `docs/` avoids code references; `adr/` expects them |
| Q4 | Imported agent: fresh db or adopt? | Fresh agent, fresh id and db; adopting an existing one is an explicit flag, never implicit | an import must never clobber live state |
| Q5 | What stays daemon-wide? | Amended by Q31: only `processes` and `board_tasks` stay in the registry db. Per-agent: `conversations`, `related_sessions`, `kv`, `files`, `workspace_files`, `vectors`, agent-local skills, `tools`/`tool_state`, `goals`, `notifications`, `user_notifications`, `human_requests`, `capability_requests`, `capability_grants`, `interactive_sessions`, agent-owned `jobs`, `workflows`, `session_events`, `event_cursors` | Amended 2026-11-16: all of these already carry `agent_id`/`conversation_id`/`owner_id` (`internal/memory/db.go:352-540`); only board/process coordination is inherently cross-agent |
| Q6 | Export bundle format? | tar reusing `internal/backup` mechanics: `agent.json` manifest (name, role, skills, self-model, reflection config) + `agent.db` + optional `workspace/` | `GET /backup` already tars db + workspace |
| Q7 | Who may export an agent? | Operator-authenticated API/UI only; never a model tool | memory may be private; same posture as `[plugins] disabled` (`docs/plugins.md`) |
| Q8 | Table split | = Q5 as amended by Q31 | — |
| Q9 | Per-agent db layout | `agents/<id>.db` next to `[memory].path` (`~/.nine/agents/<id>.db`; `/data/agents/<id>.db` in the container); the `[memory].path` file becomes the registry db, name unchanged | volume layout (`docs/glossary.md` `/data/`) |
| Q10 | Event journal location | Reversed by Q31: `session_events`/`event_cursors` live in the agent's own db; the web inbox and journal fan out reads across all agent dbs | `session_events.agent_id` exists (`db.go:498`); original daemon-wide-inbox rationale superseded 2026-11-16 |
| Q11 | Migration: auto or command? | automatic at boot, extending the in-code migration runner; per-conversation data moves into per-agent dbs, old file kept as `nine.db.pre-agents.bak`; restore reverses; a CLI verify command checks the split | schema migrations already run automatically on `Store` open (`internal/memory`) |
| Q12 | What is "run an agent"? | a conversation created from the agent's bundle (`RoleParams` from its role); no new session kind | conversations are the interactive unit; the UI already calls them agents |
| Q13 | Where do definitions live? | the registry db; runtime state in the agent's own db (`A4`) | board/processes/UI list agents daemon-wide |
| Q14 | Are definitions versioned? | yes — every change writes a version row, reusing the tool-versions pattern | `/tools/{name}/versions/{version}/restore`, `tool_state` |
| Q15 | Board role: post time or claim time? | post time — the poster names the role in `board_post`; claim cannot change it | mirrors `run_agent`, which names the role at spawn |
| Q16 | Leaf roles only? | yes — validated against the registry's leaf roles; non-leaf refused | workers are sub-agent sessions; leaves (except `executor`) cannot delegate (`docs/roles.md`) |
| Q17 | Boardworker fallback role? | yes — a task with no role runs under the claiming worker's own process role, else `[roles] default_leaf` | today's behavior, preserved |
| Q18 | Admission checks on task-named roles? | yes — existence, leaf-only, depth cap and capability grants, same as delegation | `board_post` admission rules already exist (`register_board.go`) |
| Q19 | Plugin distribution format? | phase 5 covers user-plugin files (upload or URL fetch) dropped into `[plugins].user_dir`, then the existing reload; `[[mcp.server]]` entries stay config-only in this phase | user plugins are the only runtime-reloadable kind today |
| Q20 | Operator-only install? | yes — authenticated operator session required; not exposed to models | same posture as `[plugins] disabled` (operator-only, read at boot) |
| Q21 | Do new tools reach running conversations today? | no — `handlePluginsReload` (`internal/runtime/handlers.go:816`) only rescans/restarts plugins; tool definitions are composed per loop at build (`builder.go:563,654`), so only new sessions see them. Live grants are new behavior in `A7` | verified in code |
| Q22 | Plugin replaced mid-run? | dispatcher swaps handlers atomically per call (old or new, never a gap); running sessions keep old definitions until loop rebuild/resume; no mid-turn kill | dispatcher handler map swap is per-call |
| Q23 | REST write endpoints for roles/skills? | yes — write endpoints on the API; the model's `skill_write`/`skill_modify` tools call the same store layer | one store, two front doors (REST for UI/CLI, tools for the model) |
| Q24 | Builder operator-only? | yes, auth-gated like install/export | `auth_session.go`; consistent with Q7/Q20 |
| Q25 | Builder in nav where? | new top-level "Builder" section (`web/src/sections.ts`, routes `/builder/…`), not under Tools — it covers agents, not just tools | sections list at `web/src/sections.ts:44-49` |
| Q26 | Import from upload or URL? | both, through the same operator-only import endpoint | Q7/Q20 posture; URL fetch lands in the workspace first |
| Q27 | Who may create/run agents via the agent API (A5)? | Any authenticated API client (bearer token or browser cookie); models are excluded implicitly because they never hold the token. This single tier is what "operator" means in Q7/Q20/Q24 | Single-tier `authMiddleware` (`internal/api/middleware.go:19`); no separate operator role exists in code |
| Q28 | Which agent's db holds a board worker / `run_agent` sub-agent session after A4? | The spawning agent's db; when the spawner is the daemon itself (boardworker claim, root-model `run_agent`) the session lands in a default agent's db | Board workers turn on the process handler (`internal/runtime/liveprog.go:250`); keeps worker state out of unrelated user agents |
| Q29 | What does a legacy conversation become in the Q11 migration? | One agent per conversation, adopting that conversation's role as its role bundle; ids stay stable; the old file is kept as `nine.db.pre-agents.bak` per Q11 | Preserves per-conversation roles set via `NewConversationWithRole` |
| Q30 | Trust level for imported agent bundles (A3)? | Staging + review gate: the import lands staged, the role/skills diff is shown for approval before the agent is runnable, and an audit entry is written; the endpoints stay authenticated-API-only per Q7 | Imported role/skills compose the system prompt — a prompt-injection vector |
| Q31 | Cross-db write atomicity after the A4 split? | Move every agent-keyed table — including `session_events`/`event_cursors` — into the agent's db, so the registry db holds only `processes` + `board_tasks`. The remaining cross-db writes (board/process coordination) are ordered registry-first with boot reconciliation; two SQLite files still cannot share a transaction. Cost: daemon-wide inbox/journal/notification reads fan out across all agent dbs, amplified by the Q35 LRU cap. Amends Q5/Q8, reverses Q10 | Confirmed 2026-11-16; the schemas already key these tables by `agent_id`/`conversation_id`/`owner_id` (`internal/memory/db.go:352-540`), and only `processes`/`board_tasks` are cross-agent by nature |
| Q32 | A7 live-grant mechanism for running sessions? | After a plugin reload, running loops resolve tool definitions per turn, so new tools are granted from the next turn (amends Q22's "until rebuild"); still no mid-turn swap | Confirmed 2026-11-16; definitions are composed per loop build today (`internal/runtime/builder.go:563,654`) |
| Q33 | Backup consistency across per-agent dbs? | Checkpoint the WAL on every agent db and the registry db before tarring, then resume; each db is individually consistent, the tar is not cross-db transactional | Confirmed 2026-11-16; `internal/backup` tars files, a WAL checkpoint gives per-db consistency |
| Q34 | Agent deletion semantics (A8 "remove")? | Refuse deletion while the agent has running sessions or referenced board tasks; otherwise stop its sessions and archive the db as `agents/<id>.db.deleted-<ts>` rather than hard delete | Confirmed 2026-11-16; CRUD without delete semantics is undefined today |
| Q35 | Resource caps for per-agent stores? | LRU-close idle agent Stores — default 32 concurrently open — plus a configurable max-agents-per-daemon limit, both configured under `[memory]`; per-db single-writer is already enforced | Confirmed 2026-11-16; one pool + one file per agent (`internal/memory/db.go:138,156`); the cap also bounds Q31's read fan-out |

## Consistency check

Run against `main` at `52a10c1` (2026-11-15). Every file, symbol and line cited
in the Decision and Analysis sections was re-grepped; terms were checked against
`docs/glossary.md`; the document follows `DOCS_STYLE.md` (conclusion first,
`## Limits` last) and is indexed in `adr/README.md`. Corrections made during
the check: `NewRoleRegistry` is defined at `internal/runtime/roles.go:101` (used
at `builder.go:225`), not in the builder; skills are seeded by
`internal/runtime/skills_seed.go` `SeedSkills` (`docs_seed.go` seeds docs/spec,
not skills); the `board_tasks` schema (`internal/memory/db.go:615`) indeed has
no role column; the "52 Go files" count covers `internal/` only. Review additions `Q27`–`Q35` (2026-11-16) verified against `main` at `0adffba0`: `authMiddleware` is single-tier (`internal/api/middleware.go:19`), `boardWorkerProgram` turns on the process handler (`internal/runtime/liveprog.go:250`), `board_tasks` has no role column (`internal/memory/db.go:615`); Q31's amended table split checked against the schemas at `internal/memory/db.go:352-540` (agent/conversation/owner keys present on all moved tables).

## Tests & evals

Repo machinery: `make test` / `make cover` (Go unit), `make lint`, `make
openapi` + `make openapi-check` (regenerate and validate `internal/api/apigen`
and `web/src/api/schema.d.ts` from `openapi.yaml`), `make webui-test` (web unit,
`web/src/*.test.ts`), `make webui-e2e` (playwright, `web/e2e`), `make
integration-test` (`tests/integration`), `make eval-replay` (Track R,
deterministic, every PR) and `make eval-live` (Track L, behavioral, nightly) —
case schema in `docs/evals.md`, cases in `tests/evals/cases`, graded on
side-effects first, then trajectory (`docs/evals.md` §1). Wire changes touch the
five lockstep places plus `spec/contracts/wire-protocol.md`, and bump
`plugin.ProtocolVersion` when incompatible; docs/spec sync via `/sync-nine` on
the feature branch.

| Phase | Unit / integration | E2E | Evals |
|-------|--------------------|-----|-------|
| 1 (`A1`, `A5`) | API handler tests in `internal/api` (`handlers_*_test.go`, `spec_test.go` mux patterns); wire tests in `internal/protocol`; CLI tests (`internal/cli/commands_test.go`); a grep guard that old names still alias | `web/e2e` smoke over renamed routes | Track R: replay a recorded session after the rename (journal intact); Track L: compose an agent via `POST /agents`, run it, assert the conversation row and journal side effects |
| 2 (`A3`) | Builder tests (`internal/runtime/builder_test.go`, `builder_roles_test.go`): per-agent role/skills/self-model resolution; `internal/selfmodel` per-agent source; export/import round-trip modeled on the `internal/backup` tests | — | Track L: imported agent behaves like the exported one (side-effect + trajectory parity); role-gating case: a leaf-role agent never advertises a forbidden tool |
| 3 (`A4`) | `internal/memory` migration test from a fixture `nine.db`; per-agent `Store` open/close and single-writer concurrency; backup/restore round-trip across per-agent databases (`internal/backup/backup_test.go`, `internal/cli/restore_test.go`); Q35 cap test: LRU eviction holds open Stores ≤ 32 under load; Q31 fan-out test: with more agents than the cap, the daemon-wide inbox/journal aggregates `session_events` from every agent db completely (inbox handler test in `internal/api`) | — | Track R: replay a pre-migration recorded conversation post-migration (checkpoint and history intact); Track L: with 100 agents, inbox load returns the complete journal while holding ≤ 32 Stores open |
| 4 (`A6`) | `internal/memory/board_test.go` for the new role column; board handler tests (`internal/agent/register_board.go`, `internal/api/handlers_board_test.go`): role validated, leaf-only, depth/admission checks; claim-time resolution in `internal/runtime/process_live.go` | `web/e2e` spec for the role control on a board task | Track L: orchestrator posts a task naming role X, assert the worker's `sub_agent_start` carries role X; its recording becomes the Track R fixture |
| 5 (`A7`) | Plugin manager tests (`internal/plugin/manager_test.go`, `userplugins_test.go`, `disabled_test.go`, `testplugin/` fixture): install endpoint, health gate (`socketReadyTimeout`), collision refusal, disabled re-check; a live-grant test that a running loop sees a newly installed tool | — | Track L: install an MCP fixture plugin at runtime, then the model calls its tool (pattern: the `process-condition-trigger` MCP fixture); Track R from its recording |
| 6 (`A8`) | Web unit tests (`web/src/*.test.ts`) and API coverage (`web/src/api/coverage.test.ts`, `unused.ts`); write endpoints covered as in phases 1–2 | `web/e2e` Builder spec: CRUD a role/skill/agent, export, import into a fresh instance, run it | Track L: drive the Builder flow end to end (stub model, `web/e2e/stub-model.mjs`) and assert the built agent runs |

## Limits

- Requirements `A1`–`A8` are recorded; later entries extend or amend them.
- Related ADRs cover overlapping ground (`agent-boundary.md` splits runtime from
  agent; `process-sessions.md` already separates conversation sessions from
  process sessions); this note decides the user-facing concept, not the internals.
