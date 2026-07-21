# Installation

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Docker | 24+ | Required — docker compose runs the whole stack |
| Go | 1.26+ | For development (build from source) |
| Node.js | 18+ | For development (browser plugin) |
| golangci-lint | Latest | Optional, for `make lint` |

An LLM provider is also required — see [Configuration](configuration.md) for options.

---

## Deployment (docker compose)

docker compose is the way to deploy Nine: it runs the whole stack — the
PostgreSQL (`pgvector`) database plus the daemon — with the daemon in one of two
**profiles**. Postgres always comes up; each daemon service reaches it over the
compose network as `postgres:5432` (the `NINE_DATABASE_URL` override), and reaches
the host's LLM endpoint via `host.docker.internal`.

The runtime image is a minimal Alpine image with the compiled `nine` binary and
plugins baked in — no Go toolchain, no source tree (Nine does not compile anything
at runtime).

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
change** (via `inotifywait`). This is the containerized development loop:

```bash
make compose-dev
# = docker compose --profile dev up -d --build
docker compose logs -f nine-dev   # watch builds + daemon output
make compose-session              # attaches to nine-dev if nine isn't running
```

The hot-reload container (`nine-dev`) uses its own `/data` volume and a cached Go
build volume; the two modes are separate services and are not meant to run at once.
LLM knobs (`NINE_LLM_PROVIDER`/`MODEL`/`ENDPOINT`) pass through to the services
(empty = use `nine.toml`). The browser plugin (Chromium + Node) is present only in
the production image, not the hot-reload container.

### Volume layout

The `nine` binary (with built-in skills embedded), the compiled plugins, and the
browser plugin code are immutable image content under `/opt/nine` — they are **not**
stored in a volume. Named volumes hold only mutable state:

- `nine-pgdata` — PostgreSQL data: conversations, tasks, goals, KV, skills,
  vectors, and the session event journal
- `nine-data` (prod) / `nine-dev-data` (dev) — the files-plugin workspace at
  `/data/workspace`

On first boot `entrypoint.sh` creates `/data/workspace` and the daemon seeds the
built-in skills (embedded in the binary) into Postgres; this runs every boot, so
editing a skill file and rebuilding updates it, while agent-authored skills are
left untouched. Rebuilding the image picks up code changes without touching the
volumes.

### Stop

```bash
make compose-down   # docker compose --profile prod --profile dev down
```

This stops and removes the containers but **keeps the named volumes**, so the next
`make compose-prod` comes back up with all state intact.

### Completely remove Nine

To tear everything down — containers, network, **all data volumes**, and the
locally built images:

```bash
make compose-destroy
# = docker compose --profile prod --profile dev down --volumes --remove-orphans
#   then docker image rm nine nine-dev
```

This is destructive and irreversible: the database and workspace are wiped.

---

## Development (build from source)

For working on Nine itself, build and run the binary natively. Deployment is
docker compose (above); this section is about compiling, running, and testing the
code from a checkout. For a containerized development loop instead, use
[hot-reload mode](#hot-reload-mode).

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

Postgres holds all durable state and the daemon fails fast without it. Bring up
just the database from the compose file (no profile starts only Postgres):

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

## Makefile Targets

**Build & test**

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
| `make model` | Pull the default Ollama model (`gemma4:e2b`) |
| `make integration-test` | Run integration tests (requires Docker + Ollama) |
| `make clean` | Remove `dist/` |

**Deploy (docker compose)**

| Target | Description |
|--------|-------------|
| `make compose-prod` | Deploy the stack in production mode (built image) |
| `make compose-dev` | Deploy the stack in hot-reload mode (rebuild on `.go` change) |
| `make compose-session` | Open an interactive TUI session in the running container |
| `make compose-shell` | Open a shell in the running container |
| `make compose-logs` | Follow the daemon logs |
| `make compose-down` | Stop the stack, keeping all data volumes |
| `make compose-destroy` | Remove the stack **and all data volumes and images** |

---

## Verifying the Installation

**Deployed with compose** — attach a session and check status:

```bash
make compose-session
nine status
```

**Native build** — the daemon auto-starts on first use:

```bash
./dist/nine status
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
