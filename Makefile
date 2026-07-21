BINARY   := nine
CMD      := ./cmd/nine
PLUGINS  := shell files http time
DIST     := dist
BIN_DIR  := $(DIST)/bin

GO       := go
GOFLAGS  := -mod=vendor

# Release version, derived from git tags. On a tagged commit this is e.g.
# "v0.1.0"; between tags it's "v0.1.0-3-gabc123" (and "-dirty" with local
# changes). Falls back to "dev" outside a git checkout. Injected into the
# binary via -ldflags (see build).
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -ldflags "-X main.Version=$(VERSION)"

.PHONY: all dev build plugins test test-v lint cover cover-html clean model compose-prod compose-dev compose-session compose-shell compose-logs compose-down compose-destroy integration-test integration-test-short

dev: build plugins browser-plugin

all: dev

# ── core binary ───────────────────────────────────────────────────────────────

build:
	@mkdir -p $(DIST)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(DIST)/$(BINARY) $(CMD)

# ── plugin binaries ───────────────────────────────────────────────────────────

plugins: $(addprefix $(BIN_DIR)/,$(PLUGINS))

$(BIN_DIR)/%: FORCE
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GOFLAGS) -o $@ ./plugins/$*

FORCE:

# ── browser plugin ────────────────────────────────────────────────────────────

browser-plugin: $(BIN_DIR)/browser

$(BIN_DIR)/browser: FORCE
	@mkdir -p $(BIN_DIR)
	cd plugins/browser && npm install
	cd plugins/browser && npx playwright install chromium
	cd plugins/browser && sh build.sh ../../$(BIN_DIR)/browser

.PHONY: browser-plugin

# ── tests ─────────────────────────────────────────────────────────────────────

test:
	$(GO) test $(GOFLAGS) ./...

test-v:
	$(GO) test $(GOFLAGS) -v ./...

# ── coverage ──────────────────────────────────────────────────────────────────

cover:
	@mkdir -p $(DIST)
	$(GO) test $(GOFLAGS) -coverprofile=$(DIST)/coverage.out ./...
	$(GO) tool cover -func=$(DIST)/coverage.out | tail -1

cover-html: cover
	$(GO) tool cover -html=$(DIST)/coverage.out

# ── lint ──────────────────────────────────────────────────────────────────────

lint:
	golangci-lint run ./...

# ── docker-compose deployment ─────────────────────────────────────────────────
# docker-compose is the way to run Nine: Postgres (pgvector) plus the daemon in
# one of two profiles — `prod` (the built runtime image) or `dev` (the Go
# toolchain hot-reloading the mounted source). The LLM knobs below default for a
# local Ollama and are passed through to the compose services.

NINE_LLM_PROVIDER  ?= ollama
NINE_LLM_MODEL     ?= gemma4:e2b
NINE_LLM_ENDPOINT  ?= http://host.docker.internal:11434

LLM_ENV = NINE_LLM_PROVIDER=$(NINE_LLM_PROVIDER) NINE_LLM_MODEL=$(NINE_LLM_MODEL) NINE_LLM_ENDPOINT=$(NINE_LLM_ENDPOINT)

# The 32k context window is requested per-call via num_ctx (nine.toml), so the
# stock gemma4:e2b is all that's needed — no custom Modelfile.
model:
	ollama pull gemma4:e2b

compose-prod:
	$(LLM_ENV) docker compose --profile prod up -d --build --wait
	@echo "nine (production) is up. Attach a session with: make compose-session"

compose-dev:
	$(LLM_ENV) docker compose --profile dev up -d --build
	@echo "nine (hot-reload) is building/starting. Follow it with: docker compose logs -f nine-dev"

compose-session:
	@docker exec -it nine nine 2>/dev/null || docker exec -it nine-dev nine

compose-shell:
	@docker exec -it nine sh 2>/dev/null || docker exec -it nine-dev sh

compose-logs:
	@docker compose logs -f nine 2>/dev/null || docker compose logs -f nine-dev

# Stop the stack. Named volumes (Postgres data, workspace) are kept, so a later
# `make compose-prod` comes back up with all state intact.
compose-down:
	docker compose --profile prod --profile dev down

# Completely remove Nine: stop and delete every container, network, named volume
# (all data — the database and workspace are wiped), and the locally built nine
# images. Destructive and irreversible.
compose-destroy:
	docker compose --profile prod --profile dev down --volumes --remove-orphans
	-docker image rm nine nine-dev

# ── integration tests ────────────────────────────────────────────────────────
# Requires: Docker running, Ollama on localhost:11434 with NINE_LLM_MODEL loaded.
# Override model: NINE_LLM_MODEL=llama3 make integration-test

integration-test:
	NINE_INTEGRATION=1 $(GO) test $(GOFLAGS) -v -timeout 600s -count=1 ./tests/integration/...

integration-test-short:
	NINE_INTEGRATION=1 $(GO) test $(GOFLAGS) -v -timeout 300s -count=1 \
		-run 'TestDaemonResponds|TestPluginsLoaded|TestTimeTool|TestShellToolEcho|TestShellBlocksRecursiveDeletion|TestShellAllowsSafeCommands' \
		./tests/integration/...

# ── clean ─────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(DIST)
