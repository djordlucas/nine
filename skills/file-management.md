---
name: file-management
description: The workspace file tools — read, edit, write, list, search, diff, and recover files without spawning a shell
tags: [files, workspace, read, write, edit, trash]
---

## Workspace files

Eleven tools operate on files, and `edit_file` is the one to reach for first: it
changes part of an existing file without ever loading it into your context, so
it works on a file larger than your context window. Reach for `write_file` only
to create a file or replace one whole.

Paths resolve under the workspace root (`/work`); a relative path is taken as
relative to it. Nothing outside the workspace is reachable — a path outside it
is a mount the operator adds, not an authority a tool can reach around.

| Goal | Tool |
|---|---|
| Change part of an existing file | `edit_file` |
| Create a file, or replace one whole | `write_file` |
| Read a file, or a range of lines | `read_file` |
| Find a file by path or glob | `list_files` |
| Find a file by its contents | `file_search_text` |
| Show what you changed | `diff_file` |
| Rename or relocate | `move_file` |
| Duplicate | `copy_file` |
| Remove, recoverably | `delete_file` |
| See what is recoverable | `trash_list` |
| Undo a delete or an overwrite | `restore_file` |

### Editing

`old_text` must match the file byte for byte, indentation and line breaks
included. Read the region first with `line_numbers` so you can name the exact
text:

```
read_file({"path": "internal/agent/loop.go", "lines": "120-180", "line_numbers": true})

edit_file({
    "path": "internal/agent/loop.go",
    "old_text": "\tif err != nil {\n\t\treturn err\n\t}",
    "new_text": "\tif err != nil {\n\t\treturn fmt.Errorf(\"step: %w\", err)\n\t}",
    "expect": 1
})
```

`expect` is how many occurrences to replace — a positive integer (default 1) or
`"all"`. The call fails and changes nothing when the count does not match, so a
too-short `old_text` is caught rather than applied in the wrong place. Include
enough surrounding lines to make the match unique.

Pass `"preview": true` to either `edit_file` or `write_file` to get the diff the
call would make without writing anything.

### Reading

```
read_file({"path": "nine.toml"})                              # whole file
read_file({"path": "server.log", "lines": "1-40"})            # a line range
read_file({"path": "big.json", "offset": 0, "limit": 4096})   # a byte window
```

Prefer `lines` over `offset`/`limit` for text. A windowed read also returns a
version token; pass it to `write_file` as `if_unchanged` and the write fails if
the file moved under you.

A `spill/...` path from a truncated tool result is read with this same tool.

### Writing

```
write_file({"path": "notes/summary.md", "content": "# Summary\n"})
write_file({"path": "run.log", "content": "done\n", "mode": "append"})
```

Parent directories are created as needed. `mode` is `replace` (default) or
`append` — appending never reads the file, so it costs nothing on a large one.

To copy a large payload — a spilled tool result, typically — pass `content_ref`
with its path instead of `content`, and the bytes never pass through your
context.

### Finding files

```
list_files({"pattern": "*_test.go"})                 # glob over the whole path
list_files({"prefix": "internal/agent"})             # one directory
list_files({"changed_since": "2026-09-25T00:00:00Z"})# what appeared or changed

file_search_text({"query": "delegation depth", "path": "docs/"})
```

`changed_since` sees files created outside Nine — by a `git pull`, or dropped in
by a person. `file_search_text` is full-text over stored content, and `path`
narrows it to one file or prefix, which is how you find the relevant region of a
large spilled tool output.

### Moving, copying, deleting

```
move_file({"from": "draft.md", "to": "notes/draft.md"})
copy_file({"from": "config.toml", "to": "config.toml.bak"})
delete_file({"path": "notes/draft.md"})
```

`move_file` relinks and `copy_file` streams, so neither spends context on the
file's contents and neither is bounded by its size. Both refuse an existing
destination unless `"overwrite": true`.

`delete_file` moves the file to the trash instead of destroying it, and handles
one file or one empty directory at a time. Prefer it over `rm` in the shell,
which destroys outright.

### Recovering and reporting

```
trash_list({"path": "draft"})                        # newest first
restore_file({"entry": "20260920T143015Z-1f2e3d4c"})
diff_file({"path": "notes/summary.md"})
```

The trash holds every file `delete_file` removed *and* every version
`write_file` or `edit_file` replaced, so `diff_file` can show a unified diff of
what you just changed. Report an edit with `diff_file` rather than describing it
in prose. `restore_file` never overwrites: recovering one file by destroying
another is not a recovery.

### Files versus shell

Use the file tools. They are confined to the workspace, they keep large files
out of your context, and their mistakes are recoverable from the trash. Reach
for `shell` only for what they do not cover — running a build, piping through a
program, or a recursive operation.

## Limits

| Limit | Detail |
|---|---|
| Workspace only | These tools read and write nothing outside the workspace mount. Files elsewhere need an operator-added mount. |
| No recursive delete | `delete_file` takes one file or one empty directory. Delete a tree's contents first. |
| Trash is swept | The daemon bounds the trash by age and size, so `restore_file` works until the sweep, not forever. |
| `diff_file` needs a prior version | A file Nine has never changed has nothing to diff against. |
| Exact-match editing only | `edit_file` matches literal text, not patterns. A near-miss fails rather than guessing. |
