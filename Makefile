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

.PHONY: all dev build plugins test test-v lint cover cover-html clean docker docker-run docker-stop docker-session docker-up docker-logs compose-prod compose-dev compose-session compose-down integration-test integration-test-short

dev: build plugins browser-plugin

all: dev docker

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

# ── docker ────────────────────────────────────────────────────────────────────

NINE_LLM_PROVIDER  ?= ollama
NINE_LLM_MODEL     ?= gemma4:e2b
NINE_LLM_ENDPOINT  ?= http://host.docker.internal:11434

# Container overrides for the paths and endpoints that differ from the native
# layout, so a single nine.toml serves both (see internal/config.ApplyEnvOverrides).
NINE_DATABASE_URL   ?= postgres://nine:nine@host.docker.internal:5433/nine?sslmode=disable
NINE_PLUGINS_BIN    ?= /opt/nine/bin
NINE_WORKSPACE_ROOT ?= /data/workspace

# The 32k context window is requested per-call via num_ctx (nine.toml), so the
# stock gemma4:e2b is all that's needed — no custom Modelfile.
model:
	ollama pull gemma4:e2b

docker:
	docker build -t nine .

docker-run:
	@docker image inspect nine >/dev/null 2>&1 || $(MAKE) docker
	docker compose up -d --wait postgres
	docker run -d \
	  --name nine \
	  -v nine-data:/data \
	  -v $(PWD)/nine.toml:/nine.toml:ro \
	  -e NINE_LLM_PROVIDER=$(NINE_LLM_PROVIDER) \
	  -e NINE_LLM_MODEL=$(NINE_LLM_MODEL) \
	  -e NINE_LLM_ENDPOINT=$(NINE_LLM_ENDPOINT) \
	  -e NINE_DATABASE_URL=$(NINE_DATABASE_URL) \
	  -e NINE_PLUGINS_BIN=$(NINE_PLUGINS_BIN) \
	  -e NINE_WORKSPACE_ROOT=$(NINE_WORKSPACE_ROOT) \
	  nine
	@echo "waiting for the nine daemon to be ready..."
	@for i in $$(seq 1 120); do \
	  docker exec nine test -S /tmp/nine.sock 2>/dev/null && { echo "daemon ready"; exit 0; }; \
	  sleep 0.5; \
	done; \
	echo "daemon did not become ready in 60s; check 'make docker-logs'"; exit 1

docker-stop:
	docker rm -f nine 2>/dev/null || true
	docker volume rm nine-data 2>/dev/null || true

docker-logs:
	@echo "=== container stdout/stderr ==="
	@docker logs nine 2>&1 || true
	@echo "=== nine daemon log (live) ==="
	docker exec nine tail -f /usr/local/bin/nine.log

docker-session:
	@docker inspect -f '{{.State.Running}}' nine 2>/dev/null | grep -q true || \
	  { echo "nine container is not running — start it with: make docker-run"; exit 1; }
	docker exec -it nine nine

# Build everything, then stop+wipe any prior container/volume, run a fresh
# container, and open a session — one command to go from source to a clean
# interactive session for testing.
docker-up:
	$(MAKE) all
	$(MAKE) docker-stop
	$(MAKE) docker-run
	$(MAKE) docker-session

# ── docker-compose deployment (Postgres + one daemon mode) ────────────────────
# Production: the built runtime image. Hot-reload: Go toolchain over the mounted
# source, rebuilding the daemon on .go changes. LLM knobs default as above and
# are passed through to the compose services.
LLM_ENV = NINE_LLM_PROVIDER=$(NINE_LLM_PROVIDER) NINE_LLM_MODEL=$(NINE_LLM_MODEL) NINE_LLM_ENDPOINT=$(NINE_LLM_ENDPOINT)

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

compose-down:
	docker compose --profile prod --profile dev down

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
