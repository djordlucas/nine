---
name: shell-usage
description: Best practices for using the shell tool safely and effectively
tags: [shell, bash, commands, safety]
---

## Shell Tool Usage

The `shell` tool runs commands via `sh -c`, **in the workspace** — a relative
path means the same file here as it does to `read_file` and `write_file`. Use
it for build tasks, git, pipelines, and anything the other tools don't cover.

**Reach for the file tools first for file work.** `read_file`, `write_file`,
`edit_file` and the rest keep large files out of your context and put deletions
in a recoverable trash; `rm` in a shell destroys outright. See the
`file-management` skill.

### Basic usage

```
shell({"command": "ls -la /tmp"})
shell({"command": "git log --oneline -10"})
shell({"command": "go test ./...", "timeout": 60})
```

Output is returned as `{"stdout": "...", "stderr": "...", "exit_code": 0}`.

### Timeout

Default timeout is 30 seconds. For long-running commands, set `timeout` explicitly (in seconds):
```
shell({"command": "go build ./...", "timeout": 120})
```

### Chaining commands

Use `&&` to stop on first failure, `;` to always continue:
```
shell({"command": "cd /tmp && mkdir -p work && ls work"})
```

### Capturing output

Check `exit_code` before using `stdout`. Non-zero exit means the command failed:
```json
{"stdout": "", "stderr": "command not found", "exit_code": 127}
```

### Safety rules

The following commands are **blocked by default** and will return an error:

| Category | Blocked examples |
|---|---|
| Recursive deletion | `rm -rf`, `rm -r`, `rm --recursive` |
| Secure deletion | `shred` |
| Disk operations | `dd of=/dev/…`, `mkfs`, `fdisk`, `parted`, `diskutil erase` |
| System control | `shutdown`, `reboot`, `halt`, `init 0/6`, `systemctl stop/disable` |
| Privilege escalation | `sudo`, `su` |
| Shell injection | `… \| bash`, `… \| sh`, `bash <(…)` |
| Destructive git | `git push --force`, `git reset --hard`, `git clean -f` |
| System file writes | `> /etc/passwd`, `> /dev/…` |

To bypass all checks (use with care): set `NINE_SHELL_UNSAFE=1` in the environment.

- Prefer relative paths; know the working directory before acting on it
- For long output, pipe to `head`, `tail`, or `grep` to limit size

### Common patterns

```bash
# Find files
find . -name "*.go" -not -path "./vendor/*"

# Check if something exists
[ -f ./nine.toml ] && echo "exists" || echo "missing"

# Get current directory
pwd

# Check git status
git status --short

# Run tests with coverage
go test -mod=vendor -coverprofile=coverage.out ./...
```
