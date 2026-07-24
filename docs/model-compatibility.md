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

| Case (min class) | `gemma4:e2b` (nano · H1) | `gemma4:e4b` (nano · H1) | `qwen3.5:9b` (small · H1) | `claude-haiku` (medium · API) | `claude-sonnet-5` (large · API) |
|------------------|:---:|:---:|:---:|:---:|:---:|
| `shell-echo` (nano) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `time-current` (nano) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `kv-roundtrip` (small) | ✓ 2/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `role-report-writer-no-shell` (small) | ✓ 2/2 | ✓ 2/2 | ✓ 2/2 | — | — |
| `files-write-read` (small) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `semantic-memory` (medium) | ✗ 0/3 | ✗ 0/3 | ✗ 0/3 | — | — |
| `memory-delete` (small) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `goal-create` (medium) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `workflow-plan` (medium) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `skill-write-recall` (small) | ✓ 3/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `file-store-search` (medium) | ✗ 1/3 | ✓ 3/3 | ✓ 3/3 | — | — |
| `delegate-subagent` (medium) | ✗ 1/3 | ✗ 0/3 | ✓ 3/3 | — | — |

Source runs (all on H1, post-fix): `gemma4:e2b` — `reports/20260724-170752.json`
(14 min, 9/12); `gemma4:e4b` — `reports/20260724-182911.json` (34 min, 10/12);
`qwen3.5:9b` — `reports/20260724-155013.json` (34 min, **11/12**). Every miss in
all three is a tolerated below-class case, so all three suites are green.

## What we've learned so far

- **`gemma4:e2b` (nano, H1)** — passes **9 of 12** with the harness fixes in place,
  and every miss is a tolerated below-class case, so the suite is green. Notably it
  now passes `files-write-read` (3/3, was 0/3 before the `/work` fix) and
  `skill-write-recall` (3/3, was 1/3). It still can't reliably drive the two
  hardest `medium` cases — `file-store-search` and `delegate-subagent` (both 1/3,
  nano-level flakiness on multi-step) — and misses `semantic-memory` the same way
  qwen does. `kv-roundtrip` slipped to 2/3 (still meets threshold) — ordinary
  small-model run-to-run variance.
- **`gemma4:e4b` (nano, H1)** — the stronger nano: **10 of 12**, one better than
  its e2b sibling. It clears `file-store-search` 3/3 (e2b managed only 1/3) and is
  otherwise clean across the basics and the `medium` goal/workflow cases. Its only
  misses are `delegate-subagent` (0/3 — two-level sub-agent delegation is still out
  of reach at nano) and `semantic-memory` (0/3 — KV instead of embed, as with the
  others). Both tolerated; suite green.
- **`qwen3.5:9b` (small, H1)** — passes **11 of 12** cases, including every one at
  or below its class and all four `medium` delegation/file cases; the suite is
  green (no fatal). Getting there took fixing four genuine harness bugs, not the
  model: (1) the `files` plugin rejected the eval's `/work/…` paths, so
  `files-write-read` was 0/3 for *every* model — now a `/work` root alias resolves
  them; (2) the live harness ran with no embedder, so `memory_embed`/`memory_query`
  were never registered — now an Ollama `nomic-embed-text` embedder is wired in;
  (3) transient Ollama flakiness (a malformed tool-call, a slow-host deadline) now
  retries instead of counting as a failure, which recovered `file-store-search`
  and `delegate-subagent`; (4) `delegate-subagent`'s per-run timeout was raised to
  900s for the nested sub-agent loop.
- **`semantic-memory` (medium)** is qwen's one remaining miss, and it is **model
  behavior, not a bug**: the embed tools are now available, but qwen answers the
  task with plain `memory_set`/`memory_get` (KV) instead of
  `memory_embed`/`memory_query`, so the vector never gets written. It gets the
  right answer by the "wrong" path. A 9B model reaching for KV when it suffices is
  exactly what the `medium` class encodes; the miss is tolerated and the suite
  stays green. Forcing it would need a prompt that names the semantic path.
- **Delegation (`delegate-subagent`)** — qwen now drives the two-level sub-agent
  flow reliably (3/3) once the timeout and retry stopped masking it. It stays the
  ceiling for **nano** `gemma4:e2b` (1/3) — a 2B model spawning and steering a
  sub-agent is genuinely at its limit, which is what the `medium` class encodes.
- **`claude-*` (medium/large, API)** — **not yet run.** The matrix needs
  `ANTHROPIC_API_KEY`; these rows are the priority for the next run, since the
  delegation/workflow/goal cases target their class and the local models above
  only sample the easy end of them.

## Keeping this current

After a Track-L run:

1. Note the host you ran local models on (add a profile row if it's new).
2. For each model, copy the latest `reports/<date>.json` results into the matrix
   (pass fraction per case; `✗!` when a failure is at/above the model's class).
3. Record which report each column came from under the matrix.

The `reports/` JSON is the raw record (git-ignored run artifacts); this file is
the curated, committed summary humans read.
