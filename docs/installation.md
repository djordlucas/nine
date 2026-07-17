# Installation

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Go | 1.26+ | Required for native build |
| Node.js | 18+ | Required for browser plugin |
| Docker | 24+ | Required for container build |
| golangci-lint | Latest | Optional, for `make lint` |

An LLM provider is also required — see [Configuration](configuration.md) for options.

---

## Option 1: Build from Source (Native)

This runs Nine directly on your machine. Useful for development.

### 1. Clone and build

```bash
git clone https://github.com/djordlucas/nine
cd nine

# Build the nine binary, all default plugin binaries, and the browser plugin
make all
```

The build produces:
- `dist/nine` — the main CLI/daemon binary
- `dist/bin/shell`, `dist/bin/files`, etc. — default plugin binaries
- `dist/bin/browser` — browser plugin launcher (requires Node.js + npm)

### 2. Config

The repo's `nine.toml` is the only config file, and it is written for exactly this
layout — a local Ollama, plugins in `./dist/bin`, and the compose Postgres on
`localhost:5433`. Running from the project root needs no edits.

To use it from anywhere, copy it to the global location and make the paths absolute:

```bash
mkdir -p ~/.nine
cp nine.toml ~/.nine/nine.toml
```

Nine searches `$NINE_CONFIG`, then `./nine.toml`, then `/nine.toml`, then
`~/.nine/nine.toml`. See [Configuration](configuration.md) for all options.

### 3. Start PostgreSQL

Postgres holds all durable state and the daemon fails fast without it:

```bash
docker compose up -d          # pgvector on localhost:5433
```

### 4. Pull a model and run

```bash
ollama pull gemma4:e2b
./dist/nine "Hello"
```

The daemon starts automatically and stays running in the background.

To use Anthropic instead, set `provider = "anthropic"` and a `claude-*` model in
`nine.toml`, then export your key:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
```

---

## Option 2: Docker (Recommended for Production)

The runtime image is a minimal Alpine image with the compiled `nine` binary and
plugins — no Go toolchain, no source tree (Nine does not compile anything at
runtime). This section covers a single `docker run` container; for the full
Postgres + daemon stack, see [Option 3: docker compose](#option-3-docker-compose).

### 1. Build the image

```bash
make docker
# produces nine:latest
```

### 2. Run

```bash
make docker-run
```

This is equivalent to:

```bash
docker run -d \
  --name nine \
  -v nine-data:/data \
  -v $(PWD)/nine.toml:/nine.toml:ro \
  -e NINE_LLM_PROVIDER=ollama \
  -e NINE_LLM_MODEL=gemma4:e2b \
  -e NINE_LLM_ENDPOINT=http://host.docker.internal:11434 \
  -e NINE_DATABASE_URL=postgres://nine:nine@host.docker.internal:5433/nine?sslmode=disable \
  -e NINE_PLUGINS_BIN=/opt/nine/bin \
  -e NINE_WORKSPACE_ROOT=/data/workspace \
  nine
```

The project's single `nine.toml` is bind-mounted to `/nine.toml` inside the container.
That file is written for the native layout, so the `-e` flags override the four values
that differ in a container — the LLM endpoint, the database DSN, and the plugin and
workspace paths. Edit `nine.toml` and restart the container to change anything else.
You can also override the model at run time without editing any file:

```bash
make docker-run NINE_LLM_MODEL=llama3.2
```

### 3. Open an interactive session

```bash
make docker-session
```

This runs `docker exec -it nine nine` and connects to the running daemon.

### Docker Volume Layout

The `nine` binary (with built-in skills embedded), the compiled plugins, and the
browser plugin code are immutable image content under `/opt/nine` — they are **not**
stored in the volume. The `nine-data` volume holds only mutable state:

```
/data/
└── workspace/      # Files-plugin working directory
```

Persistent state (conversations, tasks, goals, KV, skills, vectors, the session
event journal) lives in PostgreSQL, not the volume — run `docker compose up -d`
alongside the daemon.

### First-Run Initialization

On the first `docker run`, `entrypoint.sh` creates `/data/workspace` and starts the
daemon. On boot the daemon seeds the built-in skills (embedded in the binary) into
the `skills` table in Postgres; this runs every boot, so editing a skill file and
rebuilding updates it, while agent-authored skills are left untouched.

Because the source tree, plugin binaries, and built-in skills live in the image
rather than the volume, rebuilding the image picks up code changes without having to
destroy the volume.

---

## Option 3: docker compose

`docker-compose.yml` runs the whole stack — the PostgreSQL (`pgvector`) database
plus the daemon — with the daemon in one of two **profiles**. Postgres always
comes up; each daemon service reaches it over the compose network as
`postgres:5432` (the `NINE_DATABASE_URL` override), and reaches the host's LLM
endpoint via `host.docker.internal`.

### Production mode

The built runtime image (immutable; no toolchain or source):

```bash
make compose-prod
# = docker compose --profile prod up -d --build --wait
make compose-session          # docker exec -it nine nine
```

### Hot-reload mode

The Go toolchain runs over a bind-mount of the source tree; `docker/dev-entrypoint.sh`
builds `nine` + the Go plugins and **rebuilds and restarts the daemon on any `.go`
change** (via `inotifywait`). Use this for development:

```bash
make compose-dev
# = docker compose --profile dev up -d --build
docker compose logs -f nine-dev   # watch builds + daemon output
make compose-session              # attaches to nine-dev if nine isn't running
```

The hot-reload container (`nine-dev`) uses its own `/data` volume and a cached Go
build volume; the two modes are separate services and are not meant to run at
once. LLM knobs (`NINE_LLM_PROVIDER`/`MODEL`/`ENDPOINT`) pass through the same way
as `make docker-run` (empty = use `nine.toml`).

### Stop

```bash
make compose-down   # docker compose --profile prod --profile dev down
```

The browser plugin (Chromium + Node) is present only in the production image, not
the hot-reload container.

---

## Makefile Targets

| Target | Description |
|--------|-------------|
| `make build` | Compile `dist/nine` binary |
| `make plugins` | Compile all default plugin binaries to `dist/bin/` |
| `make browser-plugin` | Build the browser plugin (requires npm) |
| `make all` | All of the above |
| `make test` | Run all tests |
| `make test-v` | Run tests with verbose output |
| `make cover` | Generate `dist/coverage.out` |
| `make cover-html` | Open HTML coverage report in browser |
| `make lint` | Run golangci-lint |
| `make docker` | Build `nine:latest` Docker image |
| `make docker-run` | Start the nine container |
| `make docker-stop` | Stop and remove the nine container |
| `make docker-session` | Open an interactive TUI session in the container |
| `make docker-logs` | Tail daemon logs from the container |
| `make compose-prod` | Start the compose stack in production mode (built image) |
| `make compose-dev` | Start the compose stack in hot-reload mode (rebuild on change) |
| `make compose-session` | Open an interactive TUI session in the running compose container |
| `make compose-down` | Stop the compose stack (both profiles) |
| `make integration-test` | Run integration tests (requires Docker + Ollama) |
| `make clean` | Remove `dist/` |

---

## Verifying the Installation

```bash
# Check the daemon is running and see loaded plugins
./dist/nine status

# Send a test message
./dist/nine "What tools do you have available?"
```

Expected output from `nine status`:

```
Daemon Status
  Uptime:   2m34s
  Agents:   1 active

Loaded Plugins
  shell, files, http, plugins, skills, nine, time, browser
```
