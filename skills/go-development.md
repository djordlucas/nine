---
name: go-development
description: Go development patterns for this codebase — build, test, lint, and common idioms
tags: [go, golang, development, testing, build]
---

## Go Development

This project uses Go modules with a vendor directory. Always pass `-mod=vendor` when building or testing.

### Building

```bash
go build -mod=vendor -o nine ./cmd/nine
make build        # builds the main binary — which also serves the
                  # shell/files/http/time plugins (internal/builtins)
make all          # same as make build — nothing else ships as its own artifact
```

### Testing

```bash
go test -mod=vendor ./...            # all tests
go test -mod=vendor ./internal/...   # specific package tree
go test -mod=vendor -v -run TestFoo ./internal/runtime/ # single test
make test
```

### Coverage

```bash
go test -mod=vendor -coverprofile=coverage.out ./...
go tool cover -func=coverage.out | tail -1
make cover
```

### Linting

```bash
golangci-lint run ./...
make lint
```

### Adding dependencies

```bash
go get <module>@<version>
go mod tidy
go mod vendor
```

### Key packages in this repo

| Package | Purpose |
|---------|---------|
| `nine/internal/runtime` | The daemon — Unix-socket server, agent workers, routines |
| `nine/internal/agent` | Agent loop and tool dispatcher |
| `nine/internal/protocol` | Newline-delimited JSON wire protocol between clients and daemon |
| `nine/internal/memory` | SQLite store — checkpoints, goals, workflows, skills, the event journal |
| `nine/internal/llm` | Provider-agnostic LLM interface and request queue |
| `nine/internal/context` | Token-budget-aware context builder (package `ninectx`) |
| `nine/internal/selfmodel` | Assembles the agent's self-description each turn |
| `nine/internal/toolvm` | Sandboxed wasm host for shipped and generated tools |
| `nine/internal/builtins` | Handlers for the built-in plugins and MCP servers |
| `nine/internal/config` | Config parsing (`nine.toml`) |
| `nine/internal/cli`, `nine/internal/tui` | The thin clients |
| `nine/internal/embed` | Embedding interface |

### Common idioms used here

- `slog.Info/Debug/Warn` for structured logging
- `json.RawMessage` for deferred JSON decoding
- Tool handlers return `(string, error)` — JSON-encode structured output
- `context.Context` is always the first parameter for cancellable ops
