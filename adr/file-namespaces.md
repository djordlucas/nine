# Design note — The workspace as the agent's filesystem

- **Status:** Proposed.
- **Date:** 2026-09-20.
- **Depends on:** `tool-output-spill.md`, `rich-js-tools.md` (§6.4, why `nine:fs`
  is libc rather than host functions), `spec/contracts/memory-store.md` (R-MEM.2,
  R-MEM.3, R-MEM.9), `spec/contracts/dispatcher.md` (R-DISP.2),
  `spec/contracts/toolvm.md`, the shipped sandboxed tools
  (`internal/toolvm/shipped.go`).
- **Amends:** R-MEM.2 and R-MEM.3 (the `files` table stops holding agent-authored
  files), R-MEM.9 (the `spill/` write refusal moves from `file_store` to
  `write_file`), R-DISP.2 (the truncation notice names `read_file`), the `nine:fs`
  module (gains `remove`, `rename`, ranged reads and appends).

The agent sees three file namespaces, and choosing the wrong one is the most
common model error recorded in `tool-output-spill.md`. This note proposes one
namespace for everything the agent reads and writes — the workspace — with
truncated tool output as the single read-only exception:

1. **Agent-written files live in the workspace only.** `file_store`, `file_list`
   and `file_fetch` are retired. The agent's file tools become `read_file`,
   `write_file`, `edit_file`, `delete_file`, `list_files` and `file_search_text`.
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
7. **`delete_file` moves a file to `.nine/trash/` inside the workspace**, swept on
   a retention window. `write_file` and `edit_file` move the previous version
   there too (§8).

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
  outside the sandbox. This is from reading `shell.go` and the s6 run script; it
  has not been reproduced against a running daemon.
- **The trash lives inside the workspace** (§8), not in a sibling directory.
- **Nine's own bookkeeping in the workspace lives under `.nine/`**, which is
  excluded from search, listing and agent writes.

The sandboxed tools are confined by wazero's pre-open, which is one mount: the
root. `shell` is a subprocess holding the daemon's own authority, so its working
directory is a default, not a confinement — see Limits.

## 5. What agent-written files lose when they leave the database

| Property today | Replacement |
|----------------|-------------|
| Full-text search via `files_fts` triggers | A derived contentless index over the workspace root (§6) |
| Windowed reads (`file_fetch` `offset`/`limit`) | `read_file` gains `offset` and `limit` |
| Copy by reference (`file_store` `content_ref`) | `write_file` gains a `content_ref` property marked `x-nine-ref`. `RegisterSandboxed` already indexes ref-marked properties (`dispatcher.go`) |
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
- The result names the path, the number of replacements, and the line numbers
  touched.
- It refuses a file that is not valid UTF-8, and any path under `.nine/`.

**Implementation.** The file is never materialized whole in the guest. The tool
scans it in overlapping windows (window ≥ 2× `old_text`, so a match cannot
straddle a boundary undetected), writes the result to a temporary file beside the
original, then renames the temporary over the original. Memory stays bounded by
the window, well inside the sandbox's limits.

That needs three additions to `nine:fs`, all ordinary libc calls in the existing
pre-open, which is what `rich-js-tools.md` §6.4 asks for — containment stays
wazero's rather than becoming a path check Nine owns:

| Addition | libc |
|----------|------|
| `readRange(path, offset, length)` | `fseek` + `fread` |
| `appendFile(path, data)` | `fopen(path, "ab")` |
| `rename(from, to)` | `rename` |

`read_file` gains `offset` and `limit` over the same `readRange`, plus a
`line_numbers` option, so a model can locate a region with `file_search_text`,
read it with line numbers, and name exact text to `edit_file`.

**Atomic replacement comes free.** Writing to a temporary and renaming means an
interrupted edit leaves the original intact, which `write_file` does not manage
today. `write_file` adopts the same sequence.

## 8. Deletion and the trash

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
| Agent access | `.nine/` is excluded from the index, from `list_files` and from every write tool. `read_file` can still read a trashed file, so an agent that notices its own mistake can copy the contents back |
| Approval | Not gated by default. An operator can add `delete_file` to `[hitl].require_approval` |

**Mechanism.** With the trash inside the mount, `nine:fs remove` is a `mkdir`
plus a `rename` in the pre-open — the same libc surface §7 already adds, and no
new host function. `remove` always trashes, including for generated tools: one
rule for every deletion through `nine:fs` is simpler to state than a per-tool
policy, and the sweep bounds the cost.

## 9. Tool surface after the change

| Tool | Change |
|------|--------|
| `read_file` | Adds `offset`, `limit`, `line_numbers`. The dispatcher serves `spill/...` from the store; other paths go to the sandbox |
| `write_file` | Adds `content_ref`. Writes through a temporary and renames. Trashes the previous version. Refuses `spill/...` and `.nine/...` |
| `edit_file` | New shipped sandboxed tool (§7) |
| `delete_file` | New shipped sandboxed tool (§8) |
| `list_files` | New shipped sandboxed tool, `fs` read grant. Lists workspace paths under an optional prefix, with `changed_since`. Replaces `file_list`, which roles without `shell` otherwise lose |
| `file_search_text` | Searches the workspace index and spills. A `spill/` prefix searches spills; another prefix searches the workspace; no prefix searches both and labels each hit |
| `shell` | Runs with `cmd.Dir` set to the workspace root and `NINE_WORKSPACE` in its environment |
| `file_store`, `file_fetch`, `file_list`, `file_search_semantic` | Removed |

New configuration, all under `[workspace]`:

| Key | Default | Meaning |
|-----|---------|---------|
| `scan_interval` | 60s | Period of the background rescan |
| `index_max_file_bytes` | 8 MiB | Largest file the scan indexes; a larger file still lists, reads, and is searched when named |
| `index_max_files` | 50,000 | Scan bound; search reports truncation |
| `trash_retention` | 7 days | Age at which a trash entry is swept |
| `trash_max_bytes` | 1 GiB | Size bound on the trash, oldest entry first |

## 10. Roles

| Role | Today | After |
|------|-------|-------|
| `software-dev` | `shell, read_file, write_file, file_store, file_fetch, file_list, file_search_text, …` | `shell, read_file, write_file, edit_file, delete_file, list_files, file_search_text, …` |
| `report-writer` | `read_file, file_store, file_fetch, file_list, file_search_text, …` (no `shell`, no `write_file`) | `read_file, write_file, edit_file, delete_file, list_files, file_search_text, …` |
| `monitor` | `file_search_text, file_fetch, …` | `file_search_text, read_file, list_files, …` |

`report-writer` persists reports with `file_store` today, which cannot touch the
workspace. After the change it needs `write_file`, which reaches the whole
workspace, including files a sibling sub-agent is editing. The trash makes every
overwrite and delete recoverable, which is what makes the grant acceptable
without a per-role `fs` scope. The restriction `roles-design.md` relies on,
"cannot `shell`", still holds. `monitor` gets no write tools.

## 11. Migration

On first boot after the change, a one-time step writes each non-`spill/` row of
`files` to the same relative path under the workspace root, then deletes the row.
A path that already exists in the workspace is written under
`.nine/migrated-store/<path>` instead, and the step logs each such path.
Same-path placement keeps working any path the agent recorded with `memory_set`.

The step writes outside the database, so it cannot be an R-MEM.10 migration step,
which must commit atomically with its version bump. It runs from bootstrap, and
re-runs safely because it deletes a row only after writing the file.

## 12. Sequencing

Each phase ships on its own and leaves Nine working.

| Phase | Change | Useful alone because |
|-------|--------|----------------------|
| 1 | `shell` runs in the workspace root with `NINE_WORKSPACE` set; `read_file`/`write_file` accept the host-root alias | It fixes the §4 finding whatever happens to the rest |
| 2 | Delete `file_search_semantic` | It is dead today |
| 3 | `nine:fs` gains `readRange`, `appendFile`, `rename`; `read_file` gains `offset`/`limit`/`line_numbers`; `edit_file` ships | Large files become editable, which nothing today allows |
| 4 | `.nine/` layout and `.gitignore`; trash, sweep and bounds; `nine:fs remove`; `delete_file`; `write_file`/`edit_file` trash the previous version and write through a rename | Deletion without `shell`, recoverable overwrites, atomic writes |
| 5 | Workspace index: boot scan, periodic scan, scoped refresh, skip rules, `changed_since`; `file_search_text` covers the workspace; `list_files` ships | Bind-mounted and externally changed files become findable |
| 6 | `read_file` serves `spill/`; `write_file` gains `content_ref`; the truncation notice names `read_file` | Spills become readable with the tool the model already uses |
| 7 | Remove `file_store`/`file_fetch`/`file_list`; migrate rows; update roles, descriptions, recovery messages | Completes the change |

**Evals ship with this note**, gated on `NINE_EVAL_WORKSPACE_TOOLS` so they are
reported as skipped rather than failed until the tools they name exist:

| Case | Phase | Asserts |
|------|-------|---------|
| `workspace-edit-large` | 3 | `edit_file` is used and `write_file` is not, and the untouched lines survive |
| `workspace-delete-trash` | 4 | `delete_file` removes a file and a later listing no longer reports it |
| `workspace-external-file` | 5 | A file placed in the workspace before the session starts is found by `file_search_text` |
| `workspace-search-unindexed` | 5 | With `index_max_file_bytes` lowered to 64, a named file over the ceiling is still searched |
| `workspace-write-search` | 5 | `write_file` in one turn, `file_search_text` finds it in a later turn — the failure the split namespaces produced |

Phase 3 also needs a generated multi-MB fixture in the harness: a case file cannot
carry one inline, and `workspace-edit-large` proves the targeting, not the size.
Phase 7 updates `file-store-search`, `replay-file-store-search`, `spill-read-back`
and `spill-ref-passing`, and removes the `NINE_EVAL_WORKSPACE_TOOLS` gate from the
five cases above. The replay journals under `tests/evals/replay/` record advertised
tool lists and must be regenerated.

Phase 7 touches the spec contracts named in the header, `spec/conformance.md`,
`docs/` (`tool-output.md`, `glossary.md`, `architecture.md`, `agent-loop.md`,
`configuration.md`, `evals.md`, `plugins.md`, `writing-sandboxed-tools.md`,
`personalities.md`), `README.md`, and the three role skills.

No plugin wire contract changes: plugins cannot call the file tools, and
`plugin.ProtocolVersion` is unaffected.

## Limits

| Limit | Detail |
|-------|--------|
| Unverified shell finding | §4's claim that `shell` does not run in the workspace comes from reading `shell.go` and the s6 run script. Phase 1 starts by reproducing it. |
| `shell` is not confined to the workspace | Setting `cmd.Dir` fixes where relative paths resolve; it does not stop `cd /`. `shell` is a subprocess with the daemon's authority, and confining it is a separate design. The destructive-command guard is unchanged. |
| `shell rm` bypasses the trash | The trash covers `nine:fs`. `rm file` in `shell` still deletes permanently; the `delete_file` description steers the model away from it. Blocking non-recursive `rm` in the shell guard was rejected as too disruptive for development work. |
| The trash consumes the operator's disk | It lives in the workspace by design (§4). `trash_max_bytes` and `trash_retention` bound it; a workspace on a small volume needs them tuned. |
| `.gitignore` support is partial | The scan honors literal names, directory entries and `*` globs in a root `.gitignore`. Nested ignore files, negation and pattern edge cases are not implemented; the effect is extra indexed files, not missing ones. |
| Contentless deletes need SQLite 3.43+ | The index retracts postings with `contentless_delete=1`. If the bundled SQLite predates it, the fallback is an external-content index over a path+content table, which costs a second copy of every indexed file on disk. |
| Direct scan is unranked | Searching a named unindexed file matches literal terms in one pass, with no stemming and no `bm25` ranking. A search that spans the workspace and one large named file therefore mixes two kinds of result, and says which is which. |
| Snippet cost | Building a snippet re-reads the file from disk, so a search returning many hits from large files does many ranged reads. Hits are capped by `limit`, which bounds it. |
| Scan cost on a large workspace | A repository over `index_max_files` is indexed up to the bound and searches say so. A workspace that changes constantly may need a watcher rather than a 60-second rescan; that is out of scope. |
| Concurrent edits are last-writer-wins | `edit_file` and `write_file` rename over the target. Two sub-agents editing one file both succeed, and the second result stands. No locking is proposed. |
| Symlinks are not followed out of the root | A symlink inside the workspace pointing outside is listed but not indexed, and reads through it are whatever the pre-open allows. |
| Read-only mounts surface late | A read-only bind mount fails at the first write with the operating system's error. Nine does not probe writability at boot. |
| External-change events are unbuilt | `changed_since` lets an agent pull what changed. Pushing a change into a standing agent's turn needs the subscription design in `adr/reactive-events.md`. |
| Agent files lose tamper evidence | A `file_store` file changed only through `file_store`. A workspace file can be changed by `shell`, by a sibling sub-agent, or by the operator. No current feature depends on that property. |
| `nine trace` does not capture the workspace | A trace resolves spills but not workspace files, which may have changed since the turn ran. The same holds for `write_file` today. |
| Native path agreement is partial | Without a `/work` directory on the host, only relative paths mean the same thing to `shell` and the sandboxed tools. |
