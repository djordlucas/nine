---
name: git-workflow
description: Best practices for git branching, committing, and reviewing changes with the shell tool
tags: [git, version-control, workflow, commits]
---

## Git Workflow

Use the `shell` tool to run git commands. Always check status before making changes.

### Before starting work

```bash
git status          # check for uncommitted changes
git log --oneline -5  # review recent history
git diff            # see what changed
```

### Committing changes

```bash
git add <specific-files>   # prefer explicit paths over git add -A
git commit -m "Add feature X to solve Y"
```

Commit message rules:
- Imperative mood: "Add", "Fix", "Remove" — not "Added"
- Subject line under 72 characters
- Describe *why*, not just *what*

### Branching

```bash
git checkout -b feature/my-feature   # create and switch
git branch -a                         # list all branches
git checkout main                     # switch back
```

### Reviewing changes

```bash
git diff HEAD           # all uncommitted changes
git diff main...HEAD    # changes since branching from main
git show <commit>       # show a specific commit
git log --oneline --graph --all   # visual branch history
```

### Safe operations

These are always safe to run:
- `git status`, `git log`, `git diff`, `git show`
- `git branch`, `git stash list`

These are **blocked by the shell guard** and return an error rather than
running — do not plan around them:
- `git reset --hard`
- `git push --force`
- `git clean -f`

These are destructive but not blocked — confirm with the user first:
- `git checkout -- .`, `git branch -D`, `git stash drop`

See the `shell-usage` skill for the full blocklist.

### Undoing mistakes

```bash
git revert <commit>     # safe undo (adds a new commit)
git stash               # temporarily shelve changes
git stash pop           # restore stashed changes
```
