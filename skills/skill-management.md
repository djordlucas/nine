---
name: skill-management
description: Writing and finding your own skills — skill_search, skill_read, skill_write, skill_modify, and which skills you may not touch
tags: [skills, knowledge, skill_write, skill_search, documentation]
---

## Managing your skills

Your own skills are **rows in the store, not files**. `skill_write` persists a
skill into the database, where it survives restarts and is never touched by the
boot seeder. Do not go looking for a file afterwards; none is written.

| Tool | Use |
|---|---|
| `skill_search` | Find a skill by description when you do not know its name |
| `skill_read` | Read one skill's full text, by name |
| `skill_list` | Every skill — names, descriptions, tags |
| `skill_write` | Create or replace one of **your own** skills |
| `skill_modify` | Update part of one of your own skills |

### Only three names reach you passively

The names of the **three** most relevant skills are injected into your
self-model each turn, ranked by similarity to the current message. **Names
only — a skill's body is never preloaded.** That block is capped and dropped
when the context budget is tight, which is likeliest on exactly the long,
complex turns where a skill would help most.

So do not treat the skills you can see as the skills that exist:

```
skill_search({"query": "how do I delegate a research task"})
skill_read({"name": "delegation"})
```

`skill_list({})` gives the whole catalog when you need the inventory rather
than the best matches.

### Which skills you may change

| Source | Comes from | You may modify it |
|---|---|---|
| Built-in | `.md` files embedded in the binary | **No** |
| User | `.md` files in the operator's `[skills].user_dir` | **No** |
| Agent | Written by you with `skill_write` | Yes |

Built-in and user skills are reseeded from their files on every boot, so a
write would be silently undone. Both `skill_write` and `skill_modify` refuse
them outright: *"is a builtin skill and cannot be overwritten; it is owned by
its source file."* If you need a built-in to say something different, that is a
change to its file — tell the human, do not try to write over it.

### Creating a skill

```
skill_write({
    "name": "docker-usage",
    "description": "Build, run, and debug Docker containers — the flags that matter and the failure modes",
    "tags": ["docker", "containers", "deployment"],
    "content": "## Docker usage\n\n### Building images\n..."
})
```

`name`, `description` and `content` are all required. Writing an existing name
of your own replaces it.

**The description is the whole skill, as far as retrieval is concerned.** It is
what gets embedded into the `skills` vector namespace, what `skill_search`
matches, and what `skill_list` shows. A skill whose description does not
describe the situation it answers is a skill you will never find again. Write
it for a reader who has forgotten the context, keep it under about 200
characters, and name the problem rather than the topic.

### Updating a skill

```
skill_modify({
    "name": "docker-usage",
    "content": "## Docker usage\n\n...\n\n### New section\n..."
})
```

Fields you omit keep their existing values, so this updates content alone. An
empty string counts as omitted — `skill_modify` cannot blank a field, only
replace it.

### When to write one

- You have explained the same procedure more than twice.
- A multi-step process has details that are easy to get wrong and cheap to
  record.
- A tool or system has a non-obvious quirk you had to discover.
- The human asks you to remember how to do something.

Do not write a skill for something you did once, and do not write one that
restates a built-in. A large catalog of near-duplicates makes every skill
harder to rank, including the good ones.

## Limits

| Limit | Detail |
|---|---|
| The passive hint is three names, and droppable | It rides in the self-model block, capped and omitted when the budget is tight. `skill_search` is the route that always works. |
| Bodies are never preloaded | A skill reaches you whole through `skill_read` or not at all. Length costs nothing until you read it. |
| Built-in and user skills are immutable | Both are owned by their source file and reseeded every boot. `skill_write` and `skill_modify` refuse them. |
| `skill_modify` cannot clear a field | An omitted or empty field keeps its old value. Replace it with new text instead. |
| No delete | There is no `skill_delete`. A skill of yours that is wrong gets overwritten, not removed. |
| `skill_search` needs an embedder | Without one there is no ranking; `skill_list` is the only route to the catalog. |
