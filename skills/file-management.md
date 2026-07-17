---
name: file-management
description: Reading and writing files using the files plugin tools
tags: [files, read, write, filesystem]
---

## File Management

Use `file_read` and `file_write` for direct file access without spawning a shell.

### Reading files

```
file_read({"path": "/path/to/file.txt"})
# → full file contents as a string

file_read({"path": "/path/to/file.txt", "max_bytes": 4096})
# → first 4096 bytes (useful for large files)
```

### Writing files

```
file_write({
    "path": "/path/to/output.txt",
    "content": "Hello, world!\n"
})
# → creates or overwrites the file
```

`file_write` creates parent directories automatically.

### When to use files vs shell

| Task | Prefer |
|------|--------|
| Read a config file | `file_read` |
| Write a small file | `file_write` |
| List directory contents | `shell` with `ls` or `find` |
| Copy / move / delete files | `shell` |
| Append to a file | `shell` with `>>` |
| Read a huge log file | `shell` with `tail -n 100` |

### Common patterns

```
# Read a Go source file
file_read({"path": "./internal/agent/loop.go"})

# Write a new skill file
file_write({
    "path": "./skills/my-skill.md",
    "content": "---\nname: my-skill\n...\n---\n\n## Content\n"
})

# Read nine config
file_read({"path": "./nine.toml"})
```

### Editing existing files

To edit a file: read it, modify the content in memory, write it back. For surgical edits, use the shell tool with `sed` or write a small script.
