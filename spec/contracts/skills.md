# Contract — Skills & the Self-Improvement Boundary

**Status:** Built · **Depends on:** memory store, embedder, dispatcher hooks · **Used by:** any turn

Skills are markdown how-to notes — **data, not code**. They are the *only* thing Nine
modifies about itself (invariant I10). Writing one cannot change Nine's behavior except by
giving a future turn something to read.

Skill handling is **core-intercepted and store-backed** with binary-embedded immutable
defaults (`internal/agent/register_skills.go`, `internal/memory/skills.go`,
`skills/skills.go`, `internal/runtime/skills_seed.go`) — not a subprocess plugin.

---

## R-SKILL.1 — File / record format

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

## R-SKILL.2 — Two sources, one of them immutable

| Source | Where it lives | Mutable at runtime? |
|--------|----------------|---------------------|
| **Built-in defaults** | embedded in the binary (`//go:embed *.md`); seeded into the store on **every** boot | **No** — read-only |
| **Agent-authored** | the `skills` table only | Yes — via `skill_write`/`skill_modify` |

`skill_write` **MUST** refuse to overwrite a built-in; `skill_modify` **MUST** refuse to
modify a built-in. To change a built-in, edit its markdown in the source and rebuild the
image.

---

## R-SKILL.3 — Tools (core-intercepted)

| Tool | Effect |
|------|--------|
| `skill_list` | list all skills (names, descriptions, tags) — discovery |
| `skill_read` | read the full content of a skill by name |
| `skill_write` | create or replace one of the agent's **own** skills (not built-ins) |
| `skill_modify` | update one of the agent's own skills (description/tags/content) |

There is **no `skill_search` tool**; discovery is `skill_list` followed by `skill_read`.
(An implementation **MAY** add semantic skill search — the seed step already embeds
descriptions, see R-SKILL.4 — but it is not required.)

Skill tools are never preloaded into context as bulk content; only names/descriptions are
cheap to surface, keeping context lean for small models.

---

## R-SKILL.4 — Indexing hook

After a successful `skill_write`/`skill_modify`, a dispatcher post-call hook (R-DISP.5)
embeds the skill's description into the `skills:` vector namespace. `SeedSkills` does the
same for built-in defaults at boot. This keeps skill vectors current for any future
ranking/search, independent of whether a `skill_search` tool exists yet.

---

## R-SKILL.5 — The self-improvement boundary (N1–N3, I10)

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
