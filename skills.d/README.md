# Your skills and roles

Drop your own skills and roles here. Nine seeds this directory into its store on
every boot, alongside the skills built into the binary.

```
skills.d/
  deploy-checklist.md      a knowledge skill
  roles/
    data-wrangler.md       a role (a skill with a `role:` block)
```

This directory is the **source of truth**: edit a file and restart to update the
skill, delete the file to remove it. Discovery happens at boot only — there is no
watcher, so a restart is required to pick up changes.

Enable it in `nine.toml`:

```toml
[skills]
user_dir = "./skills.d"
```

This directory ships empty. A skill is seeded into the agent's store and becomes
instructions it can act on, so what lives here should be what you put here.

Under Docker the path `/skills.d` is wired (`NINE_SKILLS_USER_DIR`) but **not
mounted** — add `-v /my/skills.d:/skills.d:ro` to the run to seed your own.

## A skill

A skill is markdown with YAML-style frontmatter. The body is the instructions; the
description is what Nine sees when deciding whether to read it.

```markdown
---
name: deploy-checklist
description: Steps to verify before shipping a release build.
tags: [ops, release]
---

# Deploy checklist

1. Run the full test suite.
2. Tag the version.
3. Watch error rates for 15 minutes.
```

- `name` — lowercase letters, digits, single hyphens. Defaults to the filename.
  It **must not** match a built-in skill; built-ins win and your file would be
  skipped.
- `description` — **required**, under 200 characters. This is what shows in
  `skill_list` and what gets embedded for retrieval, so a skill without one is
  effectively invisible.
- `tags` — optional.

## A role

A role is a skill with a `role:` block, placed under `roles/`. The body becomes the
role's persona and the block sets its tool boundary.

```markdown
---
name: data-wrangler
description: Clean and reshape datasets — files and shell, no network.
tags: [role, data]
role:
  tools: [shell, read_file, write_file, list_files]
  delegates: false
---

You clean and reshape data. Work only in the workspace directory, and never
modify a source file without being asked.
```

- `tools` — an allowlist, or `"*"` for the full toolset. Omitting the key means
  `"*"`. A list can only ever *narrow* what Nine can do; it can never grant a tool
  the daemon does not already have.
- `delegates`, `persists`, `spawns_goals`, `interactive`, `profile` — structural
  flags. Yours are honored (you wrote the file, same as editing `nine.toml`);
  roles Nine writes for itself are restricted to tool narrowing only.
- The body may be empty, which means "use the daemon's default system prompt".

Your roles are advertised to the model as delegation targets, so a well-written
`description` is what makes Nine pick the right one.

## Check before you restart

```sh
nine skills validate            # checks the configured user_dir
nine skills validate path/to/one-skill.md
```

Invalid files are skipped at boot with a logged reason — the daemon still starts,
so a typo here can't take Nine down. `validate` just tells you before you find out
from the logs.

(This README is ignored by the loader.)
