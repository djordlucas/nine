# Design note — The workspace as the agent's filesystem

- **Status:** Implemented, phases 1–8 (PRs #162, #163, #164, #165, #166, #168,
  #170, #171). Document extraction (§6) was dropped from phase 6 and is unbuilt.
  Three places where the implementation diverged from this note are corrected
  in place below, marked **As built**.
- **Date:** 2026-09-20. Corrected 2026-09-21, after implementation.
- **Depends on:** `tool-output-spill.md`, `rich-js-tools.md` (§6.4, why `nine:fs`
  is libc rather than host functions), `spec/contracts/memory-store.md` (R-MEM.2,
  R-MEM.3, R-MEM.9), `spec/contracts/dispatcher.md` (R-DISP.2),
  `spec/contracts/hitl.md` (R-HITL.5), `spec/contracts/toolvm.md`, the shipped
  sandboxed tools (`internal/toolvm/shipped.go`).
- **Amends:** R-MEM.2 and R-MEM.3 (the `files` table stops holding agent-authored
  files), R-MEM.9 (the `spill/` write refusal moves from `file_store` to
  `write_file`), R-DISP.2 (the truncation notice names `read_file`), R-HITL.5 (a
  gated file tool's `ask_human` carries a diff), the `nine:fs` module (gains
  `remove`, `rename`, `copy`, ranged reads and appends).

The agent sees three file namespaces, and choosing the wrong one is the most
common model error recorded in `tool-output-spill.md`. This note proposes one
namespace for everything the agent reads and writes — the workspace — with
truncated tool output as the single read-only exception:

1. **Agent-written files live in the workspace only.** `file_store`, `file_list`
   and `file_fetch` are retired. The agent's file tools become `read_file`,
   `write_file`, `edit_file`, `move_file`, `copy_file`, `delete_file`,
   `list_files`, `file_search_text`, `diff_file`, `trash_list` and
   `restore_file` (§10).
2. **Spilled tool output stays in the `files` table**, agent-readable and never
   agent-writable, addressed as `spill/...` exactly as today (§3).
3. **`read_file` reads both.** The dispatcher serves a `spill/...` path from the
   store and passes every other path to the sandboxed tool.
4. **`shell` runs in the workspace root**, so a relative path means the same file
   to every tool (§4).
5. **The index is derived from the filesystem, never from Nine's own writes**, so
   a bind-mounted directory, a `git pull` and a file an operator drops in are all
   visible to search and listing (§6).
6. **`edit_file` changes part of a file without rewriting it**, so a file larger
   than the context window is still editable (§7).
7. **Every change is reviewable as a diff** — before it happens, after it
   happened, and inside an approval prompt (§8).
8. **`delete_file` moves a file to `.nine/trash/` inside the workspace**, swept on
   a retention window, and `restore_file` brings one back. `write_file` and
   `edit_file` move the previous version there too (§9).

---

## 1. The three namespaces

| Namespace | Tools | Path form | Backing |
|-----------|-------|-----------|---------|
| Memory file store | `file_store`, `file_fetch`, `file_list`, `file_search_text`; the spill sink; job output (`internal/runtime/jobs.go` `storeResult`) | `notes/x.md`, `spill/<agent>/<tool>-<hex>.txt` | `files` table with the `files_fts` FTS5 index kept in sync by triggers |
| Workspace | `read_file`, `write_file` (sandboxed, `fs` grant) | `/work/...` or relative to it | `[workspace].root` on the host: `/data/workspace` in the container, `./dist` in the repo's `nine.toml` |
| Shell | `shell` (`internal/builtins/shell.go`) | Host paths | The host filesystem. `runShell` sets no `cmd.Dir`, so relative paths resolve against the daemon's working directory, and `/work` does not exist on the host |

## 2. What the split costs today

- **Model errors.** `tool-output-spill.md` §"Empty results must explain
  themselves" records models passing `/work/audit.txt` to `file_search_text`
  after the file was spilled, and llama3.1:8b inventing a value when the search
  came back empty.
- **Recovery code.** `missingStorePath`, `noSearchHitsMessage` and
  `MissingStorePathError` exist only to explain the namespace to a model that
  picked the wrong one.
- **Tool descriptions.** `file_store`, `write_file`, `read_file` and the
  `content_ref` property each spend a sentence saying which namespace they are
  not.
- **A copy step.** Spilled output reaches the workspace only when the model first
  copies it with `file_store(content_ref=...)`, which is what
  `tests/evals/cases/spill-ref-passing.yaml` exercises.
- **Files invisible to the operator.** Files written with `file_store` exist only
  as SQLite rows. An operator cannot open, `grep` or `git`-track them, and
  neither can the agent's own `shell`.

## 3. Why spilled output stays in the database

| Property | Where it comes from | Lost if spills moved to the workspace |
|----------|---------------------|---------------------------------------|
| A spill is exactly what the tool returned (R-MEM.9) | `file_store` refuses `spill/`; nothing else writes the table | Yes: `shell`, `write_file` and `edit_file` can write anywhere under the root |
| `nine trace` resolves a spill named in the journal | The trace reads the same database the journal lives in | Yes: the trace would need the workspace directory as well as the database |
| Retention | `FileDeleteOlderThan(spill/, 7d)`, hourly | No, but a directory sweep replaces one statement |
| Windowed reads without loading the whole file | `substr` in SQL (`FileFetchRange`) | No; a ranged read works on a file too |

The first two hold only while the table stays out of the agent's write reach, so
spills stay in the database.

## 4. The workspace is the boundary

Everything Nine writes on behalf of an agent goes under `[workspace].root`, and
nothing it writes goes anywhere else. The root is the operator's directory,
frequently a bind mount of a directory that already holds their data, and a path
outside it is a path they did not offer.

Three consequences:

- **`shell` runs with `cmd.Dir` set to the root** and `NINE_WORKSPACE` in its
  environment. A relative path then means the same file to `shell`, `read_file`,
  `write_file` and `edit_file`. Today `runShell` sets no directory, so relative
  paths resolve wherever the daemon was started, and `/work` does not exist
  outside the sandbox. **As built:** confirmed at the plugin boundary rather than
  against a running daemon — `TestShellRunsInWorkspace` starts the `shell`
  built-in with `NINE_WORKSPACE` set and asserts `pwd -P` and a relative `cat`
  resolve in the workspace.
- **The trash lives inside the workspace** (§9), not in a sibling directory.
- **Nine's own bookkeeping in the workspace lives under `.nine/`**: the trash,
  and a `.gitignore` keeping all of it out of a repository the operator mounted.

The sandboxed tools are confined by wazero's pre-open, which is one mount: the
root. `shell` is a subprocess holding the daemon's own authority, so its working
directory is a default, not a confinement — see Limits.

**A write budget.** `[workspace].max_bytes` (default unset) bounds what the agent
may add to the root. Checked before a write, it fails with an error naming the
budget rather than filling the operator's volume, which a loop that writes on
every turn will otherwise do.

## 5. What agent-written files lose when they leave the database

| Property today | Replacement |
|----------------|-------------|
| Full-text search via `files_fts` triggers | A derived contentless index over the workspace root (§6) |
| Windowed reads (`file_fetch` `offset`/`limit`) | `read_file` gains `offset` and `limit` |
| Copy by reference (`file_store` `content_ref`) | `write_file` gains a `content_ref` property marked `x-nine-ref`. `RegisterSandboxed` already indexes ref-marked properties (`dispatcher.go`) |
| Copy from one path to another | `copy_file`, which streams host-side so the bytes never enter context |
| Semantic search (`file_search_semantic`) | None needed. The tool queries the `files` vector namespace, which nothing writes, so it can only return empty results. Delete it |
| Durability | Unchanged: the workspace and `nine.db` are on the same `/data` volume |
| Session deletion | Unchanged: `files` rows were never part of the R-MEM.11 cascade |

## 6. Files that change outside Nine

The workspace is shared with the operator and with whatever they run in it. A
`git pull` adds a thousand files, an operator drops a PDF in for analysis, a
build writes a directory of artifacts, and a bind mount arrives already full.
The index is therefore **derived from the filesystem**, and no write through a
Nine tool is treated as the authoritative record of what exists.

| Mechanism | Behavior |
|-----------|----------|
| Boot scan | The daemon walks the root at startup, in the background. A search issued before the first scan finishes says so in its result rather than reporting fewer hits as none |
| Periodic scan | A sweep on `[workspace].scan_interval` (default 60s) reindexes files whose `(mtime, size)` changed, adds files it has not seen, and drops rows for files that vanished |
| Scoped refresh | `file_search_text` and `list_files` refresh the subtree named by their prefix argument before answering, so a file written seconds ago by `shell` or by a `git pull` is already there |
| Change reporting | Index rows carry `first_seen` and `last_changed`. `list_files` takes `changed_since`, so an agent can ask what appeared since its last turn without diffing a listing itself |
| Vanished paths | `read_file` on an indexed path that no longer exists says the file was removed outside Nine, rather than only "not found" |

**The index holds no copy of the workspace.** `files_fts` is external-content
over `files`, which is correct there because the store *is* the text's home.
Mirroring that for the workspace would put a second copy of every indexed file
in `nine.db`, doubling the disk a repository costs. The workspace index is
therefore **contentless** (`content=''`): postings only, with the text read from
disk at query time to build each snippet. Snippets then reflect the file as it is
now rather than as it was indexed.

Deleting postings for a changed or removed file needs `contentless_delete=1`
(SQLite 3.43+), since a contentless table cannot reconstruct the old text to
retract it. The driver is `modernc.org/sqlite v1.56.0`; the bundled SQLite
version must be confirmed before this phase starts.

**What the scan skips:** `.git/` and `.nine/`; content that is not valid UTF-8;
anything matched by a `.gitignore` in the root, when one exists; and files over
`[workspace].index_max_file_bytes` (default 8 MiB).

The size ceiling bounds **churn**, not storage — contentless indexing already
removed the storage cost. FTS5 indexes a row as one document with no way to
append to it, so a file that grows retracts and retokenizes its whole contents on
every scan that sees a new `(mtime, size)`. A 40 MiB log gaining a line a minute
costs 40 MiB of tokenization a minute, forever. 8 MiB covers source, documentation
and configuration, the files where searching the whole workspace pays; text above
it is almost always data whose tokens rank poorly and whose churn is high.

**A named file is searched whether or not it is indexed.** When
`file_search_text`'s `path` argument resolves to an unindexed file, the tool scans
that file directly from disk in ranged reads rather than consulting the index. The
ceiling therefore decides what is searchable *without being asked*, not what is
reachable: a 200 MiB log dropped in for analysis is still searchable by naming it,
which is how scoping a search into one spill already works today.

**Skipping is never silent.** A skipped file still appears in `list_files` with
its size and `indexed: false`, and a search reports how many files under its
prefix were skipped. The same holds at the scan bound
`[workspace].index_max_files` (default 50,000): the scan stops and says so in
search results. A partial index presenting itself as complete is the failure mode
to avoid, because the agent reads "no matches" as "not there".

### Documents that are not text

A dropped PDF is the motivating case for this section, and byte-level it is not
UTF-8: unindexed by the rule above, and — worse — `read_file` today decodes bytes
with a non-fatal `TextDecoder` (`internal/toolvm/shipped/read_file.js`), so
reading one returns replacement characters that a model may treat as content.

Two changes:

- **`read_file` refuses non-UTF-8 content**, naming the file as binary and giving
  its size and type. An error the model can act on beats mojibake it cannot
  detect.
- **The scanner extracts text from known document formats** into
  `.nine/extracted/<path>.txt`, which is indexed and readable in its place. The
  extraction is a rendition, not the file: `list_files` shows the original with
  `extracted: true`, and a search hit names the original path.

Extraction runs as a sandboxed tool under the same capability rules as any other,
so a format Nine cannot parse is a missing tool rather than a special case in the
daemon. Anything it cannot read lists with `indexed: false` and a reason.

**As built: only the first of the two shipped.** `read_file` refuses non-UTF-8
content. Extraction does not exist: it was dropped from phase 6 when that change
was already large, and parsing PDF inside the sandbox is a design of its own
rather than a detail of this one. A dropped PDF therefore lists with
`indexed: false` and a reason, and is not searchable — honest, but not the case
this section opens with. The `extracted` flag on `list_files` is unbuilt with it.

**Journal events are a later phase.** Emitting a session event per external
change would let a standing agent react to a dropped file
(`adr/reactive-events.md`), and the scanner is the natural producer. It is out of
scope here: `changed_since` covers the pull case, and the push case needs the
subscription design, not this one.

## 7. Editing a file

Nine has no edit tool. `write_file` replaces a file whole, so changing one line
of a 4 MB file means reading 4 MB into context and writing it back — impossible
past the context window, and lossy before it.

**`edit_file(path, old_text, new_text, expect)`** replaces exact text:

- `expect` defaults to 1. The call fails when the number of matches differs from
  `expect`, reporting the count found. No partial application, and no silent
  edit of the wrong occurrence.
- `expect: "all"` replaces every occurrence and reports how many.
- `preview: true` returns the diff and writes nothing (§8).
- The result names the path, the number of replacements, and the line numbers
  touched.
- It refuses a file that is not valid UTF-8, and any path under `.nine/`.

**Implementation.** The file is never materialized whole in the guest. The tool
scans it in overlapping windows (window ≥ 2× `old_text`, so a match cannot
straddle a boundary undetected), writes the result to a temporary file beside the
original, then renames the temporary over the original. Memory stays bounded by
the window, well inside the sandbox's limits.

That needs four additions to `nine:fs`, all ordinary libc calls in the existing
pre-open, which is what `rich-js-tools.md` §6.4 asks for — containment stays
wazero's rather than becoming a path check Nine owns:

| Addition | libc |
|----------|------|
| `readRange(path, offset, length)` | `fseek` + `fread` |
| `appendFile(path, data)` | `fopen(path, "ab")` |
| `rename(from, to)` | `rename` |
| `copy(from, to)` | `fread`/`fwrite` in a fixed buffer |

**Reading a region to edit it.** `read_file` gains `offset` and `limit` over
`readRange`, a `lines: "120-180"` range, and `line_numbers`. Models reason in
lines and `edit_file` reports line numbers, so a character-only window leaves the
model converting between two coordinate systems. The sequence is: locate with
`file_search_text`, read the region with `lines`, name exact text to `edit_file`.

**Appending.** `write_file(mode: "append")` adds to the end over `appendFile`.
Without it, adding a line to a log is a whole-file read, edit and rewrite.

**Concurrent edits.** `read_file` returns a `version` token (mtime and size).
`write_file` and `edit_file` accept `if_unchanged: <token>` and fail if the file
changed since that read, naming who to re-read. It is optional, so a single-writer
case stays a single call, and it makes two sub-agents editing one file a reported
conflict rather than a silent loss.

**Atomic replacement comes free.** Writing to a temporary and renaming means an
interrupted edit leaves the original intact, which `write_file` does not manage
today. `write_file` adopts the same sequence.

## 8. Reporting a change for review

A person supervising an agent needs to see what it changed, in the form they
already read changes in. Three points in the lifecycle produce a unified diff,
all built from the `nine:diff` module that sandboxed tools already have
(`internal/toolvm/stdlib/diff.js`).

| When | Mechanism | Who reads it |
|------|-----------|--------------|
| Before the write | `edit_file(preview: true)` and `write_file(preview: true)` return the diff and change nothing | The model, checking its own targeting before committing to it |
| At an approval gate | A tool named in `[hitl].require_approval` has its auto-generated `ask_human` carry the diff instead of raw JSON arguments (R-HITL.5) | The human being asked to approve |
| After the write | `diff_file(path)` diffs the current file against its most recent `.nine/trash/` entry — the previous version, kept there by every overwrite, edit and delete (§9) | The model, reporting what it did; the human, reading that report |

`diff_file` costs nothing extra to store: the trash already holds the previous
version, so the "before" side is a file on disk, not a copy made for the purpose.
Passing `against: "trash:<entry>"` diffs an older version, and `list_files` with
`changed_since` plus `diff_file` per path is how an agent answers "show me
everything you changed this turn".

**Diffs are bounded.** `nine:diff` computes a longest-common-subsequence over
lines, which is exact and quadratic, and the sandbox's 5-second deadline
(R-TVM.4) bounds it. A diff over `[workspace].diff_max_bytes` (default 1 MiB per
side) is not computed; the result reports lines added and removed and the byte
delta instead. `edit_file`'s preview does not pay this at all: the tool knows the
match offsets, so it diffs a context window around each replacement rather than
the file.

An over-cap diff that the model asks for anyway is spilled to the store like any
large tool result (R-DISP.2), so the model reads the parts it needs by path.

## 9. Deletion, the trash, and restoring

`delete_file` removes one file or one empty directory. There is no recursive
delete, matching the shell guard's refusal of `rm -r`. It refuses the workspace
root, any path with a `.git` component, and anything under `.nine/`.

Deleting moves the file to `.nine/trash/`. So does overwriting with `write_file`
or `edit_file`, which destroy a file's contents as thoroughly as deleting it.

**Why a trash at all.** Approval gates arm only for interactive sessions
(R-HITL.5). Goal sessions, standing agents and their sub-agents run ungated, so a
mistaken delete in those runs has nobody to stop it. The trash makes it
recoverable instead.

| Aspect | Decision |
|--------|----------|
| Location | `<root>/.nine/trash/`, inside the workspace. Writing agent data outside the root would put it outside what the operator mounted, sized and backs up (§4) |
| Layout | One entry per deletion or overwrite: `.nine/trash/<UTC timestamp>-<4-byte hex>/<workspace-relative path>` |
| Cost of an entry | One `rename` within the mount. No copy, so trashing a large file is free and edits stay cheap |
| Sweep age | The timestamp in the entry name. A file's own mtime survives `rename` and records its last write, not its deletion |
| Sweep | The spill sweeper's hourly loop, against `[workspace].trash_retention` (default 7 days), plus `[workspace].trash_max_bytes` (default 1 GiB), oldest entry first. The workspace is the operator's disk, so the trash carries a size bound as well as an age bound |
| Git hygiene | The daemon creates `.nine/.gitignore` containing `*`, so a workspace that is a git repository is not dirtied by Nine's bookkeeping |
| Approval | Not gated by default. An operator can add `delete_file` to `[hitl].require_approval`, and the prompt carries the diff (§8) |

**Restoring.** `trash_list(path)` lists entries, newest first, with their
timestamps and sizes; `restore_file(entry, to)` renames one back, refusing to
clobber an existing file unless `to` names a free path. Both are scoped to
`.nine/trash/` and are the only agent access into `.nine/` — the rest of it stays
excluded from the index, from `list_files` and from every write tool.

Without these two, an agent that deletes the wrong file cannot recover it even
though the bytes are still there, which makes the trash a benefit only to an
operator watching at the time. The same pair is what makes `diff_file` (§8)
possible.

**Mechanism.** With the trash inside the mount, trashing is a `mkdir` plus a
`rename` in the pre-open — the same libc surface §7 already adds, and no new
host function.

**As built: `nine:fs remove` does not trash.** This note proposed that it always
would, for every caller, on the grounds that one rule beats a per-tool policy.
That was wrong in a way only writing it showed: `edit_file` uses `remove` to
clean up its own temporary when an edit fails, and any tool that writes scratch
files uses it the same way. Trashing at the primitive layer fills the trash with
build debris and buries the deletions someone might actually want back.

Trashing is therefore at the **tool** layer — `delete_file`, `write_file` and
`edit_file` each move the previous version aside before destroying it — and
`remove` is a plain unlink. The rule an agent sees is unchanged: every deletion
it can perform is recoverable. What changed is where the rule lives.

## 10. Tool surface after the change

| Tool | Change |
|------|--------|
| `read_file` | Adds `offset`, `limit`, `lines`, `line_numbers`, and a `version` token. Refuses non-UTF-8 content. The dispatcher serves `spill/...` from the store; other paths go to the sandbox |
| `write_file` | Adds `content_ref`, `mode: "append"`, `if_unchanged`, `preview`. Writes through a temporary and renames. Trashes the previous version. Refuses `spill/...` and `.nine/...` |
| `edit_file` | New. Exact-text replacement with `expect` and `preview` (§7) |
| `move_file` | New. Renames within the mount, so relocating a file costs no reads and no context. Refuses an existing destination unless `overwrite: true`, which trashes the destination first |
| `copy_file` | New. Streams host-side in a fixed buffer; the bytes never enter context |
| `delete_file` | New. One file or one empty directory, to the trash (§9) |
| `trash_list`, `restore_file` | New. List and recover trashed versions (§9) |
| `diff_file` | New. Unified diff of a file against its previous version or a named trash entry (§8) |
| `list_files` | New. Lists paths under a prefix, with `pattern` (glob over the indexed path column), `changed_since`, and per-entry `size` and `indexed`. An exact path returns one entry, which is how an agent stats a file. Replaces `file_list`. **As built: core-intercepted, not sandboxed** — see below |
| `file_search_text` | Searches the workspace index and spills. A `spill/` prefix searches spills; another prefix searches the workspace; no prefix searches both and labels each hit |
| `shell` | Runs with `cmd.Dir` set to the workspace root and `NINE_WORKSPACE` in its environment |
| `file_store`, `file_fetch`, `file_list`, `file_search_semantic` | Removed |

**As built: `list_files` is core-intercepted.** This note put it in the sandboxed
tier with an `fs` read grant, alongside the other file tools. But two of its three
arguments — `changed_since` and the `indexed` flag — are answered from the index,
which a sandboxed tool cannot reach: it has a filesystem mount, not a database
handle. Shipping it sandboxed would have meant either a directory walk that
cannot answer them, or a second source of truth about what exists in the
workspace. It is registered with the other core tools instead, and reports that
no workspace is configured rather than being silently absent when there is none.

A rename is a rename, not a read-write-delete: `move_file` exists because without
it, relocating a file over the context window is impossible, and relocating any
file wastes the whole file's tokens twice.

New configuration, all under `[workspace]`:

| Key | Default | Meaning |
|-----|---------|---------|
| `scan_interval` | 60s | Period of the background rescan |
| `index_max_file_bytes` | 8 MiB | Largest file the scan indexes; a larger file still lists, reads, and is searched when named |
| `index_max_files` | 50,000 | Scan bound; search reports truncation |
| `diff_max_bytes` | 1 MiB | Per-side ceiling on a computed diff; above it, counts instead (§8) |
| `trash_retention` | 7 days | Age at which a trash entry is swept |
| `trash_max_bytes` | 1 GiB | Size bound on the trash, oldest entry first |
| `max_bytes` | unset | Budget for what the agent may add to the root (§4) |

## 11. Roles

| Role | Today | After |
|------|-------|-------|
| `software-dev` | `shell, read_file, write_file, file_store, file_fetch, file_list, file_search_text, …` | `shell, read_file, write_file, edit_file, move_file, copy_file, delete_file, list_files, file_search_text, diff_file, trash_list, restore_file, …` |
| `report-writer` | `read_file, file_store, file_fetch, file_list, file_search_text, …` (no `shell`, no `write_file`) | `read_file, write_file, edit_file, move_file, copy_file, delete_file, list_files, file_search_text, diff_file, trash_list, restore_file, …` |
| `monitor` | `file_search_text, file_fetch, …` | `file_search_text, read_file, list_files, diff_file, …` |

`report-writer` persists reports with `file_store` today, which cannot touch the
workspace. After the change it needs `write_file`, which reaches the whole
workspace, including files a sibling sub-agent is editing. The trash makes every
overwrite and delete recoverable, which is what makes the grant acceptable
without a per-role `fs` scope. The restriction `roles-design.md` relies on,
"cannot `shell`", still holds. `monitor` gets `diff_file` but no write tools: it
reports what changed, and changes nothing.

Thirteen file tools is a larger surface than the six they replace, and tool
ranking (`tool-exposition.md`) already selects per turn, so the cost lands on the
ranking budget rather than on every prompt.

## 12. Migration

On first boot after the change, a one-time step writes each non-`spill/` row of
`files` to the same relative path under the workspace root, then deletes the row.
A path that already exists in the workspace is written under
`.nine/migrated-store/<path>` instead, and the step logs each such path.
Same-path placement keeps working any path the agent recorded with `memory_set`.

The step writes outside the database, so it cannot be an R-MEM.10 migration step,
which must commit atomically with its version bump. It runs from bootstrap, and
re-runs safely because it deletes a row only after writing the file.

## 13. Sequencing

Each phase ships on its own and leaves Nine working.

| Phase | Change | Useful alone because |
|-------|--------|----------------------|
| 1 | `shell` runs in the workspace root with `NINE_WORKSPACE` set; `read_file`/`write_file` accept the host-root alias | It fixes the §4 finding whatever happens to the rest |
| 2 | Delete `file_search_semantic` | It is dead today |
| 3 | `nine:fs` gains `readRange`, `appendFile`, `rename`, `copy`; `read_file` gains `offset`/`limit`/`lines`/`line_numbers`/`version` and refuses non-UTF-8; `write_file` gains `mode: "append"` and `if_unchanged`; `edit_file`, `move_file` and `copy_file` ship | Large files become editable and movable, which nothing today allows |
| 4 | `.nine/` layout and `.gitignore`; trash, sweep and bounds; `nine:fs remove`; `delete_file`, `trash_list`, `restore_file`; `write_file`/`edit_file` trash the previous version and write through a rename | Deletion without `shell`, recoverable overwrites, atomic writes |
| 5 | `diff_file`; `preview` on `edit_file`/`write_file`; approval prompts carry diffs | A supervised change becomes reviewable before and after it happens |
| 6 | Workspace index: boot scan, periodic scan, scoped refresh, skip rules, `changed_since`, `pattern`; `file_search_text` covers the workspace; `list_files` ships. Document extraction was dropped from this phase and is unbuilt (§6) | Bind-mounted and externally changed files become findable |
| 7 | `read_file` serves `spill/`; `write_file` gains `content_ref`; the truncation notice names `read_file` | Spills become readable with the tool the model already uses |
| 8 | Remove `file_store`/`file_fetch`/`file_list`; migrate rows; `[workspace].max_bytes`; update roles, descriptions, recovery messages | Completes the change |

**Evals ship with this note**, gated on `NINE_EVAL_WORKSPACE_TOOLS` so they are
reported as skipped rather than failed until the tools they name exist:

| Case | Phase | Asserts |
|------|-------|---------|
| `workspace-edit-large` | 3 | `edit_file` is used and `write_file` is not, and the untouched lines survive |
| `workspace-move-file` | 3 | A file is relocated with `move_file`, without its contents passing through a read |
| `workspace-delete-trash` | 4 | `delete_file` removes a file and a later listing no longer reports it |
| `workspace-restore-trash` | 4 | A deleted file is recovered with `trash_list` and `restore_file` |
| `workspace-diff-review` | 5 | After an edit, the agent reports the change as a diff naming both the old and new line |
| `workspace-find-by-name` | 6 | A file is located by name pattern, not by content |
| `workspace-external-file` | 6 | A file placed in the workspace before the session starts is found by `file_search_text` |
| `workspace-search-unindexed` | 6 | With `index_max_file_bytes` lowered to 64, a named file over the ceiling is still searched |
| `workspace-write-search` | 6 | `write_file` in one turn, `file_search_text` finds it in a later turn — the failure the split namespaces produced |

Phase 3 also needs a generated multi-MB fixture in the harness: a case file cannot
carry one inline, and `workspace-edit-large` proves the targeting, not the size.
Phase 8 updates `file-store-search`, `replay-file-store-search`, `spill-read-back`
and `spill-ref-passing`, and removes the `NINE_EVAL_WORKSPACE_TOOLS` gate from the
cases above. The replay journals under `tests/evals/replay/` record advertised
tool lists and must be regenerated.

Phase 8 touches the spec contracts named in the header, `spec/conformance.md`,
`docs/` (`tool-output.md`, `glossary.md`, `architecture.md`, `agent-loop.md`,
`configuration.md`, `evals.md`, `plugins.md`, `writing-sandboxed-tools.md`,
`personalities.md`), `README.md`, and the three role skills.

No plugin wire contract changes: plugins cannot call the file tools, and
`plugin.ProtocolVersion` is unaffected.

## Limits

| Limit | Detail |
|-------|--------|
| The shell finding was never reproduced end to end | §4's claim came from reading `shell.go` and the s6 run script, and the fix is covered at the plugin boundary. Neither the original behaviour nor the fix was observed through a running daemon with a live model. |
| `shell` is not confined to the workspace | Setting `cmd.Dir` fixes where relative paths resolve; it does not stop `cd /`. `shell` is a subprocess with the daemon's authority, and confining it is a separate design. The destructive-command guard is unchanged. |
| `shell` bypasses the trash and the budget | `rm file`, an overwrite by a build, and `max_bytes` are all invisible to a subprocess. The trash and the budget cover `nine:fs` only; the tool descriptions steer the model to the tools that honor them. |
| The trash consumes the operator's disk | It lives in the workspace by design (§4). `trash_max_bytes` and `trash_retention` bound it; a workspace on a small volume needs them tuned. |
| Restore does not undo a sequence | `restore_file` recovers one version of one path. An agent that made ten changes must restore ten entries, in an order it works out itself. There is no transaction and no snapshot. |
| Document extraction is unbuilt | A PDF or other non-text document lists with `indexed: false` and a reason, and is not searchable. It was dropped from phase 6 (§6); nothing else in this note depends on it. |
| `.gitignore` support is partial | The scan honors literal names, directory entries and `*` globs in a root `.gitignore`. Nested ignore files, negation and pattern edge cases are not implemented; the effect is extra indexed files, not missing ones. |
| Contentless deletes need SQLite 3.43+ | The index retracts postings with `contentless_delete=1`. If the bundled SQLite predates it, the fallback is an external-content index over a path+content table, which costs a second copy of every indexed file on disk. |
| Direct scan is unranked | Searching a named unindexed file matches literal terms in one pass, with no stemming and no `bm25` ranking. A search that spans the workspace and one large named file therefore mixes two kinds of result, and says which is which. |
| Snippet cost | Building a snippet re-reads the file from disk, so a search returning many hits from large files does many ranged reads. Hits are capped by `limit`, which bounds it. |
| Diffs are line-level and bounded | `nine:diff` is an exact LCS over lines, quadratic in their number. Above `diff_max_bytes` a change is reported as counts, not content, and a minified or single-line file diffs as one replaced line whatever changed inside it. |
| `if_unchanged` is optional | A conflict is detected only when the caller passes the token it read. Two sub-agents that both omit it still race, and the second write stands. Mandatory tokens were rejected: they would make every one-shot write a two-call sequence. |
| Scan cost on a large workspace | A repository over `index_max_files` is indexed up to the bound and searches say so. A workspace that changes constantly may need a watcher rather than a 60-second rescan; that is out of scope. |
| Symlinks are not followed out of the root | A symlink inside the workspace pointing outside is listed but not indexed, and reads through it are whatever the pre-open allows. |
| Read-only mounts surface late | A read-only bind mount fails at the first write with the operating system's error. Nine does not probe writability at boot. |
| External-change events are unbuilt | `changed_since` lets an agent pull what changed. Pushing a change into a standing agent's turn needs the subscription design in `adr/reactive-events.md`. |
| Agent files lose tamper evidence | A `file_store` file changed only through `file_store`. A workspace file can be changed by `shell`, by a sibling sub-agent, or by the operator. No current feature depends on that property. |
| `nine trace` does not capture the workspace | A trace resolves spills but not workspace files, which may have changed since the turn ran. The same holds for `write_file` today. |
| Native path agreement is partial | Without a `/work` directory on the host, only relative paths mean the same thing to `shell` and the sandboxed tools. |
