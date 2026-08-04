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

.PHONY: all dev build plugins test test-v lint cover cover-html clean model up up-hot session shell logs down destroy pgadmin pgadmin-down pg pg-down integration-test integration-test-short eval-replay eval-live eval-generate

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

# ── container deployment ──────────────────────────────────────────────────────
# Nine cannot be decoupled from its database (memory.Open fails fast if
# Postgres is unreachable), so the container runs both under s6-overlay as one
# unit (docs/single-container.md) instead of a docker-compose stack — just
# `docker run`, wrapped below for the env/volume/flag boilerplate. `up` builds
# and runs the immutable runtime image; `up-hot` builds and runs the hot-reload
# image (Go toolchain over the bind-mounted source, rebuilding and restarting
# the daemon on any .go change). pgAdmin is not part of either image — spawn it
# on demand with `make pgadmin`.

NINE_LLM_PROVIDER  ?= ollama
NINE_LLM_MODEL     ?= qwen3.5:4b
NINE_LLM_ENDPOINT  ?= http://host.docker.internal:11434

LLM_ENV = -e NINE_LLM_PROVIDER=$(NINE_LLM_PROVIDER) -e NINE_LLM_MODEL=$(NINE_LLM_MODEL) -e NINE_LLM_ENDPOINT=$(NINE_LLM_ENDPOINT)

# Flags common to both containers. The mounted nine.toml is written for the
# native layout; these override the paths that differ inside the container
# (see internal/config.ApplyEnvOverrides) — NINE_DATABASE_URL needs no such
# override since it's already baked into the image, pointing at the co-located
# Postgres.
NINE_ENV = \
	-e NINE_CONFIG=/nine.toml \
	-e NINE_PLUGINS_BIN=/opt/nine/bin \
	-e NINE_WORKSPACE_ROOT=/data/workspace \
	-e NINE_SKILLS_USER_DIR=/skills.d \
	-e NINE_PLUGINS_USER_DIR=/plugins.d \
	-e NINE_LOG_FILE=off
NINE_MOUNTS = \
	-v $(CURDIR)/nine.toml:/nine.toml:ro \
	-v $(CURDIR)/skills.d:/skills.d:ro \
	-v $(CURDIR)/plugins.d:/plugins.d:ro
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
	  -v nine-pgdata:/var/lib/postgresql/data \
	  -v nine-data:/data \
	  nine
	@echo "nine is up. Attach a session with: make session"

up-hot:
	docker build --target dev -t nine-dev .
	-docker rm -f nine-dev 2>/dev/null
	docker run -d --name nine-dev \
	  $(NINE_RUN_FLAGS) $(LLM_ENV) $(NINE_ENV) $(NINE_MOUNTS) \
	  -v $(CURDIR):/nine-src \
	  -v nine-dev-pgdata:/var/lib/postgresql/data \
	  -v nine-dev-data:/data \
	  -v nine-dev-gocache:/root/.cache/go-build \
	  -p 5432:5432 \
	  nine-dev
	@echo "nine (hot-reload) is building/starting. Follow it with: make logs"

session:
	@docker exec -it nine nine 2>/dev/null || docker exec -it nine-dev nine

shell:
	@docker exec -it nine sh 2>/dev/null || docker exec -it nine-dev sh

logs:
	@docker logs -f nine 2>/dev/null || docker logs -f nine-dev

# Stop the container. Named volumes (Postgres data, workspace) are kept, so a
# later `make up` / `make up-hot` comes back up with all state intact.
down:
	-docker rm -f nine nine-dev

# Completely remove Nine: the container, every named volume (all data — the
# database and workspace are wiped), and the locally built images. Destructive
# and irreversible.
destroy: down
	-docker volume rm nine-pgdata nine-data nine-dev-pgdata nine-dev-data nine-dev-gocache
	-docker image rm nine nine-dev

# pgAdmin is opt-in tooling, not part of either Nine image — spawn it on demand
# against whichever container publishes Postgres (nine-dev does by default via
# `up-hot`; add `-p 5432:5432` to `up`'s docker run if you want it against a
# production DB instead). Desktop mode: no login, no master password — local
# convenience only, never expose this beyond localhost.
pgadmin:
	docker run -d --name nine-pgadmin \
	  --add-host host.docker.internal:host-gateway \
	  -e PGADMIN_DEFAULT_EMAIL=nine@nine.dev \
	  -e PGADMIN_DEFAULT_PASSWORD=nine \
	  -e PGADMIN_CONFIG_SERVER_MODE=False \
	  -e PGADMIN_CONFIG_MASTER_PASSWORD_REQUIRED=False \
	  -v $(CURDIR)/docker/pgadmin-servers.json:/pgadmin4/servers.json:ro \
	  -p 5050:80 dpage/pgadmin4:latest
	@echo "pgAdmin on http://localhost:5050 (pre-wired to the nine database)"

pgadmin-down:
	-docker rm -f nine-pgadmin

# Postgres for a native `./dist/nine daemon`, evals, and integration tests —
# matches nine.toml's default DSN (localhost:5433). Unrelated to the
# containerized Nine above, which runs its own Postgres internally.
pg:
	docker run -d --name nine-pg \
	  -e POSTGRES_USER=nine -e POSTGRES_PASSWORD=nine -e POSTGRES_DB=nine \
	  -p 5433:5432 -v nine-pg-native:/var/lib/postgresql/data \
	  pgvector/pgvector:pg17

pg-down:
	-docker rm -f nine-pg

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
# every-PR gate. eval-live runs the model matrix and needs Postgres, plugin
# binaries (make plugins), and a model list.

# Track R + schema validation: no live model, no database. Fast, deterministic.
eval-replay:
	$(GO) test $(GOFLAGS) -count=1 -run 'TestCasesValidate|TestReplayFixtures' ./tests/evals/

# Track L: the live model matrix. Needs Postgres up (make pg) and plugin
# binaries. Override the models with NINE_EVAL_MODELS.
#   NINE_EVAL_MODELS=claude-haiku-4-5-20251001 make eval-live
eval-live: plugins
	NINE_EVALS_LIVE=1 \
	NINE_PLUGINS_BIN=$(abspath $(BIN_DIR)) \
	$(GO) test $(GOFLAGS) -v -count=1 -timeout 1800s -run TestLiveMatrix ./tests/evals/

# Regenerate the committed Track-R fixtures from scripted runs (needs Postgres).
eval-generate:
	NINE_EVALS_GENERATE=1 $(GO) test $(GOFLAGS) -count=1 -run TestGenerateSeedFixtures ./tests/evals/runner/

# ── clean ─────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(DIST)
