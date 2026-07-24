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
docker compose up -d postgres
NINE_EVAL_MODELS=<model> make eval-live      # local models need Ollama at NINE_LLM_ENDPOINT
# claude-* models additionally need ANTHROPIC_API_KEY
```

## Hardware profiles

Local (Ollama) models run on real hardware, and throughput/timeout behavior
depends on it — a case that fails only with `context deadline exceeded` on a
small host may pass on a faster one. Hosted `claude-*` models run on the
provider's infrastructure over the API, so they have no local hardware profile.

| Host | Machine | Chip | Memory | OS | Runtime |
|------|---------|------|--------|-----|---------|
| **H1** | Mac16,10 | Apple M4 (10 core) | 16 GB | macOS 26.6 | Ollama 0.31.2 |
| **API** | — | provider-hosted | — | — | Anthropic API |

> H1 is the reference host used for the local-model runs below. If you run the
> matrix on different hardware, add a row and tag your results with it.

## Compatibility matrix

Cases × models, from the most recent run of each model. `Class` is the model's
capability tier; `Host` is the profile it ran on.

| Case (min class) | `gemma4:e2b` (nano · H1) | `qwen3.5:9b` (small · H1) | `claude-haiku` (medium · API) | `claude-sonnet-5` (large · API) |
|------------------|:---:|:---:|:---:|:---:|
| `shell-echo` (nano) | ✓ 3/3 | ✓ 3/3 | — | — |
| `time-current` (nano) | ✓ 3/3 | ✓ 3/3 | — | — |
| `kv-roundtrip` (small) | ✓ 3/3 | ✓ 3/3 | — | — |
| `role-report-writer-no-shell` (small) | ✓ 2/2 | ✓ 2/2 | — | — |
| `files-write-read` (small) | ✗ 0/3 | ✗! 0/3 | — | — |
| `semantic-memory` (medium) | ✗ 0/3 | ✗ 0/3 | — | — |
| `goal-create` (medium) | — | — | — | — |
| `workflow-plan` (medium) | — | — | — | — |
| `skill-write-recall` (small) | — | — | — | — |
| `memory-delete` (small) | — | — | — | — |
| `file-store-search` (medium) | — | — | — | — |
| `delegate-subagent` (medium) | — | — | — | — |

Source runs: `gemma4:e2b` — `reports/20260723-230230.json` (also 20260723-183117,
same pattern); `qwen3.5:9b` — `reports/20260723-234738.json`.

## What we've learned so far

- **`gemma4:e2b` (nano, H1)** — reliable at single-tool-call work: shell, time,
  KV roundtrip, and role gating all pass. As expected for its class it does not
  handle the multi-step file-copy or semantic-embedding cases. Good smoke-tier
  signal on a 16 GB Mac.
- **`qwen3.5:9b` (small, H1)** — same shape as nano: solid on tool-calling
  basics, but `files-write-read` fails **at its own class** (`✗!`) — one run also
  tripped a `context deadline exceeded` on H1, so this is partly a
  throughput/timeout signal worth re-checking on faster hardware before reading
  it as a pure capability gap. `semantic-memory` is below its class, so its
  failure is expected.
- **`claude-*` (medium/large, API)** — **not yet run.** The matrix needs
  `ANTHROPIC_API_KEY`; these rows are the priority for the next run, since the
  delegation/workflow/goal cases target their class.
- **Newly added cases** (`goal-create`, `workflow-plan`, `skill-write-recall`,
  `memory-delete`, `file-store-search`, `delegate-subagent`) have **not been run
  on any model yet** — they postdate the reports above.

## Keeping this current

After a Track-L run:

1. Note the host you ran local models on (add a profile row if it's new).
2. For each model, copy the latest `reports/<date>.json` results into the matrix
   (pass fraction per case; `✗!` when a failure is at/above the model's class).
3. Record which report each column came from under the matrix.

The `reports/` JSON is the raw record (git-ignored run artifacts); this file is
the curated, committed summary humans read.
