# Contract — skills & the self-improvement boundary

**Status:** Built · **Depends on:** memory store, embedder, dispatcher hooks · **Used by:** any turn

Skills are markdown how-to notes — **data, not code**. They are the *only* thing Nine
modifies about itself (invariant I10). Writing one cannot change Nine's behavior except by
giving a future turn something to read.

Skill handling is **core-intercepted and store-backed** with binary-embedded immutable
defaults (`internal/agent/register_skills.go`, `internal/memory/skills.go`,
`skills/skills.go`, `internal/runtime/skills_seed.go`) — not a subprocess plugin.

---

## R-SKILL.1 — file / record format

A skill has YAML-style frontmatter and a markdown body:

```markdown
---
name: git-commit
description: How to create a well-formed git commit with a meaningful message
tags: [git, vcs]
---

# Creating a Git Commit
1. ...
```

Stored as a `Skill{Name, Description, Tags, Content[, Source]}` record. `name` and
`description` are what discovery surfaces; `content` is the full body read on demand.

The frontmatter **MAY** additionally carry a `role:` block ([`roles.md`](roles.md)
R-ROLE.1); such a skill is *also* a role. Agent-authored role blocks are purely
restrictive — their structural flags are ignored (R-ROLE.7). Built-in role skills live
under `skills/roles/` and are embedded/seeded like any other built-in (R-SKILL.2).

---

## R-SKILL.2 — three sources, two of them immutable

| Source | Where it lives | Mutable at runtime? |
|--------|----------------|---------------------|
| **Built-in defaults** | embedded in the binary (`//go:embed *.md`); seeded into the store on **every** boot | **No** — read-only |
| **User (operator-authored)** | `[skills].user_dir` on disk; seeded into the store on **every** boot | **No** — read-only to the agent |
| **Agent-authored** | the `skills` table only | Yes — via `skill_write`/`skill_modify` |

Both file-backed sources are immutable to the agent: `skill_write` **MUST** refuse to
overwrite one and `skill_modify` **MUST** refuse to modify one. A runtime write would be
undone by the next boot's reseed anyway, and for user skills it would let Nine edit the
operator's intent. To change a built-in, edit its markdown in the source and rebuild;
to change a user skill, edit the file and restart.

### User skills

`[skills].user_dir` mirrors the built-in layout — `*.md` at the top level, role skills
under `roles/` — and uses the **same format** as a built-in (R-SKILL.1). Discovery is
**boot-only**; there is no watcher.

Seeding **MUST**:

- run **after** the built-ins, so collisions are checked against the full built-in set;
- **reject** a user skill whose name matches a built-in — built-ins win, and a same-named
  user skill would be silently clobbered by the next reseed;
- **validate** each file and **skip** invalid ones with a logged reason, without failing
  the boot — one typo must not take the daemon down;
- **prune** `source=user` rows whose files are gone, leaving built-in and agent-authored
  skills untouched, so the directory is the source of truth. An **absent or unconfigured**
  directory is a no-op and **MUST NOT** prune (there is no desired state to reconcile);
  an existing but empty one means "no user skills" and prunes normally.

Validation covers name shape (`^[a-z0-9]+(-[a-z0-9]+)*$`), a required non-empty
description, a non-empty body for non-role skills (a role body **MAY** be empty — that is
the R-ROLE.3 daemon-prompt fallback), and known routine kinds in a role `profile`. Tool
names in a role allowlist are **NOT** validated: core-intercepted tools are not registered
with the plugin manager, so any name-based check would reject valid allowlists. An
implementation **SHOULD** expose this validator as an offline command (`nine skills
validate`) so an operator can check a file without restarting.

---

## R-SKILL.3 — tools (core-intercepted)

| Tool | Effect |
|------|--------|
| `skill_list` | list all skills (names, descriptions, tags) — full enumeration |
| `skill_search` | semantically rank skills against a query, returning names + descriptions — discovery by meaning |
| `skill_read` | read the full content of a skill by name |
| `skill_write` | create or replace one of the agent's **own** skills (not built-ins) |
| `skill_modify` | update one of the agent's own skills (description/tags/content) |

Discovery is either `skill_list` (enumerate) or `skill_search` (rank the `skills:`
vector namespace by relevance, R-SKILL.4), followed by `skill_read` for the body.
`skill_search` is **embedder-gated**: with no embedder configured it is neither
registered nor advertised, and discovery falls back to `skill_list`.

Skill tools are never preloaded into context as bulk content; only names/descriptions are
cheap to surface, keeping context lean for small models.

The system prompt **SHOULD** steer the agent to consult skills before a task with an
established procedure — discovery followed by `skill_read` — rather than relying on the
passive name hint alone. That hint is context priority 5 and is dropped first under
budget pressure, so it is least likely to survive on the long turns where a skill helps
most. Because `skill_search` is embedder-gated, such steering **MUST NOT** name it as
the only route: it has to admit `skill_list`, or it names a tool that does not exist on
an embedder-less deployment.

---

## R-SKILL.4 — indexing hook

After a successful `skill_write`/`skill_modify`, a dispatcher post-call hook (R-DISP.5)
embeds the skill's description into the `skills:` vector namespace. `SeedSkills` does the
same for built-in defaults at boot. This keeps skill vectors current for both the
passive self-model ranking and the `skill_search` tool (R-SKILL.3).

---

## R-SKILL.5 — the self-improvement boundary (N1–N3, I10)

Skills are the **whole** of runtime self-modification. A conforming implementation
**MUST NOT** provide any tool or path that:

- generates, builds, starts, hot-swaps, or rolls back a plugin (N1);
- edits `nine.toml` or soft-reloads configuration (N2);
- reads, rebuilds, or restarts from its own source tree (N3).

Consequently the runtime image carries **no Go toolchain, no git, and no source tree**.
The supervisor's self-improvement loop is limited to *writing skills* — when a task
completes in a way worth codifying, it **SHOULD** capture the approach as a skill so
future similar work can reuse it. It does not write code.

---

## Reference symbols

`internal/agent/register_skills.go` (core-intercepted `skill_*`),
`internal/memory/skills.go` (`Skill`, `SkillUpsert/Get/List/Delete`),
`skills/skills.go` (embedded defaults + parser), `internal/runtime/skills_seed.go`
(`SeedSkills`).
