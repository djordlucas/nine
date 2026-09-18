# Skills

Skills are markdown how-to notes describing reusable knowledge or procedures. When a skill is semantically relevant to the current task, its name is surfaced into the agent's self-model so it can read the full skill on demand — acting as on-demand documentation or playbooks.

---

## What skills are

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

## Where skills live

Skills are stored in the **memory store** (the `skills` table), not on the filesystem. There are three kinds:

| Kind | Source | Mutable at runtime? |
|---|---|---|
| **Built-in** | The `.md` files in the repo's `skills/` directory, embedded into the `nine` binary at build time | **No** — immutable |
| **User** | Your own `.md` files in `[skills].user_dir` | **No** — the file is the source of truth |
| **Agent-authored** | Written by Nine via `skill_write` / `skill_modify` | Yes |

Built-in and user skills are both **seeded into the store on every boot** from their files, which are their single source of truth: edit a file and restart to update it, delete it to prune it. Agent-authored skills persist in the store across restarts and are never touched by the seeder.

This split is deliberate: curated skills should not drift on a running instance (which could cause inconsistent behaviour across sessions), so they change only by editing their file. Nine remains free to capture what it learns in its own skills — but it cannot edit yours.

---

## Your own skills and roles

Point `[skills].user_dir` at a directory and Nine seeds it alongside the built-ins:

```toml
[skills]
user_dir = "./skills.d"
```

```
skills.d/
  deploy-checklist.md      a knowledge skill
  roles/
    data-wrangler.md       a role (a skill with a `role:` block)
```

The format is identical to a built-in — see [`skills.d/README.md`](../skills.d/README.md) for annotated examples of both, and [roles.md](roles.md) for what a `role:` block can declare.

**How it behaves:**

- **Boot-only.** There is no watcher; restart to pick up changes.
- **Files are authoritative.** Deleting a file removes the skill on the next boot. Only user skills are pruned — built-in and agent-authored ones are untouched. An unconfigured or missing directory prunes nothing.
- **Built-ins win.** A user skill may not take a built-in's name; the collision is logged and the file skipped.
- **Bad files are skipped, not fatal.** A file that fails validation is logged with the reason and ignored, so one typo cannot stop the daemon from starting.
- **Nine cannot edit them.** `skill_write` and `skill_modify` refuse user skills the same way they refuse built-ins.
- **Your roles are trusted.** Structural flags (`persists`, `delegates`, `spawns_goals`, `interactive`, `profile`) are honored in your role files, exactly as in built-ins — you wrote the file, the same as editing `nine.toml`. Roles Nine writes for *itself* are restricted to narrowing tools. Either way a `tools` list can only narrow; it never grants a tool the daemon lacks.

Check files before restarting:

```bash
./nine skills validate                        # the configured user_dir
./nine skills validate skills.d/my-skill.md   # a single file
```

It runs the same validation the daemon runs at boot and reports every problem per file, so a file that passes here is a file that will seed. In Docker the path `/skills.d` is wired (`NINE_SKILLS_USER_DIR`) but not mounted: add `-v /my/skills.d:/skills.d:ro` to seed your own, so the container runs the skills you chose rather than whatever the repo carried.

---

## Tools

| Tool | Effect |
|---|---|
| `skill_list` | List all skills (built-in and agent-authored) with names, descriptions, tags |
| `skill_read` | Read a skill's full content by name |
| `skill_write` | Create or replace one of Nine's own skills (refuses built-in and user skills) |
| `skill_modify` | Update one of Nine's own skills (refuses built-in and user skills) |

These are **core-intercepted** tools (handled in-process against the memory store), not a plugin subprocess.

```bash
./nine "List all available skills"
./nine "Show me the contents of the git-workflow skill"
./nine "Create a skill called 'python-testing' with best practices for pytest and fixtures"
```

Attempting to overwrite or modify a built-in skill returns an error directing the change to the repo + rebuild; the same applies to a user skill, whose file is its source of truth.

---

## Adding or editing a built-in skill

Edit (or add) a `.md` file in the repo's `skills/` directory and rebuild. On the next boot the seeder upserts it into the store and embeds its description. There is no runtime path to change a built-in skill.

---

## How skills affect the agent

Two mechanisms.

**Passively**, relevant skill *names* are injected into the self-model block (context priority 5 — dropped first under budget pressure). Names only: the body is never preloaded. For complex turns with long histories the hint may be trimmed away entirely.

**Actively**, the system prompt tells the agent to consult skills before a task with an established procedure — `skill_search` with a short description (or `skill_list` where no embedder is configured, since `skill_search` is embedder-gated), then `skill_read` the hit. This is default behaviour, not something a caller has to ask for.

The active path exists because the passive one is not enough on its own. A name is not a procedure, and priority 5 is the first thing dropped under budget pressure — so on exactly the long, complex turns where a skill would help most, the hint is likeliest to be gone. It also carries knowledge the tool descriptions cannot: which tools *this* deployment has for a job, and what to do when it lacks them. `web-research` is the worked example — it branches on whether a browser MCP server is loaded (see [browser.md](browser.md)).

If a skill is critical, you can still name it explicitly:

```bash
./nine "Using the git-workflow skill, help me write a commit message for these changes: ..."
```

---

## Skills vs. memory

| | Skills | Memory (KV store) |
|--|--------|------------------|
| Storage | `skills` table | `kv` table |
| Retrieval | Semantic (embedding similarity) | Exact key lookup |
| Scope | Surfaced into context automatically | Explicitly fetched with `memory_get` |
| Best for | Reusable procedures, guidelines | Per-session state, dynamic data |

Use skills for knowledge that should influence how the agent approaches a class of task. Use memory for values the agent needs to look up during task execution.

---

## Limits

| Limit | Detail |
|-------|--------|
| Built-in skills are immutable at runtime | `skill_write` and `skill_modify` refuse built-in and user skills. Changing a built-in means editing `skills/*.md` and rebuilding; a user skill's file is its source of truth. |
| The passive hint is dropped first | Relevant skill names ride at context priority 5, the first thing trimmed under budget pressure. On long, complex turns — where a skill helps most — the hint is likeliest to be gone. The active `skill_search` path exists to cover that. |
| Names only, never bodies | The passive path injects skill names. A body reaches context only through an explicit `skill_read`. |
| `skill_search` needs an embedder | It is embedder-gated. With no embedder configured, use `skill_list`. |
| No versioning or history | A `skill_modify` replaces the content. There is no revision history and no way to diff or roll back. |
