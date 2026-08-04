# Installation

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Docker | 24+ | Required — runs the whole stack in one container |
| Go | 1.26+ | For development (build from source) |
| Node.js | 18+ | For development (browser plugin) |
| golangci-lint | Latest | Optional, for `make lint` |

An LLM provider is also required — see [Configuration](configuration.md) for options.

---

## Deployment (single container)

Nine cannot be decoupled from its database — the daemon fails fast if Postgres
is unreachable, and Postgres holds all durable state. So the whole stack is
**one container**: PostgreSQL (`pgvector`) and the daemon, supervised together
by s6-overlay (see [Single-container Nine](single-container.md) for the full
design). There is no docker-compose file; `docker run` is wrapped in a handful
of Makefile targets for the env/volume/flag boilerplate. Inside the container
the daemon reaches Postgres at `localhost:5432` (baked into the image's
`NINE_DATABASE_URL`), and reaches the host's LLM endpoint via
`host.docker.internal`.

There are two modes, built from the same `Dockerfile`, both re-based on
`pgvector/pgvector:pg17` (Debian bookworm) so Postgres, pgvector, and the
official Postgres entrypoint are reused rather than reimplemented.

### Production mode

The built runtime image (immutable; no toolchain or source):

```bash
make up
make session          # docker exec -it nine nine
```

### Hot-reload mode

The Go toolchain runs over a bind-mount of the source tree; the `nine` service's
run script builds `nine` + the Go plugins and **rebuilds and restarts the
daemon on any `.go` change** (via `inotifywait`, see `docker/dev-entrypoint.sh`).
This is the containerized development loop:

```bash
make up-hot
make logs              # watch builds + daemon output
make session           # attaches to nine-dev if nine isn't running
```

The hot-reload container (`nine-dev`) uses its own volumes and a cached Go build
volume; the two modes are separate containers and are not meant to run at once.
LLM knobs (`NINE_LLM_PROVIDER`/`MODEL`/`ENDPOINT`) pass through (empty = use
`nine.toml`). The browser plugin (Chromium + Node) is present in **both**
images — it is Node, so it is baked in as immutable content rather than rebuilt
from the mounted source, but that applies equally to hot-reload and production.

### Optional: pgAdmin

pgAdmin is not part of either image — it's opt-in tooling, spawned on demand as
a separate container:

```bash
make pgadmin           # http://localhost:5050, pre-wired to the nine database
make pgadmin-down
```

It connects over whichever container's published Postgres port; `up-hot`
publishes `5432` by default. Desktop mode (no login) is for local convenience
only — never expose it beyond localhost.

### Volume layout

The `nine` binary (with built-in skills embedded), the compiled plugins, and the
browser plugin code are immutable image content under `/opt/nine` — they are **not**
stored in a volume. Each mode has **two** named volumes for mutable state, kept
separate so the database can be backed up, snapshotted, or reset independently
of the workspace:

- `nine-pgdata` / `nine-dev-pgdata` — the PostgreSQL cluster: conversations,
  tasks, goals, KV, skills, vectors, and the session event journal
- `nine-data` / `nine-dev-data` — the files-plugin workspace at `/data/workspace`

On first boot the `nine` service's run script creates `/data/workspace` and the
daemon seeds the built-in skills (embedded in the binary) into Postgres; this
runs every boot, so editing a skill file and rebuilding updates it, while
agent-authored skills are left untouched. Rebuilding the image picks up code
changes without touching the volumes.

### Stop

```bash
make down
```

This removes the container but **keeps the named volumes**, so the next
`make up` / `make up-hot` comes back up with all state intact.

### Completely remove Nine

To tear everything down — the container, **all data volumes**, and the locally
built images:

```bash
make destroy
```

This is destructive and irreversible: the database and workspace are wiped.

---

## Development (build from source)

For working on Nine itself, build and run the binary natively. Deployment is a
single container (above); this section is about compiling, running, and testing
the code from a checkout. For a containerized development loop instead, use
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
layout — a local Ollama, plugins in `./dist/bin`, and `make pg`'s standalone
Postgres on `localhost:5433`. Running from the project root needs no edits.

To use it from anywhere, copy it to the global location and make the paths absolute:

```bash
mkdir -p ~/.nine
cp nine.toml ~/.nine/nine.toml
```

Nine searches `$NINE_CONFIG`, then `./nine.toml`, then `/nine.toml`, then
`~/.nine/nine.toml`. See [Configuration](configuration.md) for all options.

### 3. Start PostgreSQL

Postgres holds all durable state and the daemon fails fast without it. This is
a standalone database container — unrelated to the containerized Nine above,
which runs its own Postgres internally:

```bash
make pg                       # pgvector on localhost:5433
```

### 4. Pull a model and run

```bash
ollama pull qwen3.5:4b
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
| `make model` | Pull the default Ollama model (`qwen3.5:4b`) |
| `make integration-test` | Run integration tests (requires Docker + Ollama) |
| `make clean` | Remove `dist/` |

**Deploy (single container)**

| Target | Description |
|--------|-------------|
| `make up` | Build + run the production container |
| `make up-hot` | Build + run the hot-reload container (rebuild on `.go` change) |
| `make session` | Open an interactive TUI session in the running container |
| `make shell` | Open a shell in the running container |
| `make logs` | Follow the container's logs (Postgres + daemon) |
| `make down` | Remove the container, keeping all data volumes |
| `make destroy` | Remove the container **and all data volumes and images** |
| `make pgadmin` | Spawn the opt-in pgAdmin side-container |
| `make pgadmin-down` | Remove it |
| `make pg` | Standalone Postgres for the native dev loop / evals (`localhost:5433`) |
| `make pg-down` | Remove it |

---

## Verifying the Installation

**Deployed in a container** — attach a session and check status:

```bash
make session
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
