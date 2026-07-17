---
name: skill-management
description: How to create, read, update, and organise skills
tags: [skills, knowledge, documentation]
---

## Skill Management

Skills are markdown files injected into the context when semantically relevant. Manage them with `skill_list`, `skill_read`, `skill_write`, and `skill_modify`.

### Listing skills

```
skill_list({})
# → [{name, description, tags}, ...]
```

### Reading a skill

```
skill_read({"name": "web-research"})
# → full markdown content including frontmatter
```

### Creating a new skill

```
skill_write({
    "name": "docker-usage",
    "description": "How to build, run, and debug Docker containers",
    "tags": ["docker", "containers", "deployment"],
    "content": "## Docker Usage\n\n### Building images\n..."
})
```

The file is saved to `<skills-dir>/<name>.md`.

### Updating a skill

```
skill_modify({
    "name": "docker-usage",
    "content": "## Docker Usage\n\n### Building images\n...\n\n### New section\n..."
})
```

Only the fields you provide are updated; omit `description` or `tags` to keep them unchanged.

### Writing good skills

- **Name**: kebab-case, descriptive (`git-workflow` not `git`)
- **Description**: one sentence, starts with a verb or noun phrase; this is what the semantic search sees
- **Tags**: 3-6 relevant terms for filtering
- **Content**: concrete, actionable — show examples, not just principles
- **Length**: 200-600 words is ideal; longer skills may be trimmed by the context budget

### When to create a skill

- You find yourself explaining the same thing repeatedly
- There's a multi-step procedure with easy-to-forget details
- A tool or system has non-obvious quirks worth documenting
- The user asks you to remember how to do something
