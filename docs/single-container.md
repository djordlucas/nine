# Single-container Nine — design & implementation plan

> Status: **implemented**. This document plans (and now records) collapsing the
> former multi-container docker-compose stack (Postgres + daemon + pgAdmin) into
> **one image that runs Nine and its PostgreSQL database as a single unit**,
> keeping the two existing modes — *normal* (the immutable runtime) and *dev*
> (hot-reload) — behaviourally identical to before. pgAdmin is no longer part of
> either image; it's opt-in tooling (`make pgadmin`, §8). Both images have been
> built and boot-verified against the §13 checklist below.

## 1. Motivation

Nine cannot be decoupled from its database. `memory.Open` **fails fast** when
Postgres is unreachable — it is not a degraded mode, it is a startup error
(`internal/memory/db.go`). Postgres holds *all* durable state: conversations,
goals, workflows, KV, files, vectors, and the session event journal. The daemon
without its database is not a running Nine; it is a crash loop.

Because the two are one logical unit, packaging them as two containers adds
coordination cost (compose networks, service-name DSNs, health-gated
`depends_on`, two lifecycles to start/stop/upgrade) for a boundary that never
needs to be crossed independently. The goal is **simplicity**: one image, one
volume, one thing to start, stop, back up, and reason about.

This is deliberately the "database-inside-the-app-container" pattern that is
normally discouraged for horizontally-scaled services. For Nine it is the right
call *because* the coupling is intrinsic — see [§12 Tradeoffs](#12-tradeoffs--caveats)
for the costs we are knowingly accepting.

## 2. Where we are today

`docker-compose.yml` defines four services across two profiles:

| Service     | Profile     | Image                     | Role                                   |
|-------------|-------------|---------------------------|----------------------------------------|
| `postgres`  | (always)    | `pgvector/pgvector:pg17`  | Database, exposed on host `5433`       |
| `pgadmin`   | `dev`       | `dpage/pgadmin4:latest`   | DB admin UI on host `5050`             |
| `nine`      | `prod`      | `nine` (Dockerfile `runtime`) | Immutable daemon                  |
| `nine-dev`  | `dev`       | `nine-dev` (Dockerfile `dev`) | Hot-reload daemon over bind-mount |

The daemon reaches Postgres by compose service name (`postgres:5432`) via
`NINE_DATABASE_URL`, and the host LLM via `host.docker.internal`. A session is
attached with `docker exec -it <nine|nine-dev> nine`. Named volumes hold
mutable state only: `nine-pgdata`, `nine-data` / `nine-dev-data`, `nine-pgadmin`.

The `Dockerfile` is already multi-stage with `go-build`, `node-build`, `dev`,
and `runtime` targets — a good foundation to extend rather than rewrite.

### What already makes this easy

- **One config file, env-overridden.** `internal/config.ApplyEnvOverrides` +
  `Config.DatabaseURL()` already let a single `nine.toml` serve every layout;
  the container just overrides `NINE_DATABASE_URL`, `NINE_PLUGINS_BIN`,
  `NINE_WORKSPACE_ROOT`, `NINE_SKILLS_USER_DIR`, `NINE_PLUGINS_USER_DIR`. No
  code change is required for the DSN to point at a co-located Postgres.
- **Schema is self-applying.** `initSchema` runs `CREATE EXTENSION IF NOT EXISTS
  vector` and all `CREATE TABLE IF NOT EXISTS …` on every boot, so any
  pgvector-capable Postgres cluster is bootstrapped by the daemon itself. No
  external migration step.
- **The client is just `nine`.** Attaching a session is `docker exec -it nine
  nine`, talking to `/tmp/nine.sock` inside the container — unchanged.

## 3. Target design at a glance

One image, built to the same two targets we ship today, each now bundling
Postgres:

```
┌─────────────────── container (nine + postgres) ───────────────────┐
│  s6-overlay (pid 1 · supervises · reaps · forwards signals)        │
│                                                                     │
│      ┌──── postgres ────┐        ┌──────── nine ────────┐          │
│      │ initdb once      │        │ gate: wait pg_isready │         │
│      │ listen localhost │───────▶│ then daemon / watcher │         │
│      └──────────────────┘        └───────────────────────┘         │
│                                                                     │
│   pgdata vol → /var/lib/postgresql/data    nine vol → /data         │
└─────────────────────────────────────────────────────────────────────┘
     localhost:5432 (in-container)          LLM → host.docker.internal

   pgAdmin is NOT in the container — it is opt-in tooling (`make pgadmin`)
   that runs `dpage/pgadmin4` as a throwaway side-container when you want it.
```

- **Normal mode** (`runtime` target): two supervised processes — Postgres and
  the immutable `nine daemon`. No toolchain, no source.
- **Dev mode** (`dev` target): Postgres + the hot-reload watcher (rebuilds and
  restarts the daemon on `.go` change). Go toolchain and source bind-mount as
  today. **Same two processes as normal mode** — pgAdmin is not one of them.
- **pgAdmin stays apart.** It is optional DB-admin tooling, spawned on demand as
  a separate container via a Makefile target (§8) — never baked into the Nine
  image. This keeps both image targets lean and the container to two processes.
- **Two named volumes** — one for the Postgres cluster, one for the rest of
  Nine's mutable data (§9). The DB's on-disk state stays independently
  backup-/reset-able from the workspace.

## 4. Process supervision

The container runs exactly **two** long-lived processes (Postgres + the daemon)
in both modes. It still needs a real init to (a) reap zombies, (b) forward
`SIGTERM`/`SIGINT` from `docker stop` to *both* Postgres and the daemon for a
clean shutdown, and (c) order startup so the daemon does not launch before
Postgres accepts connections (remember: fail-fast).

**Recommendation: `s6-overlay`.** It is the de-facto standard for
multi-process images, is tiny, handles PID 1 duties correctly, and supports
per-service `dependencies` and readiness gates. Two small `run` scripts:

- `svc/postgres/run` → execs the official image's `docker-entrypoint.sh
  postgres`, reusing its cluster init, `POSTGRES_USER/PASSWORD/DB` handling, and
  `/docker-entrypoint-initdb.d` hooks. We do **not** reimplement `initdb`.
- `svc/nine/run` → blocks on a `pg_isready -h localhost` loop, then
  execs the daemon (normal) or the hot-reload watcher (dev).

**Alternative considered: `supervisord`.** Simpler mental model, but weaker
dependency ordering and signal semantics; we would hand-roll the readiness gate
anyway. **Plain shell entrypoint** (`postgres & ; wait-for-pg ; exec nine
daemon`) is now more tenable given only two processes, but still reaps zombies
poorly and forwards signals to one child. s6 remains the recommendation for
correct PID 1 behaviour; the shell entrypoint is a viable fallback if we want to
avoid the dependency.

> Whichever supervisor: the daemon service must be **restart-on-exit** so a
> hot-reload rebuild that momentarily drops the daemon (dev) or a transient
> crash (normal) comes back, exactly as `restart: unless-stopped` gives us now.

## 5. Startup ordering & the fail-fast constraint

This is the crux. Today compose enforces order with
`depends_on: postgres: condition: service_healthy`. Inside one container we
reproduce it explicitly:

1. s6 starts `postgres`. On first boot the official entrypoint runs `initdb`
   into the empty PGDATA volume (`/var/lib/postgresql/data`), creates the `nine`
   role/db, and starts listening on `localhost:5432`.
2. `nine`'s `run` script gates on `until pg_isready -q -h localhost -U nine; do
   sleep 0.5; done` before exec'ing the daemon. This turns the previous
   external health-gate into an in-container gate and prevents the fail-fast
   `Ping` from ever hitting a not-yet-ready cluster.
3. Daemon boots, `initSchema` creates the extension + tables (idempotent),
   seeds embedded skills, reconciles standing agents — unchanged behaviour.

No Go code changes are required for this ordering; the gate lives in the service
script. (Optionally we could later add bounded connect-retry to `memory.Open`
for resilience, but it is **not** needed for this plan and is out of scope.)

## 6. Image layout (Dockerfile plan)

Extend the existing multi-stage file. Base the runtime/dev stages on a
Postgres+pgvector image so the database, the `vector` extension, and the
official init script are present.

- **Base choice:** `pgvector/pgvector:pg17` (Debian bookworm). It already ships
  Postgres 17 + the pgvector extension and the `docker-entrypoint.sh` we want to
  reuse. Node.js and Chromium (browser plugin) install cleanly from Debian
  repos. This replaces today's Alpine runtime base — a deliberate trade of image
  size for reusing the official Postgres tooling and avoiding a hand-built
  pgvector.
- **`go-build` / `node-build` stages:** unchanged — compile `nine` + Go plugins,
  install browser-plugin `node_modules`.
- **`runtime` target (normal):**
  - `FROM pgvector/pgvector:pg17`
  - install `s6-overlay`, `nodejs`, `chromium` + fonts
  - `COPY` the `nine` binary, `/opt/nine/bin/*` plugins, `/opt/nine/browser`
    code + `node_modules`, and the browser launcher shim (as today)
  - `COPY` s6 service definitions (`postgres`, `nine`)
  - `ENV NINE_BIN=/opt/nine/bin`, `PLAYWRIGHT_*`, `POSTGRES_USER/PASSWORD/DB=nine`
    (keep the official image's default `PGDATA=/var/lib/postgresql/data`)
  - ⚠️ **Chromium path changes.** Today's
    `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser` is Alpine's
    layout. On Debian the package is `chromium` and the binary is
    **`/usr/bin/chromium`** — update the env var in *both* targets or the browser
    plugin silently fails to launch.
  - `VOLUME /var/lib/postgresql/data`, `VOLUME /data`; `ENTRYPOINT ["/init"]` (s6)
- **`dev` target (hot-reload):**
  - same base + s6, **plus** the Go toolchain and `inotify-tools` — **no
    pgAdmin** (it is a separate side-container, §8)
  - bake the browser plugin under `/opt/nine/browser` as immutable content
    (unchanged rationale — it is Node, not rebuilt from the mount)
  - `COPY` the same two s6 service definitions, but the `nine` service's `run`
    wraps today's `docker/dev-entrypoint.sh` build/watch loop
  - source bind-mounts at `/nine-src` at runtime, as today

The current `entrypoint.sh` (`mkdir -p /data/workspace; exec nine daemon`) folds
into the s6 `nine` service `run` (the `mkdir` becomes a one-shot init service or
a line in the gate script). `docker/dev-entrypoint.sh` is preserved almost
verbatim as the dev `nine` service body.

## 7. Normal mode

```bash
docker run -d --name nine \
  -v nine-pgdata:/var/lib/postgresql/data \
  -v nine-data:/data \
  --add-host host.docker.internal:host-gateway \
  -e NINE_LLM_ENDPOINT=http://host.docker.internal:11434 \
  nine
docker exec -it nine nine        # attach a session
```

- Two processes (Postgres + daemon), two volumes (§9), no exposed ports required
  — a session is a `docker exec`, exactly as `make session` (§11) wraps.
- `NINE_DATABASE_URL` defaults to `postgres://nine:nine@localhost:5432/nine?sslmode=disable`
  (co-located), baked into the image env so the mounted `nine.toml` still needs
  no container-specific edits.
- LLM knobs (`NINE_LLM_PROVIDER/MODEL/ENDPOINT`) pass through unchanged.

## 8. Dev / hot-reload mode

```bash
docker run -d --name nine-dev \
  -v "$PWD":/nine-src \
  -v nine-dev-pgdata:/var/lib/postgresql/data \
  -v nine-dev-data:/data \
  -v nine-dev-gocache:/root/.cache/go-build \
  -p 5432:5432 \
  --add-host host.docker.internal:host-gateway \
  nine-dev
docker logs -f nine-dev
```

- Same two supervised processes as normal mode — Postgres + the `.go`
  watch/rebuild/restart loop — plus the mounted source and Go build cache.
- The hot-reload loop is the existing `dev-entrypoint.sh` logic; the browser
  plugin stays baked image content and is excluded from the watch, unchanged.
- Dev publishes Postgres on host `5432` so pgAdmin (or `psql`) can reach the DB
  from outside the container. Normal mode leaves it unexposed.
- Behavioural parity: same rebuild-on-save, same separate data volumes from
  normal mode, same "not meant to run both at once" (enforced naturally — they
  are different containers/names).

### pgAdmin — opt-in tooling, not in the image

pgAdmin is **not** built into either Nine image. When you want a DB UI, spawn it
on demand as a throwaway side-container that connects to the exposed Postgres
port. A Makefile target wraps it:

```make
# Spawn pgAdmin against the running dev container's Postgres (host :5432).
# Desktop mode: no login, no master password. Tear down with `make pgadmin-down`.
pgadmin:
	docker run -d --name nine-pgadmin \
	  --add-host host.docker.internal:host-gateway \
	  -e PGADMIN_DEFAULT_EMAIL=nine@nine.dev \
	  -e PGADMIN_DEFAULT_PASSWORD=nine \
	  -e PGADMIN_CONFIG_SERVER_MODE=False \
	  -e PGADMIN_CONFIG_MASTER_PASSWORD_REQUIRED=False \
	  -v $(PWD)/docker/pgadmin-servers.json:/pgadmin4/servers.json:ro \
	  -p 5050:80 dpage/pgadmin4:latest
	@echo "pgAdmin on http://localhost:5050 (pre-wired to the nine database)"

pgadmin-down:
	-docker rm -f nine-pgadmin
```

- `docker/pgadmin-servers.json` is updated so `Host` is `host.docker.internal`
  and `Port` is `5432` — it reaches Nine's Postgres through the published port,
  not a compose service name.
- Nothing about pgAdmin touches the Nine image, the supervisor, or either mode's
  process set. It is pure operator tooling, spawned and torn down independently.
- Works against **either** mode as long as that container publishes `5432` (dev
  does by default; add `-p 5432:5432` to the normal-mode `docker run` if you want
  to inspect a production DB).

## 9. Volumes & persistence

**Two named volumes per mode** — the database cluster kept separate from the
rest of Nine's mutable data:

| Volume (prod / dev)                | Mount                          | Contents                                   |
|------------------------------------|--------------------------------|--------------------------------------------|
| `nine-pgdata` / `nine-dev-pgdata`  | `/var/lib/postgresql/data`     | PostgreSQL cluster (`PGDATA`)              |
| `nine-data` / `nine-dev-data`      | `/data`                        | files-plugin workspace (`/data/workspace`), and any other daemon-side state under `/data` |

- Keeping PGDATA on its **own** volume lets the database be backed up, snapshotted,
  reset, or migrated (major-version `pg_upgrade`) independently of the workspace —
  and vice-versa. The container is still one unit; only its on-disk state is split
  along the natural DB-vs-app-data seam.
- PGDATA stays at the official image default (`/var/lib/postgresql/data`), so the
  Postgres entrypoint's init logic works untouched. `initdb` runs only when that
  volume is empty, so re-runs are safe and existing data survives container
  replacement.
- **Backups:** `pg_dump`/`pg_restore` over the DB (via `docker exec` or the
  exposed port) for the database; a `nine-data` volume snapshot for the
  workspace. The two concerns back up on their own cadence.

## 10. Networking, ports, env

- **In-container:** daemon ↔ Postgres over `localhost:5432` (or the Postgres
  unix socket `/var/run/postgresql` for slightly less overhead — TCP localhost
  is simpler and keeps the DSN format; recommend TCP).
- **Exposed ports:** normal mode needs none. Dev publishes `5432` so the opt-in
  pgAdmin side-container (or `psql`) can reach the DB from outside. pgAdmin, when
  spawned, publishes its own `5050` from its separate container (§8).
- **LLM:** unchanged — `host.docker.internal` via `--add-host …:host-gateway`,
  or an external endpoint.
- **Env surface** stays the current `NINE_*` set; only `NINE_DATABASE_URL`'s
  default host changes (`postgres` → `localhost`).

## 11. Run surface: Makefile over `docker run` (no compose)

**`docker-compose.yml` is deleted.** Compose exists to orchestrate *multiple*
containers — networks, service-name DNS, health-gated `depends_on`, profile
fan-out. Once Nine and Postgres are one container, every one of those features
is dead weight: there is nothing to orchestrate. Keeping a one-service compose
file would preserve the ceremony the whole change is meant to remove, and it
would be a second place where volumes, env, and ports are declared.

The run surface is **`docker run`, wrapped in Makefile targets** for the env
passthrough and flag soup. `docker` is the only dependency; `docker compose` is
no longer required at all.

| Target             | Does                                                       |
|--------------------|------------------------------------------------------------|
| `make up`          | build + run normal mode (`nine`), detached                 |
| `make up-hot`      | build + run hot-reload mode (`nine-dev`), source mounted   |
| `make session`     | `docker exec -it <nine\|nine-dev> nine` — attach a TUI      |
| `make shell`       | `docker exec -it <nine\|nine-dev> sh`                       |
| `make logs`        | `docker logs -f <nine\|nine-dev>`                           |
| `make down`        | `docker rm -f` the container — **volumes kept**            |
| `make destroy`     | `down` + `docker volume rm` both volumes + `docker image rm` |
| `make pgadmin`     | spawn the pgAdmin side-container (§8)                      |
| `make pgadmin-down`| remove it                                                   |

Notes for the implementer:

- **Name collision:** the Makefile already has a `dev` target (`build plugins
  browser-plugin` — the *native* build). The container hot-reload mode must
  **not** be called `dev`. Hence `up-hot` above; `run-dev` or `hot` work too, but
  `dev` is taken.
- `session` / `shell` / `logs` keep today's "prod first, fall back to dev"
  pattern: `docker exec -it nine … 2>/dev/null || docker exec -it nine-dev …`.
- The existing `LLM_ENV` passthrough (`NINE_LLM_PROVIDER/MODEL/ENDPOINT`) carries
  over verbatim — it just becomes `-e` flags on `docker run` instead of compose
  `environment:` interpolation.
- `make down` removing the container (rather than stopping it) matches the old
  `compose down` semantics: containers gone, named volumes intact, next `make up`
  comes back with all state.
- Everything the compose file used to declare — the two volumes, `--add-host
  host.docker.internal:host-gateway`, `NINE_CONFIG=/nine.toml`,
  `NINE_LOG_FILE=off`, the `nine.toml`/`skills.d`/`plugins.d` read-only mounts,
  `--restart unless-stopped` — moves onto the `docker run` line in the target.
  §7/§8 show the shape; the full flag set is mechanical.
- **Restart policy:** pass `--restart unless-stopped` on `docker run` to preserve
  today's behaviour.

### Don't strand the native dev loop

Compose is not only the deployment surface today — it is also how a **native**
checkout gets a database. Deleting the file without a replacement breaks three
things that have nothing to do with containerized Nine:

1. **`nine.toml`'s default DSN is `localhost:5433`** — that port exists only
   because the compose `postgres` service maps `5433:5432`. A developer running
   `make build && ./dist/nine daemon` on the host needs something listening there.
2. **`make eval-live`** documents *"Needs Postgres up (`docker compose up -d
   postgres`)"*.
3. **`docs/configuration.md`** tells readers to start a local instance with
   `docker compose up -d`.

Replace all three with a standalone target — a bare pgvector container on 5433,
no compose:

```make
# Postgres for a native `./dist/nine daemon`, evals, and integration tests.
# Matches nine.toml's default DSN (localhost:5433). Unrelated to the Nine
# container, which runs its own Postgres internally.
pg:
	docker run -d --name nine-pg \
	  -e POSTGRES_USER=nine -e POSTGRES_PASSWORD=nine -e POSTGRES_DB=nine \
	  -p 5433:5432 -v nine-pg-native:/var/lib/postgresql/data \
	  pgvector/pgvector:pg17

pg-down:
	-docker rm -f nine-pg
```

Keep the port at **5433** so `nine.toml` and every existing doc/DSN stay correct,
and so it never collides with the containerized Nine publishing 5432 (§8). Update
the `eval-live` comment and `docs/configuration.md` to point at `make pg`.

## 12. Tradeoffs & caveats

Accepting DB-in-app-container knowingly:

- **Coupled lifecycle.** Restarting the container cycles Postgres too. This is
  the intended semantics (one unit), but it means a daemon-only restart is now a
  `docker exec`-level concern (send the daemon a signal / restart its s6
  service), not a container restart.
- **Postgres major-version upgrades** require the usual `pg_upgrade` /
  dump-restore dance against the volume — the image can't just bump `pg17`→`pg18`
  and reuse an old `PGDATA`. Document a dump/restore path.
- **No independent scaling.** Acceptable — Nine is a single-instance daemon.
- **Larger image.** Postgres + pgvector + Node + Chromium (+ Go in dev) on a
  Debian base is heavier than today's Alpine runtime. Mitigate with multi-stage
  copies and by keeping the toolchain in the `dev` target only. (pgAdmin adds
  nothing — it is a separate on-demand container.)
- **Resource limits** now cover two engines; document sane `--memory` guidance
  (Postgres `shared_buffers` + the model client + Chromium).
- **Log multiplexing.** Postgres and daemon logs share the container's stdout.
  Keep `NINE_LOG_FILE=off` (stderr) as today and prefix/space them via s6 so
  `docker logs` stays readable.

## 13. Behavioural-parity checklist

Everything below must work identically to the current stack. Checked items were
verified directly (build + boot both images, `docker exec ... nine status`,
touch-a-`.go`-file, timed `docker stop`); unchecked items still need a real run
(no live Ollama/agent config in the verification environment):

- [x] `docker exec -it nine nine` opens a session against `/tmp/nine.sock` —
      verified via `nine status` over the same socket path; not re-tested with
      an actual interactive pty.
- [x] `initSchema` bootstraps a fresh PGDATA volume (extension + tables) on
      first boot; second boot is a no-op; existing data survives — confirmed
      `initdb` runs once, "Skipping initialization" on restart, prior session
      resumed.
- [x] Built-in skills seed every boot; agent-authored skills untouched — 17
      built-ins seeded identically across restarts, both images.
- [ ] `skills.d` / `plugins.d` user dirs discovered from their mounts — not
      exercised in verification (no user skills/plugins configured).
- [ ] Standing agents / scheduling reconcile on boot — not exercised (no
      `[[agent]]` configured in the test run).
- [x] Browser plugin runs (Chromium + Node) in **both** modes, with the Debian
      binary path (`/usr/bin/chromium`, §6) — confirmed: `plugin started
      name=browser tools=9` in both the runtime and dev images.

      > **Pre-existing doc bug, reconciled:** `docs/installation.md` claimed the
      > browser plugin was "present only in the production image, not the
      > hot-reload container". That was false — the `dev` stage installs
      > `chromium` and bakes `/opt/nine/browser` + `node_modules` + the launcher
      > exactly as `runtime` does. Fixed in the docs sweep (§14 step 8).
- [x] Dev mode rebuilds + restarts the daemon on `.go` save; DB survives the
      restart — confirmed: touching a `.go` file triggered `[dev] change
      detected — rebuilding` → new daemon PID → session resumed.
- [ ] `make pgadmin` spawns a side-container that reaches the DB via the
      published `5432` and the updated `servers.json`; `make pgadmin-down`
      removes it — not exercised in verification.
- [x] `docker stop` cleanly terminates both Postgres and the daemon (s6 signal
      forwarding) — confirmed on both images; a real bug was found and fixed
      here: `docker/dev-entrypoint.sh`'s watch loop ran `inotifywait` as the
      loop's direct foreground command, and dash (Debian's `/bin/sh`) defers a
      trapped `TERM` until a foreground child exits — so `docker stop` hung
      idle for the full grace period, then force-killed the container,
      skipping Postgres's own shutdown entirely. Fixed by backgrounding
      `inotifywait` and joining it with `wait`, which *is* interrupted
      immediately by a trapped signal; clean shutdown now completes in ~3s
      instead of hitting the 10s force-kill.
- [ ] LLM reachable via `host.docker.internal` — the connection attempt fires
      correctly (logged `dial tcp ... connect: connection refused`, as
      expected with no Ollama listening in the verification environment); not
      confirmed against a real running model.
- [ ] Integration tests (`make integration-test`) pass against the single
      image — not run (needs Docker + a live Ollama with the configured model).

## 14. Implementation steps

1. **Add s6-overlay** to the Dockerfile base and author the `postgres` / `nine`
   service scripts — one pair, shared by both targets (the dev target's `nine`
   `run` is the watch loop).
2. **Re-base** `runtime` and `dev` targets on `pgvector/pgvector:pg17`; port the
   existing binary/plugin/browser `COPY`s and env; keep the default
   `PGDATA=/var/lib/postgresql/data`.
3. **Fold** `entrypoint.sh` (workspace mkdir) and `docker/dev-entrypoint.sh`
   (watch loop) into the s6 `nine` service scripts.
4. **Add the pg-ready gate** in the `nine` service script; default
   `NINE_DATABASE_URL` to `localhost:5432` in image env.
5. **Update** `docker/pgadmin-servers.json` (Host `host.docker.internal`, Port
   `5432`) and add `make pgadmin` / `pgadmin-down` targets — pgAdmin is a
   side-container, not part of any image.
6. **Delete `docker-compose.yml`** and replace the `make compose-*` targets with
   the `docker run`-based set in §11 (`up` / `up-hot` / `session` / `shell` /
   `logs` / `down` / `destroy`), carrying over the `LLM_ENV` passthrough, the two
   volumes per mode, the read-only config/skills/plugins mounts, `--add-host`,
   and `--restart unless-stopped`. Hot-reload mode publishes `5432`.
7. **Add `make pg` / `pg-down`** (§11) so the native dev loop, `make eval-live`,
   and the integration tests keep a Postgres on `localhost:5433` after compose is
   gone; update the `eval-live` comment and `docs/configuration.md` accordingly.
8. **Docs sync (`/sync-nine`)**: rewrite `docs/installation.md`'s Docker
   section (including the "Docker 24+ — docker compose runs the whole stack"
   prerequisite), update `docs/README.md` quick-start and the top-level
   `README.md` to describe the single-container model, and sweep the stale
   compose references in `docs/configuration.md`,
   `docs/architecture_detailed.md`, and `docs/event-log.md`. This doc is the
   design of record; installation becomes the how-to.
9. **Verify** the §13 checklist, including `make integration-test`, on both
   targets before merging.

## 15. Decisions

Settled — implement to these, don't re-litigate:

| Question | Decision | Rationale |
|----------|----------|-----------|
| Init / PID 1 | **s6-overlay** | Correct reaping, per-service restart-on-exit, clean signal fan-out to both processes (§4). |
| Base image | **`pgvector/pgvector:pg17`** (Debian bookworm) | PG 17 + pgvector prebuilt, and the official `docker-entrypoint.sh` is reused rather than reimplemented (§6). |
| Native dev loop | **Keep it — add `make pg`** | Host `./dist/nine daemon`, `make eval-live`, and integration tests keep a Postgres on `localhost:5433`; `nine.toml`'s default DSN stays correct (§11). |
| Makefile targets | **`up` / `up-hot` / `session` / `shell` / `logs` / `down` / `destroy`** | Docker-idiomatic; `up-hot` avoids the existing `make dev` collision (§11). |

**Ubuntu was considered and rejected** for the base image. It offers no
meaningful advantage over Debian bookworm (which is what the pgvector image
already is — same apt/glibc experience, comparable size), while costing two
things: vendoring/reimplementing the official Postgres entrypoint, and working
around Ubuntu 22.04+ shipping `chromium-browser` as a **snap transitional
package** that cannot run in a container (Playwright's docs steer to Debian for
this reason). Revisit only if a base-image policy demands it.

### Still open (low stakes, decide during implementation)

- **Postgres over TCP `localhost:5432` vs. unix socket** — recommend TCP for DSN
  simplicity; unix socket is a later micro-optimisation.
- **Publish Postgres in normal mode?** Hot-reload publishes `5432` for
  pgAdmin/`psql`. Leaving it unexposed in normal mode is the safer default;
  document adding `-p 5432:5432` when an operator wants to point pgAdmin at a
  production DB.
- **pgAdmin auth defaults.** The `make pgadmin` target uses desktop mode (no
  login) for convenience, matching today's dev pgAdmin. Fine for local use;
  call out that it should never be exposed beyond localhost.
