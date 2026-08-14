BINARY   := nine
CMD      := ./cmd/nine
DIST     := dist
# BIN_DIR holds plugins that ship as their own artifacts. Since the Go built-ins
# (shell/files/http/time) moved into the nine binary as `nine plugin serve
# <name>` (internal/builtins), that is the browser plugin alone.
BIN_DIR  := $(DIST)/bin

GO       := go
GOFLAGS  := -mod=vendor

# Release version, derived from git tags. On a tagged commit this is e.g.
# "v0.1.0"; between tags it's "v0.1.0-3-gabc123" (and "-dirty" with local
# changes). Falls back to "dev" outside a git checkout. Injected into the
# binary via -ldflags (see build).
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -ldflags "-X main.Version=$(VERSION)"

.PHONY: all dev build test test-v lint cover cover-html clean model up up-hot session shell logs down destroy integration-test integration-test-short eval-replay eval-live eval-generate quickjs-wasm quickjs-verify

dev: build browser-plugin

all: dev

# ── core binary ───────────────────────────────────────────────────────────────
# This also builds the shell/files/http/time plugins: they are served out of the
# nine binary itself (`nine plugin serve <name>`), so there is nothing else to
# build or ship for them.

build:
	@mkdir -p $(DIST)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(DIST)/$(BINARY) $(CMD)

FORCE:

# ── browser plugin ────────────────────────────────────────────────────────────

browser-plugin: $(BIN_DIR)/browser

$(BIN_DIR)/browser: FORCE
	@mkdir -p $(BIN_DIR)
	cd plugins/browser && npm install
	cd plugins/browser && npx playwright install chromium
	cd plugins/browser && sh build.sh ../../$(BIN_DIR)/browser

.PHONY: browser-plugin

# ── sandboxed tools: the QuickJS blob ─────────────────────────────────────────
# Deliberately NOT part of `dev` or `build` (docs/sandboxed-tools.md §10.1). The
# interpreter is built from pinned tags and committed, so an ordinary build needs
# no wasi-sdk, no clang, and no clone — and the runtime image gains no toolchain.
# Run this only on a deliberate version bump, and review the qjs.wasm diff and
# the new hash in that PR.

quickjs-wasm:
	sh internal/toolvm/quickjs/build.sh

# Verify the committed blob against its recorded hash. A binary artifact in the
# tree is only acceptable if changing it is loud; this is what makes it loud in
# CI, where it runs on every build.
quickjs-verify:
	cd internal/toolvm/quickjs && shasum -a 256 -c qjs.wasm.sha256

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

# ── container deployment ──────────────────────────────────────────────────────
# The container runs the daemon alone under s6-overlay (docs/single-container.md)
# — its database is a file on the /data volume, so there is no second service to
# orchestrate and no compose stack. Just `docker run`, wrapped below for the
# env/volume/flag boilerplate. `up` builds and runs the immutable runtime image;
# `up-hot` builds and runs the hot-reload image (Go toolchain over the
# bind-mounted source, rebuilding and restarting the daemon on any .go change).

NINE_LLM_PROVIDER  ?= ollama
NINE_LLM_MODEL     ?= qwen3.5:4b
NINE_LLM_ENDPOINT  ?= http://host.docker.internal:11434

LLM_ENV = -e NINE_LLM_PROVIDER=$(NINE_LLM_PROVIDER) -e NINE_LLM_MODEL=$(NINE_LLM_MODEL) -e NINE_LLM_ENDPOINT=$(NINE_LLM_ENDPOINT)

# Flags common to both containers. The mounted nine.toml is written for the
# native layout; these override the paths that differ inside the container
# (see internal/config.ApplyEnvOverrides). The database needs no override: it
# defaults to /data/nine.db whenever the /data volume is present.
NINE_ENV = \
	-e NINE_CONFIG=/nine.toml \
	-e NINE_PLUGINS_BIN=/opt/nine/bin \
	-e NINE_WORKSPACE_ROOT=/data/workspace \
	-e NINE_SKILLS_USER_DIR=/skills.d \
	-e NINE_PLUGINS_USER_DIR=/plugins.d \
	-e NINE_TOOLS_USER_DIR=/tools.d \
	-e NINE_LOG_FILE=off
NINE_MOUNTS = \
	-v $(CURDIR)/nine.toml:/nine.toml:ro \
	-v $(CURDIR)/skills.d:/skills.d:ro \
	-v $(CURDIR)/plugins.d:/plugins.d:ro \
	-v $(CURDIR)/tools.d:/tools.d:ro
NINE_RUN_FLAGS = --add-host host.docker.internal:host-gateway --restart unless-stopped

# The 32k context window is requested per-call via num_ctx (nine.toml), so the
# stock qwen3.5:4b is all that's needed — no custom Modelfile.
model:
	ollama pull qwen3.5:4b

up:
	docker build --target runtime -t nine .
	-docker rm -f nine 2>/dev/null
	docker run -d --name nine \
	  $(NINE_RUN_FLAGS) $(LLM_ENV) $(NINE_ENV) $(NINE_MOUNTS) \
	  -v nine-data:/data \
	  nine
	@echo "nine is up. Attach a session with: make session"

up-hot:
	docker build --target dev -t nine-dev .
	-docker rm -f nine-dev 2>/dev/null
	docker run -d --name nine-dev \
	  $(NINE_RUN_FLAGS) $(LLM_ENV) $(NINE_ENV) $(NINE_MOUNTS) \
	  -v $(CURDIR):/nine-src \
	  -v nine-dev-data:/data \
	  -v nine-dev-gocache:/root/.cache/go-build \
	  nine-dev
	@echo "nine (hot-reload) is building/starting. Follow it with: make logs"

session:
	@docker exec -it nine nine 2>/dev/null || docker exec -it nine-dev nine

shell:
	@docker exec -it nine sh 2>/dev/null || docker exec -it nine-dev sh

logs:
	@docker logs -f nine 2>/dev/null || docker logs -f nine-dev

# Stop the container. Named volumes (database, workspace) are kept, so a
# later `make up` / `make up-hot` comes back up with all state intact.
down:
	-docker rm -f nine nine-dev

# Completely remove Nine: the container, every named volume (all data — the
# database and workspace are wiped), and the locally built images. Destructive
# and irreversible.
destroy: down
	-docker volume rm nine-data nine-dev-data nine-dev-gocache
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

# ── evals ─────────────────────────────────────────────────────────────────────
# Two tracks (docs/evals.md). eval-replay is deterministic and infra-free — the
# every-PR gate. eval-live runs the model matrix and needs a built nine binary
# (make build), and a model list.

# Track R + schema validation: no live model, no database. Fast, deterministic.
eval-replay:
	$(GO) test $(GOFLAGS) -count=1 -run 'TestCasesValidate|TestReplayFixtures' ./tests/evals/

# Track L: the live model matrix. Needs a built nine binary, which is also where
# the shell/files/http/time plugins now live. Override the models with
# NINE_EVAL_MODELS.
#   NINE_EVAL_MODELS=qwen3.5:4b,qwen3.5:9b make eval-live
eval-live: build
	NINE_EVALS_LIVE=1 \
	NINE_BINARY=$(abspath $(DIST)/$(BINARY)) \
	$(GO) test $(GOFLAGS) -v -count=1 -timeout 1800s -run TestLiveMatrix ./tests/evals/

# Regenerate the committed Track-R fixtures from scripted runs.
eval-generate:
	NINE_EVALS_GENERATE=1 $(GO) test $(GOFLAGS) -count=1 -run TestGenerateSeedFixtures ./tests/evals/runner/

# ── clean ─────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(DIST)
