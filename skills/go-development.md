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
go build -mod=vendor -o bin/<name> ./plugins/<name>
make build        # builds the main binary
make plugins      # builds all plugin binaries
make all          # builds everything
```

### Testing

```bash
go test -mod=vendor ./...            # all tests
go test -mod=vendor ./internal/...   # specific package tree
go test -mod=vendor -v -run TestFoo ./internal/daemon/  # single test
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
| `nine/internal/llm` | Provider-agnostic LLM interface |
| `nine/internal/agent` | ReAct loop and tool dispatcher |
| `nine/internal/daemon` | Unix socket server, runners, client |
| `nine/internal/context` | Token-budget-aware context builder |
| `nine/internal/pluginutil` | Plugin boilerplate |
| `nine/internal/embed` | Embedding interface |

### Common idioms used here

- `slog.Info/Debug/Warn` for structured logging
- `json.RawMessage` for deferred JSON decoding
- Plugin tools return `(string, error)` — JSON-encode structured output
- `context.Context` is always the first parameter for cancellable ops
