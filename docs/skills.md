# Skills

Skills are markdown how-to notes describing reusable knowledge or procedures. When a skill is semantically relevant to the current task, its name is surfaced into the agent's self-model so it can read the full skill on demand — acting as on-demand documentation or playbooks.

---

## What Skills Are

A skill has a name, a description, an optional set of tags, and a markdown body. Built-in skills are authored as `.md` files with YAML frontmatter:

```markdown
---
name: git-workflow
description: Best practices for Git branching, committing, and pull requests
tags: [git, version-control, workflow]
---

## Git Workflow

### Branch naming
Use lowercase kebab-case: `feature/add-login`, `fix/null-pointer`, `chore/update-deps`

### Commits
Write commit messages in imperative mood: "Add login page", not "Added login page".
```

The description is embedded into the `skills` vector namespace; the self-model scores skills by cosine similarity to the current query and lists the most relevant ones so the agent can `skill_read` them.

---

## Where Skills Live

Skills are stored in the **memory store** (the `skills` table in PostgreSQL), not on the filesystem. There are two kinds:

| Kind | Source | Mutable at runtime? |
|---|---|---|
| **Built-in** | The `.md` files in the repo's `skills/` directory, embedded into the `nine` binary at build time | **No** — immutable |
| **Agent-authored** | Written by Nine via `skill_write` / `skill_modify` | Yes |

Built-in skills are **seeded into the store on every boot** from the binary, which is their single source of truth: editing a skill file and rebuilding updates it, and removing one prunes it. Agent-authored skills persist in the store across restarts and are never touched by the seeder.

This split is deliberate: the curated default skills should not drift on a running instance (which could cause inconsistent behaviour across sessions), so they are changed only through normal development — edit the file, rebuild. Nine remains free to capture what it learns in its own skills.

---

## Tools

| Tool | Effect |
|---|---|
| `skill_list` | List all skills (built-in and agent-authored) with names, descriptions, tags |
| `skill_read` | Read a skill's full content by name |
| `skill_write` | Create or replace one of Nine's own skills (refuses built-in names) |
| `skill_modify` | Update one of Nine's own skills (refuses built-in skills) |

These are **core-intercepted** tools (handled in-process against the memory store), not a plugin subprocess.

```bash
./nine "List all available skills"
./nine "Show me the contents of the git-workflow skill"
./nine "Create a skill called 'python-testing' with best practices for pytest and fixtures"
```

Attempting to overwrite or modify a built-in skill returns an error directing the change to the repo + rebuild.

---

## Adding or Editing a Built-in Skill

Edit (or add) a `.md` file in the repo's `skills/` directory and rebuild. On the next boot the seeder upserts it into the store and embeds its description. There is no runtime path to change a built-in skill.

---

## How Skills Affect the Agent

Relevant skill names are injected into the self-model block (context priority 5 — dropped first under budget pressure). For complex turns with long histories, the skills hint may be trimmed; if a skill is critical, mention it explicitly:

```bash
./nine "Using the git-workflow skill, help me write a commit message for these changes: ..."
```

---

## Skills vs. Memory

| | Skills | Memory (KV store) |
|--|--------|------------------|
| Storage | `skills` table (PostgreSQL) | `kv` table (PostgreSQL) |
| Retrieval | Semantic (embedding similarity) | Exact key lookup |
| Scope | Surfaced into context automatically | Explicitly fetched with `memory_get` |
| Best for | Reusable procedures, guidelines | Per-session state, dynamic data |

Use skills for knowledge that should influence how the agent approaches a class of task. Use memory for values the agent needs to look up during task execution.
