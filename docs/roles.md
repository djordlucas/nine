# Roles — worker kinds as data

**Status:** Built (2026-07-04) · **Depends on:** skills, dispatcher, builder, orchestration, session-plans · **Supersedes:** the `depth int` mechanism in the loop builder and the `AgentWorker`/sub-agent fork of identity

This document was the **implementation handoff** and is kept as design rationale. The
normative requirements now live in [`spec/contracts/roles.md`](../spec/contracts/roles.md)
(with the R-ROLE.* deltas applied to `dispatcher.md`, `orchestration.md`, `skills.md`,
and the gates added to `spec/conformance.md`) — where this document and the spec
disagree, the spec wins. The implementation lives in `internal/runtime/roles.go`,
`internal/runtime/builder.go`, and `skills/roles/*.md`; the §12 acceptance gates are
covered by `internal/runtime/roles_test.go`, `internal/runtime/builder_roles_test.go`,
`internal/agent/restrict_test.go`, and `skills/skills_test.go`.

---

## 1. Summary

Today one privileged top-level agent "does everything" until it delegates, and the
difference between that agent and a sub-agent is faked with two proxies:

- **`depth int`** threaded through `AgentBuilder.build(agentID, depth, interactive)`
  (`internal/runtime/builder.go`). `depth == 0` gets goal-session spawning; `depth < 2`
  gets delegation tools; `depth == 2` is a pure leaf. These "depths" are really *roles*
  in disguise (orchestrator vs executor).
- **The `AgentWorker` vs `subagent.go` split.** A `AgentWorker` persists
  (checkpoints), runs a **routine plan**, and owns user I/O + HITL. A sub-agent
  (`RunSubAgentSync`/`SpawnSubAgent`) is an ephemeral one-shot `turn()` with `nil` plan,
  `nil` save, `nil` notif.

Note a consequence of the current code that shapes this design: the top-level
orchestrator, the **reflection** session, and every **pursue** session are all built
through the *same* depth-0 factory (`AgentBuilder.Build` → `build(id, 0, …)`, reached via
`LoopFactory`/`makeAgentWorker`). They carry the **identical full tool surface** —
including delegation and, at depth 0, goal-spawn — and are distinguished today only by
their session *plan/profile*, not by their tools. Giving reflection and pursue distinct,
*narrower* roles is therefore a deliberate behavior change, not a pure refactor (see §4's
behavior-change note and §8 phase 1).

A **Role** makes worker-kind first-class data. A role bundles:

1. the **"how"** — a persona/how-to (this is skill-shaped; see §3), and
2. the **"what"** — an enforced **tool allowlist** (a skill cannot do this — it is
   advisory context, droppable under budget pressure; the tool boundary must be enforced
   at loop-build time), and
3. **structural wiring** — does this worker persist? run routines? own HITL? spawn
   goal-sessions? delegate?

With roles, the top-level `AgentWorker` is simply the worker running the **orchestrator
role** ("its job is to follow the session"), and a sub-agent is a worker running a **leaf
role**. `depth` is a *guardrail*, not the gating mechanism (§6). A leaf's persona comes
from its role body.

### Why "roles as data": the optimization

Coarse, named leaf roles (`software-dev`, `sysadmin`, `report-writer`) are not just
ergonomic — they are a real efficiency win:

- **Prompt caching.** Each coarse role has a *stable* tool-definition block and *stable*
  system prompt, cacheable across every spawn of that role. A bespoke per-spawn tool
  allowlist busts the cache on every delegation.
- **Smaller tool surface per leaf.** Today `buildToolList` hands every sub-agent the
  *entire* plugin toolset; the leaf sifts a large list every turn. 4–6 tools → fewer
  wrong-tool calls, fewer tokens.
- **Cheaper delegation decision.** The orchestrator picks a role by name from a menu
  instead of enumerating tool names.
- **Structural blast-radius.** `report-writer` *cannot* `shell` — enforced at the
  dispatcher, not requested in a prompt.

---

## 2. The `Role` descriptor

Reference shape (a conforming implementation MAY use different names; it MUST NOT change
the observable tool boundary or the frontmatter format of §3):

```interface
// Role is a worker kind: how-to + tool boundary + structural wiring.
// It subsumes the `depth`-based gating and the AgentWorker/subagent fork.
type Role struct {
    Name         string    // "orchestrator", "executor", "software-dev", ...
    Description  string    // one line; what this role is for (surfaced to the picker)

    // The "how". Body of the backing skill (§3). May be empty for the default executor.
    SystemPrompt string    // resolved from the skill body at load time

    // The "what". See §5 for resolution.
    AllTools     bool      // true  ⇒ every available tool (plugin + core), minus depth-gated
    Tools        []string  // used only when AllTools == false: strict allowlist

    // Structural wiring. Only operator-authored roles — built-in or user —
    // MAY set these true (R-ROLE.7); agent-authored ones are forced to leaf
    // defaults.
    Delegates    bool      // gets run_agent/run_agents/workflow_* (was: depth < 2)
    SpawnsGoals  bool      // gets goal-session spawn fn         (was: depth == 0)
    Persists     bool      // saveFn/checkpointing wired         (AgentWorker vs sub-agent)
    Interactive  bool      // HITL: ask_human (approval gates follow the owning
                           // session instead — R-HITL.5)
    Profile      []string  // routine kinds; nil ⇒ ephemeral leaf (no routines)
}
```

`goal_*` tools are considered part of the delegation bundle for tool-registration
purposes; `SpawnsGoals` additionally controls whether the *background pursue-session
spawn function* is wired (only the orchestrator gets it), matching today's depth-0 rule.

---

## 3. Storage — roles live in skill frontmatter (decision: Option A)

A role **is** a skill that declares a `role:` block. This reuses the entire existing
skill pipeline (embedded `//go:embed` defaults in `skills/`, `SeedSkills` on boot, the
`skills` table, `skill_read` for the body) and closes the loop on the original question
"is a role just a skill?" — yes, a skill **plus** an enforced tool allowlist.

**R-ROLE.1 — Frontmatter format.** A skill MAY carry an optional `role:` block. A skill
*with* a `role:` block is *also* a role; a skill *without* one is an ordinary knowledge
skill, unchanged (backward compatible — R-SKILL.1 still holds).

```markdown
---
name: software-dev
description: Implement and modify code — edit files, run builds and tests via shell.
tags: [coding, dev]
role:
  tools: [shell, read_file, write_file, file_store, file_fetch, file_list,
          file_search_text, memory_get, memory_set, memory_list, skill_read, skill_list]
  # structural flags below are honored for operator-authored skills — built-in
  # and user (docs/skills.md) alike (R-ROLE.7); for agent-authored skills they
  # are ignored and forced to leaf defaults.
  delegates: false
  spawns_goals: false
  persists: false
  interactive: false
  profile: []
---

# Software Dev role
You implement and modify code. Prefer editing existing files over creating new ones...
(the skill body becomes the role's SystemPrompt)
```

**R-ROLE.2 — Wildcard tools.** `tools: "*"` (or an omitted `tools` key with a role
block present) means **all available tools** (`AllTools = true`) — the superset used by
the orchestrator and the default executor. An explicit list means a **strict allowlist**
(`AllTools = false`).

**R-ROLE.3 — The role body is the persona.** The skill's markdown body becomes the
role's `SystemPrompt`. For the orchestrator this MAY be empty, in which case the loop
falls back to the daemon's configured `SystemPrompt` (today's behavior), so nothing
regresses if the orchestrator role ships with an empty body.

---

## 4. Built-in roles (seeded from `skills/`)

Ship these as embedded skill files (suggested: `skills/roles/*.md`, still picked up by
the existing `//go:embed`). Tool names below are exact (verified against the running
plugins: `files` → `read_file`/`write_file`; `http` → `http_get`/`http_post`/
`web_search`/`web_page_read`; `shell` → `shell`; `time` → `time`; core-intercepted →
`memory_*`, `file_*`, `skill_*`).

**MCP tools and allowlists.** An MCP server's tools are prefixed with the server name
(`playwright__browser_navigate`), and the prefix is chosen by the operator in
`nine.toml`. A tool list is matched exactly — there is no pattern form — so a
*built-in* role with an allowlist can never name an MCP tool, and will not receive
one however the deployment is configured. This is why `report-writer` researches over
HTTP even where a browser is available.

Two ways out, both the operator's:

- Author a role in `skills.d/roles/` naming the prefixed tools, since the operator
  knows their own server names.
- Use an `AllTools` role (`tools: "*"`), which receives whatever is loaded.

Extending allowlists to match a prefix pattern would remove the need for both; it is
not implemented, and would be a change to R-ROLE.2.

### Structural roots

| Role | AllTools | Delegates | SpawnsGoals | Persists | Interactive | Profile |
|------|:--------:|:---------:|:-----------:|:--------:|:-----------:|---------|
| `orchestrator` (the session-follower) | ✓ | ✓ | ✓ | ✓ | (from caller) | `["active"]` |
| `reflection` | ✗ (`memory_*`, `skill_read`) | ✗ | ✗ | ✓ | ✗ | `["idle-reflection"]` |
| `pursue` | ✓ | ✓ | ✗ | ✓ | ✗ | `["pursue"]` |
| `executor` (default leaf) | ✓ | (depth-gated, see §6) | ✗ | ✗ | ✗ | `nil` |

> **Behavior change (intended).** Today reflection and pursue sessions are built at depth
> 0 and carry the orchestrator's *full* tool surface (delegation, and at depth 0
> goal-spawn). The `reflection` and `pursue` roles above deliberately narrow that surface
> — `reflection` down to `memory_*` + `skill_read`, and both down to no goal-spawn. This
> is the one place the roles rollout is **not** behavior-preserving; only `orchestrator`
> and `executor` reproduce prior behavior exactly. See §8 phase 1 and §12 gate 6.

`executor` reproduces today's sub-agent *toolset* (full toolset minus depth-gated tools,
no persistence, no routines) and is the default when a delegation call names no role — the
backward-compat anchor (§8). It does **not** reproduce today's sub-agent *persona*, which
is a latent bug this feature fixes (R-ROLE.10).

`executor` is the one leaf role with `Delegates:true` (shown as "depth-gated" above): a
spawned executor can still sub-delegate exactly as today's depth-1 sub-agent does, with
`depthGuard` providing the cap (§6). Every **coarse** leaf role (below) sets
`Delegates:false` and therefore *cannot* sub-delegate at all — an intended narrowing
relative to today's "any depth-1 agent may delegate" behavior. So `executor.Delegates`
must be `true` for backward compatibility even though the field is a plain bool.

**R-ROLE.10 — Fix the sub-agent persona (retire `SubAgentSystemPrompt`).** Today
`internal/runtime/subagent.go` defines `SubAgentSystemPrompt` ("You are Nine operating in
sub-agent mode… you have a specific, finite task… do not ask clarifying questions…") but
**never wires it**: `build()` sets `SystemCore: lc.SystemPrompt`, so every sub-agent
silently inherits the *orchestrator's* full system prompt — the wrong persona for a
finite, non-interactive leaf. As part of this feature:

- The intent of `SubAgentSystemPrompt` MUST become the **`executor` role's body** (its
  `SystemPrompt` per R-ROLE.3) — the canonical leaf persona. Every leaf role's body
  SHOULD open from this same finite-task, no-clarifying-questions stance.
- The `SubAgentSystemPrompt` constant MUST be **removed** once the executor role carries
  its content; leaving a defined-but-unwired constant is exactly the inconsistency this
  fixes (and it double-serves the ROADMAP's "understand and clean the codebase from
  inconsistencies, tech debt and missing tests" item).
- After the fix, a spawned leaf's `SystemCore` MUST be its role body, never
  `lc.SystemPrompt` — the orchestrator prompt reaches only the orchestrator role (whose
  body MAY be empty and fall back to `lc.SystemPrompt`, R-ROLE.3).

### Coarse leaf roles (the optimization)

| Role | Tools | Backing knowledge skill |
|------|-------|------------------------|
| `software-dev` | `shell, read_file, write_file, file_store, file_fetch, file_list, file_search_text, memory_get, memory_set, memory_list, skill_read, skill_list` | `go-development`, `git-workflow` |
| `sysadmin` | `shell, read_file, write_file, http_get, http_post, memory_get, memory_set, memory_list, skill_read` | `shell-usage` |
| `report-writer` | `web_search, web_page_read, http_get, read_file, file_store, file_fetch, file_list, file_search_text, memory_get, memory_set, skill_read` (**no `shell`, no `write_file`**) | `web-research` |

All coarse leaf roles are `Delegates:false, SpawnsGoals:false, Persists:false,
Interactive:false, Profile:nil`. (A `qa`/`browser` role over a browser MCP server's
`<name>__browser_*` tools is an obvious later addition; not required for v1.)

---

## 5. Tool resolution — enforce at BOTH boundaries

**R-ROLE.4 — Two-boundary enforcement.** A role's tool set MUST be enforced at *both*:

1. **The advertised `Tools` list** the loop sends to the model (`buildToolList` today) —
   so the model never *sees* a disallowed tool, and
2. **The dispatcher handler registration** (`registerCoreTools` /
   `registerSubAgentTools` / `RegisterPlugin` today) — so a disallowed tool cannot be
   *called* even if the model hallucinates it.

Resolution algorithm inside `build()`:

```text
available = plugin tools (from Mgr.Running) ∪ core-intercepted tools ∪ (delegation tools if allowed) ∪ (ask_human if interactive)
if role.AllTools:
    selected = available            (minus depth-gated tools per §6)
else:
    selected = available ∩ role.Tools    (intersection — see R-ROLE.5)
register handlers only for `selected`; advertise only `selected`.
```

**R-ROLE.5 — Roles only narrow, never widen (security).** The intersection in the
non-wildcard branch is normative: a role allowlist naming a tool that the daemon does not
expose (unknown name, or a plugin that is not running) MUST be **silently dropped**, not
created. A role can only ever *restrict* the daemon's total tool surface. This is what
makes agent-authored roles safe (§ R-ROLE.7) and preserves invariant N4 (no new access
to daemon-private state).

`gap_report` remains always-registered regardless of role (it is the escape hatch when
no allowed tool fits — a role that narrows tools makes `gap_report` *more* important, not
less).

---

## 6. How `build()` consumes a role

Signature change:

```text
AgentBuilder.build(agentID string, depth int, interactive bool)
  → AgentBuilder.build(agentID string, role Role, depthGuard int)
```

`interactive` folds into `role.Interactive` (the caller sets it on the orchestrator role
instance it passes for interactive sessions). Branch replacements in the current body:

| Current (builder.go) | Becomes |
|----------------------|---------|
| `if depth < 2 { registerSubAgentTools(...) }` | `if role.Delegates && depthGuard > 0 { registerSubAgentTools(...) }` |
| `if depth == 0 { goalSpawn = *p }` | `if role.SpawnsGoals { goalSpawn = *p }` |
| `if interactive && HITL != nil { registerHumanTools }` | `if role.Interactive && HITL != nil { registerAskHuman }`, plus `registerApprovalGates` for any loop with an interactive owner (R-HITL.5) |
| `buildToolList(lc, depth)` | `buildToolList(lc, role, depthGuard)` (applies §5) |
| `SystemCore: lc.SystemPrompt` | `SystemCore: role.SystemPrompt or fallback lc.SystemPrompt` (R-ROLE.3) |
| sub-agent spawn `f.build(subID, depth+1, false)` | `f.build(subID, resolvedLeafRole, depthGuard-1, subGate(...))` |

**Call sites beyond `build()`.** `build` has only two direct callers today: `Build`
(depth 0) and the sub-agent spawn (depth+1). Reflection and pursue sessions reach `build`
*indirectly* through `LoopFactory` (`internal/runtime/daemon.go`, currently typed
`func(agentID, interactive) *Loop` with **no** role) inside `makeAgentWorker`
(`internal/runtime/handlers.go`). To give reflection/pursue distinct roles,
`LoopFactory`/`makeAgentWorker` MUST become role-aware: they resolve the session's role
from its plan **profile** (`active` → `orchestrator`, `idle-reflection` → `reflection`,
`pursue` → `pursue`) and pass that role into `build`. This profile→role resolution is the
plumbing that turns today's profile-only distinction into a tool-level role distinction;
it is the mechanism behind the §4 behavior-change note.

**R-ROLE.6 — Depth becomes a guardrail, not the mechanism.** Termination is primarily
structural: a leaf role has `Delegates:false`, so it cannot spawn further. `depthGuard`
(default `2`, decremented on each spawn) is a **backstop** that hard-stops delegation if
a misconfigured `Delegates:true` role would otherwise recurse without bound. When
`depthGuard == 0`, delegation tools are not registered even for a delegating role. This
preserves the "runaway recursion is structurally impossible" guarantee (R-ORCH.3 / I6).

---

## 7. Selecting a role at delegation time

**R-ROLE.8 — `run_agent`/`run_agents` gain an optional `role` field.** Add to the tool
schemas in `internal/agent/register_subagents.go`:

```schema
run_agent  input += { "role": string (optional, default "executor") }
run_agents input += each task gets optional { "role": string }
```

- The orchestrator picks a role by **name** (a small enum in the tool description listing
  the available leaf roles + their one-line descriptions, so the model chooses well).
- That enum is **rendered from the live registry** each time an agent is built
  (`RoleRegistry.RoleEnum` → `agent.SubAgentDefs`), so store-backed role skills —
  operator-authored and agent-authored alike — are advertised, not just built-ins. It is
  deterministically ordered (built-ins first, then store roles, each by name) because it
  feeds a tool schema. Storeless callers fall back to `agent.DefaultRoleEnum`.
- An unknown/omitted role resolves to `roles.default_leaf` (default `executor`, §11;
  R-ROLE.9), never an error — the system degrades to today's behavior.
- The system prompt SHOULD steer: "when delegating, pick the narrowest role that fits;
  use `executor` only when no coarse role matches." This sentence is rendered from the
  live registry too (`DelegationSteering`) and composed onto the system core of any
  worker that can delegate — not baked into the daemon prompt, which a role with its own
  body never sees. Hardcoding the names here would steer the model toward the built-ins
  and away from the operator's own roles, however well the schema advertises them.

Semantic role selection (embedding the request against role descriptions, reusing the
`skills:` vector namespace that already exists) is a **MAY** for a later iteration — v1 is
explicit selection.

**R-ROLE.9 — Resolution & fallback.** A `RoleRegistry` (built from seeded role-skills at
boot, refreshed when agent-authored role-skills change via the R-SKILL.4 hook) resolves a
name to a `Role`. Unknown name → `roles.default_leaf` (default `executor`, §11). A leaf role that is somehow marked
`Persists`/`Interactive`/`SpawnsGoals` (only possible for built-ins) is still only ever
*spawned* as a leaf by the delegation path, which passes `depthGuard-1` and ignores those
root-only flags for spawned children.

---

## 8. Backward compatibility & migration

Introduced with only `orchestrator`, `executor`, `reflection`, `pursue` and every
delegation defaulting to `executor`, the design is **behavior-preserving for top-level
conversations and sub-agents** (the `orchestrator`/`executor` paths). The **one intended
behavior change** is that `reflection` and `pursue` — today built at depth 0 with the
orchestrator's full tool surface (§1, §4) — are narrowed to their role toolsets. Suggested
phases (each independently shippable, each with a green test gate):

1. **Introduce `Role` + `RoleRegistry`, rewrite `build()` to take a `Role`.** Define the
   struct, seed only the four structural roots, and make the loop-build path role-aware.
   `build` has two direct callers — `Build` (→ orchestrator) and the sub-agent spawn
   (→ executor); reflection and pursue reach `build` through `LoopFactory`/
   `makeAgentWorker`, which now maps the session **profile** to a role (`active` →
   orchestrator, `idle-reflection` → reflection, `pursue` → pursue; see §6). **Gate:**
   top-level conversations and sub-agents are byte-for-byte identical to today
   (orchestrator/executor reproduce depth-0/leaf exactly). Reflection and pursue
   **intentionally** change: they shed the delegation/goal-spawn/full-tool surface they
   inherited from the shared depth-0 factory and take on their narrowed role toolset (§4).
   Update their tests to assert the new toolset rather than "no diff."
2. **Enforce two-boundary tool filtering (§5) and fix the sub-agent persona
   (R-ROLE.10).** Move `SubAgentSystemPrompt`'s content into the `executor` role body,
   wire spawned leaves to use their role body as `SystemCore`, and delete the constant.
   Still no coarse roles. **Gate:** executor still sees the full toolset; a spawned leaf's
   `SystemCore` is the executor body, **not** `lc.SystemPrompt`; `SubAgentSystemPrompt` no
   longer exists in the tree; a temporary test role with a narrow allowlist proves both
   the advertised list and dispatcher registration are filtered, and that a dropped tool
   errors as "unknown tool" when force-called.
3. **Add the `role` field to `run_agent`/`run_agents` (R-ROLE.8), defaulting to
   executor.** **Gate:** delegating with no role is byte-for-byte today's behavior;
   delegating with `role:"software-dev"` yields a leaf whose tool list is exactly the
   allowlist.
4. **Ship the coarse leaf roles (§4) as embedded skill files; update the system prompt to
   steer role selection.** **Gate:** `report-writer` leaf cannot call `shell` (dispatch
   returns unknown-tool); prompt-cache hit rate on repeated same-role spawns improves
   (observable via provider metrics / logs).
5. **(Optional) migrate `SkillRef` prompts and lift contracts into `spec/`.** Move the
   `Normative contract deltas` below into `spec/contracts/roles.md` + edits, add the
   `R-ROLE.*` rows to `spec/conformance.md`, and run the `sync-nine` skill.

---

## 9. Security & self-improvement boundary

**R-ROLE.7 — Agent-authored roles are purely restrictive.** Skills are the only thing
Nine modifies about itself (I10), and agent-authored skills are mutable at runtime
(`skill_write`/`skill_modify`). A role block on an **agent-authored** skill:

- MAY declare a `tools` allowlist — but by R-ROLE.5 this can only *narrow* the existing
  tool surface, never grant a tool the daemon does not already expose. No privilege
  escalation is possible.
- MUST have its **structural flags ignored / forced to leaf defaults**:
  `Persists:false, Interactive:false, SpawnsGoals:false, Delegates:false, Profile:nil`.
  Only **built-in** (embedded, immutable) role-skills MAY set structural flags true. This
  prevents an agent from writing itself a role that grants persistence, HITL, or
  goal-spawning. It keeps N1–N3 intact: defining a role is *writing data* (a skill), not
  changing Nine's executable shape.

Consequence: agents can usefully author *narrowing* roles (e.g. a "read-only auditor"
role) to delegate safely, but cannot use roles as an escalation path.

---

## 10. Edge cases & failure modes

- **Role names an unavailable plugin tool** (plugin crashed / not running): dropped per
  R-ROLE.5; the leaf simply lacks it and may `gap_report`.
- **Empty allowlist** (`tools: []`, not `"*"`): a leaf with only `gap_report` (always
  registered) + nothing else. Legal but near-useless; the picker SHOULD avoid it. Not an
  error.
- **Empty vs omitted `profile`**: an empty (`profile: []`) or absent `profile` key is
  equivalent to `nil` — an ephemeral leaf with no routines. (Unlike `tools`, there is no
  wildcard distinction for `profile`; `[]` and `nil` mean the same thing.)
- **`depthGuard` exhausted with a delegating role**: delegation tools simply aren't
  registered; the worker behaves as a leaf. No error.
- **Interactive orchestrator delegates**: children are always non-interactive
  (`Interactive:false` on spawned leaves), so R-HITL.1 holds — a sub-agent never gets
  `ask_human`, because the spawn path never sets `Interactive`. Its *approval gates* are
  a separate channel: they ride the parent's `gateCtx` down to the child and prompt on the
  owning session's stream (R-HITL.5), so delegation does not bypass `require_approval`.
- **Role block malformed in frontmatter**: seeding SHOULD log and treat the skill as a
  plain knowledge skill (no role), never fail the boot.

---

## 11. Config knobs (optional)

- `roles.default_leaf` (default `"executor"`) — the role used when a delegation names
  none. Allows an operator to make the default stricter.
- `roles.max_delegation_depth` (default `2`) — the `depthGuard` seed.

Both are `MAY`; the defaults reproduce current behavior.

---

## 12. Acceptance gates (for `spec/conformance.md`)

1. Delegating with no `role` produces a leaf identical to today's sub-agent (same tools,
   no persistence, no routines).
2. Delegating with `role:"report-writer"` produces a leaf that **cannot** call `shell`:
   the tool is absent from the advertised list *and* `Dispatch("shell", …)` returns
   `unknown tool`.
3. An agent-authored skill with `role: { persists: true, tools: "*" }` yields, when
   spawned, a **non-persisting leaf** (structural flags ignored) — R-ROLE.7.
4. A role allowlist naming a nonexistent tool spawns a leaf without that tool and does
   not error — R-ROLE.5.
5. `depthGuard` reaching 0 removes delegation tools from a `Delegates:true` role —
   R-ROLE.6.
6. The `orchestrator` and `executor` roots give top-level conversations and sub-agents
   their full respective tool surfaces — **with the one
   intended exception** of the sub-agent persona (gate 7). The `reflection` and `pursue`
   roots **intentionally** narrow their tool surface (§4): a reflection session does not
   advertise `shell`/delegation, and neither reflection nor pursue can spawn goal sessions.
7. A spawned leaf's `SystemCore` equals its role body (the executor body for a default
   leaf), never the orchestrator's `lc.SystemPrompt`, and `SubAgentSystemPrompt` is absent
   from the tree — R-ROLE.10.

---

## Normative contract deltas

To be lifted into `spec/` when implemented.

**New contract `spec/contracts/roles.md`** — carries `R-ROLE.1`…`R-ROLE.10` above
(R-ROLE.10 is the sub-agent-persona fix / `SubAgentSystemPrompt` removal).
Status: Planned → Built on ship. Depends on: skills, dispatcher, builder, orchestration,
session-plans.

**Edit `spec/contracts/dispatcher.md` R-DISP.6** — replace "depth-aware registration"
with "role-aware registration": the builder takes `(agentID, role, depthGuard)`; tool
registration is governed by `role.Delegates`/`role.SpawnsGoals`/`role.Tools` (§5–§6),
with `depthGuard` as the recursion backstop. The per-role delegation surface is the
illustrative special case (orchestrator; executor leaf at guard 1/0).

**Edit `spec/contracts/orchestration.md` R-ORCH.3** — the depth cap is restated: leaf
roles are non-delegating (structural termination); `depthGuard`/`max_delegation_depth` is
the backstop. Add `role` to the `run_agent`/`run_agents` input schema (R-ORCH.1) per
R-ROLE.8.

**Edit `spec/contracts/skills.md` R-SKILL.1** — the frontmatter MAY include a `role:`
block (§3); a skill with one is also a role. Add R-ROLE.7 cross-reference: agent-authored
role blocks are purely restrictive.

**Edit `spec/conformance.md`** — add the §12 gates keyed to `R-ROLE.*`.

---

## Reference symbols (files a future session will touch)

- `internal/runtime/builder.go` — `AgentBuilder.build`, `buildToolList`,
  `registerCoreTools`, `registerSubAgentTools`, `coreToolNames`, `subAgentToolNames`
  (the core of the change; `depth` → `role`/`depthGuard`).
- `internal/runtime/subagent.go` — `RunSubAgentSync`/`SpawnSubAgent` shrink to
  "build a worker with a leaf role, run one turn, tear down"; delete/retire
  `SubAgentSystemPrompt`.
- `internal/agent/register_subagents.go` — add `role` to `run_agent`/`run_agents`
  schemas and plumb it through `SubAgentTask`.
- `skills/` (+ new `skills/roles/*.md`) and `skills/skills.go` — parse the `role:`
  frontmatter block; embed the built-in role-skills.
- `internal/memory/skills.go` — extend the `Skill` record with an optional parsed role.
- `internal/runtime/skills_seed.go` — build/refresh the `RoleRegistry` from seeded +
  agent-authored role-skills (reuse the R-SKILL.4 indexing hook to refresh on write).
- `internal/runtime/daemon.go` — `LoopFactory` gains a role (or a profile→role resolver);
  it is the shared indirection through which reflection and pursue sessions reach `build`,
  so it MUST become role-aware (§6).
- `internal/runtime/handlers.go` (`makeAgentWorker`), `internal/runtime/bootstrap.go`,
  `internal/runtime/goal_session.go` — resolve the session's role from its plan **profile**
  and pass it through (`active` → orchestrator / `idle-reflection` → reflection / `pursue`
  → pursue); the sub-agent spawn passes executor.
- `internal/runtime/prompts.go` — unaffected: `ReflectionPrompt`/`PursuePromptTemplate`
  are routine *turn texts*, not system prompts, and stay as-is.
</content>
</invoke>
