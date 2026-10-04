# Roadmap

Planned work, undated, landing in whatever order makes sense. Shipped items are removed
from this table rather than marked done, so what is listed here is what is still missing.

| Item | Status | Detail |
|------|--------|--------|
| Hardening | Planned | Nine is not hardened. Only sandboxed tools run behind a real boundary; the `shell` plugin and native plugins run as the daemon's process user. See the README's Limits table for what that means today. |
| Remote access | Partial | The REST API serves every operation it declares, streams over SSE and WebSocket, and ships with bearer-token auth and TLS ([api.md](api.md)). It is a translation layer over the local Unix socket, so the server runs on the daemon's host; there is no multi-host story, and a browser cannot authenticate a WebSocket without a proxy that adds the header. |
| Model routing | Planned | Route different work to different models in one deployment. Nine uses one model at a time. |
| More LLM backends | Partial | Mistral is supported. llama.cpp and vLLM both speak an OpenAI-compatible API, so one adapter covers them. |
| Richer sandboxed tools | Planned | FS and env gaps, runtime wasm grants, binary data, missing JS globals, HTTP audit, secret sharing, structured tool errors. |
| TUI improvements | Partial | Slash-command views are read-only, and the journal, notifications and moving background sessions have no place in the TUI. Migrated to charm.land v2 (bubbletea, lipgloss, bubbles, glamour). |
| More built-in plugins | Planned | — |
| Codebase improvements | Planned | Refactoring and performance work — [`adr/codebase-improvement.md`](../adr/codebase-improvement.md). |
| Fix eval-runner daemon hang | Planned | `tests/evals/runner` spins up a real in-process daemon per test and intermittently deadlocks under CI load on a turn whose reply never arrives. Excluded from `make ci` until fixed; `make eval-replay` still runs. |

## Limits

| Limit | Detail |
|-------|--------|
| Undated and non-binding | No item here carries a date or a commitment. Order changes with whatever makes sense next. |
| Not a changelog | Shipped work leaves this page rather than accumulating on it. Read the [releases](https://github.com/djordlucas/nine/releases) for what landed. |
