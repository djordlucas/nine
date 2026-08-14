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

Nine's durable state is a SQLite file, so the deployment unit is **one
container** running the daemon alone under s6-overlay (see
[Single-container Nine](single-container.md) for the full design). There is no
docker-compose file and no database service to orchestrate; `docker run` is
wrapped in a handful of Makefile targets for the env/volume/flag boilerplate.
Inside the container the daemon opens `/data/nine.db` and reaches the host's LLM
endpoint via `host.docker.internal`.

There are two modes, built from the same `Dockerfile`, both based on
`debian:bookworm-slim` — enough for chromium and Node, which the browser plugin
needs. The `nine` binary is pure Go and carries no libc dependency of its own.

That extends to the sandboxed-tool host. Its wasm runtime (wazero) is pure Go,
and the QuickJS interpreter it runs `js` tools on is **built ahead of time from
pinned tags and committed** as `internal/toolvm/quickjs/qjs.wasm`
(`docs/sandboxed-tools.md` §10.1). So an ordinary `make build` needs **no
wasi-sdk, no clang, and no clone**, and the runtime image gains no toolchain —
which is the property `docs/self-modification.md` insists on and the reason the
blob is committed rather than built on demand. `make quickjs-wasm` rebuilds it
and is invoked only on a deliberate version bump; `make quickjs-verify` re-checks
the recorded hash and runs in CI.

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

### Inspecting the database

The database is a plain SQLite file, so any SQLite client reads it. Under WAL a
reader never blocks the daemon:

```bash
docker exec -it nine sh -c 'sqlite3 /data/nine.db ".tables"'
```

### Volume layout

The `nine` binary (with built-in skills embedded), the compiled plugins, and the
browser plugin code are immutable image content under `/opt/nine` — they are **not**
stored in a volume. Each mode has **one** named volume for mutable state:

- `nine-data` / `nine-dev-data` — mounted at `/data`, holding both `nine.db`
  (conversations, goals, KV, skills, vectors, and the session event journal) and
  the files-plugin workspace at `/data/workspace`

> Backing up the database means copying `nine.db` **and** its `-wal` and `-shm`
> sidecars together, or running `VACUUM INTO` to produce a single consistent file.

On first boot the `nine` service's run script creates `/data/workspace` and the
daemon seeds the built-in skills (embedded in the binary) into the database; this
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

# Build the nine binary and the browser plugin
make all
```

The build produces:
- `dist/nine` — the main CLI/daemon binary, which also *is* the `shell`, `files`,
  `http`, and `time` plugins: the daemon starts each as a `nine plugin serve <name>`
  child process, so they need no build step and no binary of their own
- `dist/bin/browser` — browser plugin launcher (requires Node.js + npm)

### 2. Config

The repo's `nine.toml` is the only config file, and it is written for exactly this
layout — a local Ollama and plugins in `./dist/bin`. Running from the project root
needs no edits; the database is created on first run at `~/.nine/nine.db`.

To use it from anywhere, copy it to the global location and make the paths absolute:

```bash
mkdir -p ~/.nine
cp nine.toml ~/.nine/nine.toml
```

Nine searches `$NINE_CONFIG`, then `./nine.toml`, then `/nine.toml`, then
`~/.nine/nine.toml`. See [Configuration](configuration.md) for all options.

### 3. Pull a model and run

```bash
ollama pull qwen3.5:4b
./dist/nine "Hello"
```

The daemon starts automatically and stays running in the background.

Ollama is the only chat backend — Nine runs on local models. Pick one that can
actually drive the agent loop: [Model compatibility](model-compatibility.md)
records which models have been run and how they did.

---

## Makefile Targets

**Build & test**

| Target | Description |
|--------|-------------|
| `make build` | Compile `dist/nine` (which serves the `shell`/`files`/`http`/`time` plugins too) |
| `make browser-plugin` | Build the browser plugin (requires npm) |
| `make all` | All of the above |
| `make test` | Run all tests. The two live-model tests skip unless `NINE_LIVE_MODEL` names an Ollama tag (e.g. `NINE_LIVE_MODEL=qwen3.5:4b make test`) |
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
| `make logs` | Follow the container's logs |
| `make down` | Remove the container, keeping all data volumes |
| `make destroy` | Remove the container **and all data volumes and images** |

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
