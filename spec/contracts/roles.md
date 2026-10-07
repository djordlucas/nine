# Contract — roles: worker kinds as data

**Status:** Built · **Depends on:** skills, dispatcher, context builder, orchestration, processes · **Used by:** loop builder, delegation tools

A **role** makes worker-kind first-class data: a persona (skill body), an **enforced
tool allowlist**, and structural wiring (persistence, delegation, goal-spawning, HITL
eligibility, routine profile). Roles carry all tool gating: the top-level worker is the
**orchestrator** role, a sub-agent is a worker running a **leaf** role, and `depth` is a
recursion guardrail. Full design rationale and the
implementation handoff live in [`../../docs/roles.md`](../../docs/roles.md); this file
is the normative contract.

---

## R-ROLE.1 — frontmatter format

A skill ([`skills.md`](skills.md) R-SKILL.1) **MAY** carry an optional `role:`
frontmatter block. A skill *with* a role block is *also* a role; a skill *without* one
is an ordinary knowledge skill, unchanged.

```schema
role:
  tools: "*" | [name, ...]   # wildcard or strict allowlist (R-ROLE.2)
  delegates: bool             # run_agent/run_agents/workflow_*/goal_* tools
  spawns_goals: bool          # background pursue-session spawn fn
  persists: bool              # checkpointing session worker vs ephemeral leaf
  interactive: bool           # HITL-eligible (effective only with an interactive caller)
  profile: [routine-kind, ...]  # [] / absent ⇒ ephemeral leaf, no routines
```

A malformed role block **MUST NOT** fail skill loading — the skill degrades to a plain
knowledge skill.

## R-ROLE.2 — wildcard tools

`tools: "*"` (or an omitted `tools` key with a role block present) means **all
available tools**. An explicit list means a **strict allowlist**.

## R-ROLE.3 — the role body is the persona

The skill's markdown body becomes the role's system prompt. An empty body falls back to
the daemon's configured system prompt (so the orchestrator, whose body is empty, runs on
the daemon prompt).

## R-ROLE.4 — two-boundary enforcement

A role's tool set **MUST** be enforced at *both*:

1. the **advertised tool list** the loop sends to the model, and
2. the **dispatcher handler registration** — a disallowed tool dispatches as
   `unknown tool` even if the model hallucinates its name.

Wildcard roles keep the full handler set (including registered-but-unadvertised
tools such as `gap_report` and the embedding-backed memory tools).

## R-ROLE.5 — roles only narrow, never widen

For allowlist roles, the effective tool set is the **intersection** of the allowlist
with the tools the daemon exposes. A name the daemon does not expose (unknown tool, or
a plugin that is not running) **MUST** be silently dropped, never created.
`gap_report` remains registered regardless of role — it is the escape hatch when no
allowed tool fits.

## R-ROLE.6 — depth becomes a guardrail

Termination of delegation is primarily structural: leaf roles have `delegates: false`.
A **depthGuard** (default seed **2**, configurable via `roles.max_delegation_depth`,
decremented on each spawn) is the backstop: when it reaches 0, delegation tools are not
registered even for a delegating role. Runaway recursion stays structurally impossible
(I6, R-ORCH.3).

## R-ROLE.7 — agent-authored roles are purely restrictive

A role block on an **agent-authored** skill MAY declare a `tools` allowlist (which, by
R-ROLE.5, can only narrow). Its **structural flags MUST be ignored** and forced to leaf
defaults (`persists/interactive/spawns_goals/delegates: false`, no profile). Nine
defining a role for itself is writing data, not changing its executable shape (I10,
N1–N3) — so it can never become an escalation path.

The line is **authorship, not storage**: *operator-authored* role skills — built-in
(embedded) and user (`[skills].user_dir`, R-SKILL.2) alike — **MAY** set structural
flags. A user role skill arrives on a file the operator mounted, the same trust level as
editing `nine.toml`; treating it as untrusted would mean an operator could not define a
persisting or delegating role at all. *Agent-authored* role skills (`source=agent`,
written at runtime via `skill_write`) stay purely restrictive.

Both kinds are equally bound by R-ROLE.5: a `tools` list can only ever narrow the
daemon's surface. Trust governs structure, never the allowlist.

## R-ROLE.8 — role selection at delegation time

`run_agent` input and each `run_agents` task gain an optional `role` field (string).
The tool schema **SHOULD** enumerate the available leaf roles with one-line descriptions
so the model chooses well; the system prompt **SHOULD** steer toward the narrowest
fitting role.

That enumeration **MUST** be rendered from the live role registry at agent-build time,
not fixed at compile time, and **MUST** cover store-backed role skills (operator- and
agent-authored) as well as built-ins. A role that resolves but is never advertised is
reachable only if the model guesses its name — which makes an operator's role skill
effectively invisible. The rendered list **MUST** be deterministically ordered
(built-ins first, then store-backed, each sorted by name) so the tool schema is stable
across boots.

Only the `role` field's description varies; the tools' `name` and `description` stay
fixed, keeping the builder's name-keyed tool-embedding cache valid. A storeless caller
falls back to a static list of the built-in leaf roles.

The **system prompt** steering ("pick the narrowest fitting role") is subject to the same
rule: its role names **MUST** be rendered from the live registry, not hardcoded, or the
prompt steers toward the built-ins and away from the very roles the schema advertises.
It **MUST** be composed onto the system core of every worker that can actually delegate
— including one whose role supplies its own body and therefore never sees the daemon
prompt (the `executor` is exactly that case) — and **MUST NOT** be given to a role that
cannot delegate, for which it is dead text. Names alone belong here; the one-line
descriptions are already in the tool schema.

## R-ROLE.9 — resolution and fallback

A role registry resolves names: built-ins first (embedded role skills, loaded at
boot), then agent-authored role skills (looked up in the store at resolve time, so a
`skill_write` takes effect on the next delegation). An unknown or omitted name resolves
to `roles.default_leaf` (default `executor`) — never an error. A spawned child always
runs as a **leaf**: root-only structural flags are ignored on the delegation path.

## R-ROLE.10 — the leaf persona

A spawned leaf's system core **MUST** be its role body (the executor body for a default
leaf) — never the orchestrator's daemon prompt. The executor role body carries the
finite-task, no-clarifying-questions sub-agent stance.

---

## Built-in roles

Seeded from `skills/roles/*.md` (embedded, immutable — R-SKILL.2 applies):

| Role | Tools | Delegates | SpawnsGoals | Persists | Interactive | Profile |
|------|:-----:|:---------:|:-----------:|:--------:|:-----------:|---------|
| `orchestrator` | `*` | ✓ | ✓ | ✓ | ✓ (from caller) | `[active]` |
| `reflection` | `memory_*`, `skill_read` | ✗ | ✗ | ✓ | ✗ | the `reflect` process |
| `pursue` | `*` | ✓ | ✗ | ✓ | ✗ | `[pursue]` |
| `executor` (default leaf) | `*` | ✓ (guard-capped) | ✗ | ✗ | ✗ | — |
| `software-dev` | shell + file + memory allowlist | ✗ | ✗ | ✗ | ✗ | — |
| `sysadmin` | shell + file + http allowlist | ✗ | ✗ | ✗ | ✗ | — |
| `report-writer` | web + workspace-file allowlist (**no `shell`**, no method beyond `http_get`) | ✗ | ✗ | ✗ | ✗ | — |
| `monitor` | web + read-only file allowlist (**no write of any kind**) | ✗ | ✗ | ✗ | ✗ | — |
| `code-reviewer` | read-only file allowlist + memory (**no `shell`, no web**) | ✗ | ✗ | ✗ | ✗ | — |
| `analyst` | none (`tools: ""`) | ✗ | ✗ | ✗ | ✗ | — |

The daemon resolves a session's role from the process that drives it
([`processes.md`](processes.md) R-PROC.6): a conversation runs the orchestrator, a goal
session its `pursue` process's role (pursue, or a standing agent's declared role), and the
self-reflection session the reflection role. `orchestrator` and `executor` carry
the full delegation surface; `reflection` and `pursue` are deliberately **narrowed** to
their background purpose.

---

## Config

| Key | Default | Meaning |
|-----|---------|---------|
| `roles.default_leaf` | `"executor"` | role used when a delegation names none |
| `roles.max_delegation_depth` | `2` | depthGuard seed (R-ROLE.6) |

---

## Reference symbols

`internal/runtime/roles.go` (`Role`, `RoleRegistry`, `roleNameForPlan`),
`internal/runtime/builder.go` (`AgentBuilder.build(agentID, role, depthGuard)`,
`buildToolList`, `filterByRole`), `internal/agent/dispatcher.go` (`RestrictTo`),
`nine/skills` (`RoleSpec`, role frontmatter parsing), `skills/roles/*.md`.
