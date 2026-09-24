# Model compatibility

Which LLMs Nine has been run against, how well they drive the agent loop, and on
what hardware. Use it to judge whether a model is likely to work for you before
you wire it up.

This is a **living record of observed results**, not a support promise. It is
compiled by hand from Track-L eval runs (`tests/evals/reports/<date>.json`, see
[evals.md](evals.md) §6/§8). Re-run the matrix and update the tables below when
you test a new model or a new host.

## How to read this

- Results come from the **Track-L** eval matrix: each case is run N times on a
  model and graded with tolerant, side-effect/trajectory assertions
  ([evals.md](evals.md) §1). A cell shows the pass fraction for that case.
- Every case declares an `expected_pass_min_class` (`nano < small < medium <
  large`). A model is only *expected* to pass cases at or below its own class; a
  failure **below** a model's class is normal (a 3B model can't do 3-hop
  delegation) and is reported, not counted against the model. See
  [evals.md](evals.md) §6.
- ✓ = met the case's pass threshold · ✗ = did not · — = not run · ✗! = a
  fatal miss (failed a case at or above the model's own class).

Reproduce any row:

```sh
NINE_EVAL_MODELS=<model> make eval-live      # needs Ollama at NINE_LLM_ENDPOINT
```

## Hardware profiles

Every model is local (Ollama), so throughput and timeout behavior depend on the
host — a case that fails only with `context deadline exceeded` on a small host
may pass on a faster one. Always read a result together with its host profile.

| Host | Machine | Chip | Memory | OS | Runtime |
|------|---------|------|--------|-----|---------|
| **H1** | Mac16,10 | Apple M4 (10 core) | 16 GB | macOS 26.6 | Ollama 0.31.2 |

> H1 is the reference host used for the local-model runs below. If you run the
> matrix on different hardware, add a row and tag your results with it.

## Compatibility matrix

Cases × models, from the most recent run of each model. `Class` is the model's
capability tier; `Host` is the profile it ran on.

**This is the 12-case corpus as it stood in 2026-07.** The corpus is now 28 live
cases; the rows added since — the `workspace-*` set, the spill, generated-tool and
`skill-search` cases — have not been run across all four models, and the workspace
set has its own table below. A case absent from this table is unmeasured on that
model, not failing.

| Case (min class) | `gemma4:e2b` (nano · H1) | `gemma4:e4b` (nano · H1) | `qwen3.5:4b` (small · H1) | `qwen3.5:9b` (small · H1) |
|------------------|:---:|:---:|:---:|:---:|
| `shell-echo` (nano) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `time-current` (nano) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `kv-roundtrip` (small) | ✓ 2/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `role-report-writer-no-shell` (small) | ✓ 2/2 | ✓ 2/2 | ✓ 2/2 | ✓ 2/2 |
| `files-write-read` (small) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `semantic-memory` (small) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 2/3 |
| `memory-delete` (small) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `goal-create` (medium) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `workflow-plan` (medium) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `skill-write-recall` (small) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 |
| `file-store-search` (medium) † | ✗ 1/3 | ✓ 3/3 | ✓ 2/3 | ✓ 3/3 |
| `delegate-subagent` (medium) | ✗ 1/3 | ✗ 0/3 | ✓ 3/3 | ✓ 3/3 |

† `file-store-search` was retired with the `file_store` / `file_fetch` /
`file_list` tools it exercised. The row is the record of a run, not a case you can
re-run; `workspace-write-search` is the nearest current equivalent.

### Workspace file tools (2026-09-21, H1)

From two runs of the nine `workspace-*` cases on `qwen3.5:9b` (small), plus the
pre-existing suite. Recorded because the class field is a finding, not an estimate:
a case marked above every model on the machine can never fail the suite, and one
marked below what it reliably reaches fails it for no reason.

| Case | Run 1 | Run 2 | Recorded class |
|------|:---:|:---:|:---:|
| `workspace-delete-trash` | 3/3 | 3/3 | small |
| `workspace-diff-review` | 3/3 | 3/3 | small |
| `workspace-edit-large` | 3/3 | 3/3 | small |
| `workspace-find-by-name` | 3/3 | 3/3 | small |
| `workspace-move-file` | 3/3 | 3/3 | small |
| `workspace-restore-trash` | 0/3 | 2/3 | medium |
| `workspace-search-unindexed` | 2/3 | 2/3 | medium |
| `workspace-write-search` | 2/3 | 1/3 | medium |
| `workspace-external-file` | 3/3 | 1/3 | medium |

The `restore-trash` jump is a corrected assertion, not a model difference: run 1
required the model to quote the restored file's contents back, which a correct
restore does not entail.

Where the misses cluster: `file_search_text` is not reliably chosen at this size.
`external-file` and `write-search` both failed runs with "`file_search_text` never
called" — the model answered by reading or listing instead, reaching the right
answer the wrong way. The index and the tool work; tool *selection* is the gap.

**`gemma4:12b` is not usable on H1.** Nine runs took 109 minutes and died of
`context deadline exceeded` rather than grading: a 12B model exceeds
`[llm].timeout_seconds` on ordinary turns at 16 GB. H1 therefore has no
medium-class model, so a `medium` case reports locally but cannot fail the suite.

`tool-output-spill` is marginal here independently of the workspace work: 1/3 on
current main, 2/3 on `a011dbb` (the commit before the truncation banner changed).
Three runs a side cannot separate those, and 2/3 is the threshold itself.

Source runs (all on H1, post-fix): `gemma4:e2b` — `reports/20260724-170752.json`
(**10/12**); `gemma4:e4b` — `reports/20260724-182911.json` (**11/12**);
`qwen3.5:4b` and `qwen3.5:9b` — `reports/20260724-195901.json` /
`20260724-155013.json` (**12/12 each**). The `semantic-memory` row is from the
corrected-case re-run `reports/20260725-190739.json`. Every remaining miss is a
tolerated below-class case, so all four suites are green.

`gemma4:12b` (medium) is **not benchmarked on H1**: on 16 GB it is
throughput-bound and thrashes — runs hit `deadline exceeded` / malformed-tool-call
errors so often that even with retries the numbers would measure the host's memory
ceiling, not the model. Revisit it on a host with more memory.

## What we've learned so far

- **`qwen3.5:4b` and `qwen3.5:9b` (small, H1)** — both now pass **all 12 cases**,
  including every `medium` delegation/file case. Getting there took fixing genuine
  harness bugs, not the models: (1) the `files` plugin rejected the eval's `/work/…`
  paths, so `files-write-read` was 0/3 for *every* model — now a `/work` root alias
  resolves them; (2) the live harness ran with no embedder, so semantic memory
  couldn't index — now an Ollama `nomic-embed-text` embedder is wired in;
  (3) transient Ollama flakiness (a malformed tool-call, a slow-host deadline) now
  retries instead of counting as a failure, which recovered `file-store-search`
  and `delegate-subagent`; (4) `delegate-subagent`'s per-run timeout was raised to
  900s for the nested sub-agent loop. That a 4B model holds the whole tool surface
  is the strongest signal that the fixes, not raw scale, were the blocker.
- **`gemma4:e4b` (nano, H1)** — **11 of 12**: clean everywhere except
  `delegate-subagent` (0/3 — driving a two-level sub-agent is out of reach at
  nano). Tolerated; suite green.
- **`gemma4:e2b` (nano, H1)** — **10 of 12**: passes the basics, the `medium`
  goal/workflow/semantic cases, and (post-`/work`-fix) `files-write-read`. Its two
  misses are the hardest `medium` cases — `file-store-search` (1/3) and
  `delegate-subagent` (1/3) — nano-level flakiness on multi-step work. Both
  tolerated; `kv-roundtrip` at 2/3 is ordinary run-to-run variance.
- **`semantic-memory`** now passes on all four models — but only after the case
  was **corrected to test the real feature**. It originally asserted calls to
  `memory_embed`/`memory_query`, which turned out to be **never advertised** to the
  model (not in `coreToolNames`), so it could never pass on any model. In this
  architecture semantic memory is driven *through* `memory_set`: each set
  auto-embeds its value into the shared `memories` vector pool for later relevance
  surfacing. The rewritten case stores facts with `memory_set` (asserting the
  vector pool is populated) and selectively recalls one — an honest test of the
  path as built. (Isolating pull-surfacing specifically would need a fresh session,
  which the harness doesn't yet express.)
- **Delegation (`delegate-subagent`)** — qwen now drives the two-level sub-agent
  flow reliably (3/3) once the timeout and retry stopped masking it. It stays the
  ceiling for **nano** `gemma4:e2b` (1/3) — a 2B model spawning and steering a
  sub-agent is genuinely at its limit, which is what the `medium` class encodes.
- **`medium`/`large` local models** — **not yet run.** `gemma4:12b` is the only
  medium model attempted here and it is memory-bound on H1 (see above); the
  medium and large rows are the next priority, on a host with more memory, to
  confirm behavior at their own class and to exercise judged cases. The small
  models already clear the full corpus.

## Post-tool-observation microbenchmark (H1)

Separate from the matrix, and much narrower: one real turn's **second** LLM call —
the one following a `skill_list` observation — captured from the session journal
and replayed 10 times per model. It isolates the failure R-LOOP.5's retry exists
to absorb: a response carrying neither text nor a tool call.

| Model | Resident | Empty | Median | Range |
|-------|---------:|------:|-------:|------:|
| `qwen3.5:4b`  | 3.4 GB | **0/10** | **16.5 s** | 13.9–21.6 s |
| `qwen3.5:9b`  | 6.6 GB | 0/10 | 22.2 s | 19.1–24.7 s |
| `gemma4:12b`  | 7.6 GB | 0/10 | 32.3 s | 26.8–34.3 s |
| `gemma4:e2b`  | 7.2 GB | 2/10 | 8.1 s | 0.2–12.7 s |

Each model was warmed up once first, so load time is excluded, and nothing else
was using Ollama. Only `gemma4:e2b` produces the empty response at all — its 0.2 s
minimum *is* the failure, a lone end-of-turn token. An earlier version of this run
reported wildly slower and more variable timings for every model; that run had a
Nine daemon sharing the same Ollama, and the constant model eviction dominated the
numbers. Measure with nothing else resident.

One case is not a capability verdict — `gemma4:12b` still has **no Track-L run at
all**. Use the matrix above for that.

## Keeping this current

After a Track-L run:

1. Note the host you ran local models on (add a profile row if it's new).
2. For each model, copy the latest `reports/<date>.json` results into the matrix
   (pass fraction per case; `✗!` when a failure is at/above the model's class).
3. Record which report each column came from under the matrix.

The `reports/` JSON is the raw record (git-ignored run artifacts); this file is
the curated, committed summary humans read.

## Limits

| Limit | Detail |
|-------|--------|
| Small models only | The matrix covers models that fit a 16 GB machine. Nine is developed against small models as a baseline; behavior on large hosted models is unmeasured. |
| One hardware profile per column | Results are per host profile. A model's score does not transfer across hardware, quantization, or `num_ctx`. |
| A snapshot, not a guarantee | Each column comes from one Track-L report on one date. Model releases move; a passing row can stop being true without this file changing. |
| Curated by hand | `reports/` JSON is the raw record and is git-ignored. This file is the summary someone copied across, so it can lag the last run. |
| The matrix lags the corpus | The main table is 12 cases from 2026-07 against a corpus that now holds 28 live ones. Most of the difference has been measured on one model only, so "not in the table" means unmeasured rather than passing. |
