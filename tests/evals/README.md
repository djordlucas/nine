# Nine evals

End-to-end behavioral tests for Nine, per [docs/evals.md](../../docs/evals.md).
Two tracks:

- **Track R — replay** (deterministic, no live model). A recorded session journal
  is re-executed against a recorded provider/dispatcher and the reproduced answers
  are asserted equal to the recorded ones. Guards the loop / dispatcher / context
  code — the parts we own. Infra-free; runs on every PR.
- **Track L — live** (behavioral). The same case run against a real model (or a
  matrix), graded over N runs with tolerant, side-effect- and trajectory-based
  assertions. Needs Postgres + plugin binaries + a model.

## Layout

```
cases/        *.yaml — the case corpus (schema: docs/evals.md §2)
replay/<id>/  recorded Track-R fixtures (journal.json)
runner/       the Go harness: loads cases, isolates state, drives turns, grades
reports/      JSON + rendered grid, one per live run
```

## Running

```sh
make eval-replay      # Track R + schema validation. No model, no database.
docker compose up -d postgres
NINE_EVAL_MODELS=claude-haiku-4-5-20251001 make eval-live   # Track L matrix
make eval-generate    # re-record the committed Track-R fixtures (needs Postgres)
```

The runner drives turns **in-process**: it stands up the real daemon (the
production wiring from `cmd/nine/daemon.go`) over a schema-per-run Postgres store
and an ephemeral workspace, so a case sees a clean world and grading reads the
same durable journal (`session_events`) production writes.

### Environment

| Var | Purpose |
|-----|---------|
| `NINE_EVALS_LIVE=1` | enable the Track-L matrix (`TestLiveMatrix`) |
| `NINE_EVAL_MODELS` | comma-separated model list for the matrix |
| `NINE_EVAL_TIER` | run only one tier (e.g. `smoke`) |
| `NINE_PLUGINS_BIN` | plugin binary dir (default from `make eval-live`) |
| `NINE_TEST_DATABASE_URL` | eval Postgres DSN (default docker-compose) |
| `ANTHROPIC_API_KEY` | for `claude-*` matrix models |
| `NINE_LLM_ENDPOINT` | Ollama endpoint for local matrix models |

## Adding a case

Write a `cases/<id>.yaml` per the schema in docs/evals.md §2, forcing the target
behavior via `prompts` and asserting with the **strongest** available check
(side-effect > trajectory > answer). For a Track-R case, record its fixture once
(`make eval-generate`, or capture a live run's journal).
