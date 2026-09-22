BINARY   := nine
CMD      := ./cmd/nine
DIST     := dist
# BIN_DIR holds plugins that ship as their own artifacts. The Go built-ins
# (shell/files/http/time) are served out of the nine binary as `nine plugin
# serve <name>` (internal/builtins), and browser automation is now an ordinary
# MCP server declared in nine.toml (docs/browser.md) rather than a plugin we
# build — so nothing here builds into it. It stays as the directory a user
# plugin drops its binary in ([plugins].bin).
BIN_DIR  := $(DIST)/bin

GO       := go
GOFLAGS  := -mod=vendor

# Release version, derived from git tags. On a tagged commit this is e.g.
# "v0.1.0"; between tags it's "v0.1.0-3-gabc123" (and "-dirty" with local
# changes). Falls back to "dev" outside a git checkout. Injected into the
# binary via -ldflags (see build).
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -ldflags "-X main.Version=$(VERSION)"

.PHONY: all dev build openapi openapi-check test test-v lint cover cover-html clean model up up-hot session shell logs down destroy integration-test integration-test-short eval-replay eval-live eval-generate quickjs-wasm quickjs-verify

dev: build

all: dev

# ── core binary ───────────────────────────────────────────────────────────────
# This also builds the shell/files/http/time plugins: they are served out of the
# nine binary itself (`nine plugin serve <name>`), so there is nothing else to
# build or ship for them.

build:
	@mkdir -p $(DIST)
	$(GO) build $(GOFLAGS) $(LDFLAGS) -o $(DIST)/$(BINARY) $(CMD)

# Regenerate the API models and server interface from internal/api/openapi.yaml.
# The document is the source of truth (docs/api.md): a handler that disagrees
# with it fails to compile. The generator lives in the tools module so its
# dependencies stay out of nine's own graph and out of vendor/.
openapi:
	go -C tools run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen \
		-config ../internal/api/oapi-codegen.yaml ../internal/api/openapi.yaml

# Fail if the committed generated code does not match the document. Run in CI:
# without it the two drift apart silently, which is the failure the spec-first
# layout exists to prevent.
openapi-check: openapi
	@git diff --exit-code -- internal/api/apigen || { \
		echo ""; \
		echo "internal/api/apigen is stale — run 'make openapi' and commit the result."; \
		exit 1; \
	}

FORCE:

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

# The per-package timeout is explicit rather than Go's 10m default. internal/toolvm
# runs ~4 minutes on its own — it compiles wasm modules — so the default leaves
# little headroom, and a loaded machine has blown through it. A killed package
# reports a goroutine dump rather than a test failure, which is the least useful
# way to learn a suite is slow. Override for a slow host:
#   NINE_TEST_TIMEOUT=30m make test
NINE_TEST_TIMEOUT ?= 20m
test:
	$(GO) test $(GOFLAGS) -timeout $(NINE_TEST_TIMEOUT) ./...

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
# The container runs the daemon alone under s6-overlay (adr/single-container.md)
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
# None of the three user directories is mounted. The env vars above wire the
# paths — /skills.d, /plugins.d, /tools.d — and an operator who wants any of them
# adds their own `-v /my/skills.d:/skills.d:ro`, getting exactly what they chose.
#
# Mounting the repo's copies would install whatever they happen to contain on
# every `make up`: sandboxed tools become live capability the model is offered,
# and skills become instructions the agent is seeded with. That is not a decision
# this Makefile should be making on an operator's behalf, and it is the reason
# all three directories ship empty. Worked tool examples are in examples/tools/.
#
# For plugins there is a second reason: a plugin is an executable, and one built
# on the host does not run in this debian container unless it happens to be a
# linux binary for the same architecture. Mounting the host's plugins.d offered
# an arrangement that mostly could not work.
NINE_MOUNTS = \
	-v $(CURDIR)/nine.toml:/nine.toml:ro
NINE_RUN_FLAGS = --add-host host.docker.internal:host-gateway --restart unless-stopped

# The 32k context window is requested per-call via num_ctx (nine.toml), so the
# stock qwen3.5:4b is all that's needed — no custom Modelfile.
model:
	ollama pull qwen3.5:4b

up:
	docker build --target runtime -t nine .
	-docker rm -f nine 2>/dev/null
	docker run -d --name nine \
	  -p 8080:8080 \
	  $(NINE_RUN_FLAGS) $(LLM_ENV) $(NINE_ENV) $(NINE_MOUNTS) \
	  -v nine-data:/data \
	  nine
	@echo "nine is up. API at http://localhost:8080. Attach a session with: make session"

up-hot:
	docker build --target dev -t nine-dev .
	-docker rm -f nine-dev 2>/dev/null
	docker run -d --name nine-dev \
	  -p 8080:8080 \
	  --env-file .env \
	  $(NINE_RUN_FLAGS) $(NINE_ENV) $(NINE_MOUNTS) \
	  -v $(CURDIR):/nine-src \
	  -v nine-dev-data:/data \
	  -v nine-dev-gocache:/root/.cache/go-build \
	  nine-dev
	@echo "nine (hot-reload) is building/starting. API at http://localhost:8080. Follow it with: make logs"

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
#
# The timeout is per *matrix*, not per case, so it scales with the model list:
# the 18-case suite takes ~33 min for one small local model, and the previous
# 1800s bound cut that off 9% short of the finish — with the whole run lost,
# because the grid renders only at the end. NINE_EVAL_TIMEOUT overrides it for a
# long matrix (several models, or a slower host).
NINE_EVAL_TIMEOUT ?= 7200s
eval-live: build
	NINE_EVALS_LIVE=1 \
	NINE_BINARY=$(abspath $(DIST)/$(BINARY)) \
	$(GO) test $(GOFLAGS) -v -count=1 -timeout $(NINE_EVAL_TIMEOUT) -run TestLiveMatrix ./tests/evals/

# Regenerate the committed Track-R fixtures from scripted runs.
eval-generate:
	NINE_EVALS_GENERATE=1 $(GO) test $(GOFLAGS) -count=1 -run TestGenerateSeedFixtures ./tests/evals/runner/

# ── clean ─────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(DIST)
