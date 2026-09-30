# Installation

Three ways to run Nine, in increasing order of effort:

| Path | Needs | For |
|------|-------|-----|
| [Pull the image](docker-image.md) | Docker, an LLM endpoint | Running Nine |
| Build the image from a clone | Docker, the repo | Changing the image, or auditing the build |
| Native build | Go 1.26+, the repo | Developing Nine |

The published image is the shortest path and needs no clone. The package is public, so the
pull needs no credentials:

```bash
docker run -d --name nine \
  -p 127.0.0.1:8080:8080 \
  --add-host host.docker.internal:host-gateway \
  -v nine-data:/data \
  ghcr.io/djordlucas/nine:latest
```

Registries, tags, signature verification and the image's configuration surface
are in [Container image](docker-image.md). The rest of this document covers
building it yourself.

## Prerequisites

| Requirement | Version | Notes |
|-------------|---------|-------|
| Docker | 24+ | Required — runs the whole stack in one container. Only this is needed to run the published image. |
| Go | 1.26+ | For development (build from source) |
| Node.js | 18+ | Optional — only to run `npx`-launched MCP servers (e.g. a browser) |
| golangci-lint | Latest | Optional — only for `make lint-host`; `make lint` runs it pinned, in a container |

An LLM provider is also required — see [Configuration](configuration.md) for options.

---

## Deployment (single container)

Nine's durable state is a SQLite file, so the deployment unit is **one
container** running the daemon alone under s6-overlay (see
[Single-container Nine](../adr/single-container.md) for the full design). There is no
docker-compose file and no database service to orchestrate; `docker run` is
wrapped in a handful of Makefile targets for the env/volume/flag boilerplate.
Inside the container the daemon opens `/data/nine.db` and reaches the host's LLM
endpoint via `host.docker.internal`.

These targets build the image locally. The same `runtime` stage is what gets
published — `make image` builds it exactly as a release does, and `make
image-test` asserts the same contract CI does before a push.

There are two modes, built from the same `Dockerfile`, both based on
`debian:bookworm-slim`. The `nine` binary is pure Go and carries no libc
dependency of its own. Neither image ships a browser; the dev image carries
`nodejs`/`npm` so an `npx`-launched MCP server can be tried out, and the runtime
image carries neither (see [Browser Automation](browser.md#6-docker)).

That extends to the sandboxed-tool host. Its wasm runtime (wazero) is pure Go,
and the QuickJS interpreter it runs `js` tools on is **built ahead of time from
pinned tags and committed** as `internal/toolvm/quickjs/qjs.wasm`
(`sandboxed-tools.md` §10.1). So an ordinary `make build` needs **no
wasi-sdk, no clang, and no clone**, and the runtime image gains no toolchain —
which is the property `self-modification.md` insists on and the reason the
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
`nine.toml`).

### Inspecting the database

The database is a plain SQLite file, so any SQLite client reads it. Under WAL a
reader never blocks the daemon:

```bash
docker exec -it nine sh -c 'sqlite3 /data/nine.db ".tables"'
```

### Volume layout

The `nine` binary (with built-in skills embedded, and serving the built-in
plugins) is immutable image content — it is **not** stored in a volume. Each mode has **one** named volume for mutable state:

- `nine-data` / `nine-dev-data` — mounted at `/data`, holding both `nine.db`
  (conversations, goals, KV, skills, vectors, and the session event journal) and
  the sandboxed tools' workspace at `/data/workspace`

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

# Build the nine binary
make all
```

The build produces **one file**:

- `dist/nine` — the CLI, the TUI, the daemon, and the `shell` plugin. The daemon
  starts a plugin as a `nine plugin serve <name>` child process, so it keeps its
  own process, socket, and crash isolation while needing no build step and no
  binary of its own. Files, HTTP and the clock are sandboxed tools rather than
  plugins, and need no subprocess at all.

There is no second artifact. `dist/bin/` stays empty unless one of your own plugins
puts a binary there, and a capability Nine does not implement itself arrives as an
`[[mcp.server]]` rather than something to build (see [Browser Automation](browser.md)).

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

Ollama is the default backend and the one Nine is developed against; `mistral`
is the other, for an OpenAI-compatible endpoint
([configuration.md](configuration.md#mistral)). On a local model, pick one that
can actually drive the agent loop: [Model compatibility](model-compatibility.md)
records which models have been run and how they did.

---

## Makefile targets

**Build & test**

| Target | Description |
|--------|-------------|
| `make build` | Compile `dist/nine` (which serves the `shell` plugin too) |
| `make all` | Same as `make build` |
| `make test` | Run all tests. The two live-model tests skip unless `NINE_LIVE_MODEL` names an Ollama tag (e.g. `NINE_LIVE_MODEL=qwen3.5:4b make test`) |
| `make test-v` | Run tests with verbose output |
| `make cover` | Generate `dist/coverage.out` |
| `make cover-html` | Open HTML coverage report in browser |
| `make lint` | Run golangci-lint from a pinned container image (reproducible; needs Docker) |
| `make lint-host` | Run the golangci-lint installed on `PATH` — quicker, but version-dependent |
| `make model` | Pull the default Ollama model (`qwen3.5:4b`) |
| `make integration-test` | Run integration tests (requires Docker + Ollama) |
| `make clean` | Remove `dist/` |

**The gate**

GitHub Actions is disabled for this repository, so nothing runs these on a
push. `make ci` is what a merge should pass.

| Target | Description |
|--------|-------------|
| `make ci` | Build, OpenAPI drift + lint, tests, eval gate, lint, and the Docker image contract tests. Needs a running Docker daemon; nothing else to install. |
| `make ci-test` | The test step alone. Excludes `tests/evals/runner`, which starts a real in-process daemon per test and deadlocks intermittently; `make test` still runs everything. |
| `make scan` | Trivy over the filesystem and the runtime image — CRITICAL and HIGH, fixable only. |
| `make scan-fs` | The filesystem half of `make scan`. |

The scan targets run Trivy as a container and `make lint` runs golangci-lint
as one, both at pinned versions, so there is nothing to install beyond Docker
and no machine-to-machine drift in what the gate reports.

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

The rest of the Makefile is documented where its subject is: `image`,
`image-test`, `image-scan` and `image-verify` in
[Container image](docker-image.md), `eval-replay` / `eval-live` /
`eval-generate` in [Evals](evals.md), `openapi` and its lint/check pair in
[HTTP API](api.md), and `quickjs-wasm` / `harness-bc` in
[Sandboxed tools](sandboxed-tools.md#101-building-the-quickjs-blob).

---

## Verifying the installation

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
Uptime:   2m34s
LLM queue: 0 inflight, 0 waiting  (max 1 concurrent)
Agents:   1 active
Plugins:  mcp, shell
```

## Limits

| Limit | Detail |
|-------|--------|
| One container, one host | The deployment unit is a single container holding the daemon and its SQLite file. There is no multi-node story and no second service to orchestrate. |
| Config changes need a restart | Nine cannot rewrite `nine.toml` at runtime. Edit the file and restart the daemon. |
| Environment overrides are a fixed set | Only the documented `NINE_*` variables override the file. Whether the sandboxed-tool subsystem runs at all stays in `nine.toml`, which no environment variable can flip on. |
| No browser in either image | The dev image carries Node for `npx` MCP servers; the runtime image carries neither Node nor a browser. |
| Published image runs unprivileged | The daemon and API run as uid 1000, so the agent cannot install packages or write outside `/data` inside the container. Derive an image to add anything. |
| `docker exec` lands as root | Pass `-u nine` for anything touching `/data`, or a root-owned file appears in the volume. |
| Rebuilding the QuickJS blob needs wasi-sdk | Ordinary builds use the committed `qjs.wasm` artifact and its recorded SHA-256. Only `make quickjs-wasm` wants a wasi-sdk. |
