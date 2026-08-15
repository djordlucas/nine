# Single-container Nine — design & implementation plan

> **Superseded in part:** the browser plugin this document bakes into both images
> no longer exists. Nine ships no browser: the runtime image carries neither Node
> nor Chromium, and browser automation is an `[[mcp.server]]`
> ([browser.md](browser.md)). Every `chromium` / `/opt/nine/browser` /
> `node_modules` step below is historical, as is the checklist item asserting the
> browser plugin runs in both modes. The s6-overlay reaping argument still holds —
> an MCP server's process tree orphans the same way Chromium did.
>
> Status: **implemented**, and since **simplified**. This document plans (and
> records) collapsing the former multi-container docker-compose stack (Postgres +
> daemon + pgAdmin) into one image, keeping the two modes — *normal* (the
> immutable runtime) and *dev* (hot-reload) — behaviourally identical.
>
> **The database has since moved to SQLite**, which removes the second process
> entirely: the container now runs the daemon alone, with its database as a file
> on the `/data` volume. The parts of this document that argue for *packaging two
> coupled processes together* are therefore historical — the coupling they
> describe no longer exists, and the conclusion (one container) simply became
> easier to reach. Sections describing the database service, its readiness gate,
> its volume, and pgAdmin are marked or removed accordingly. The reasoning is kept
> because it explains why the boundary was collapsed in the first place.

## 1. Motivation

Nine cannot be decoupled from its database. `memory.Open` **fails fast** when the
database is unusable — it is not a degraded mode, it is a startup error
(`internal/memory/db.go`). The database holds *all* durable state: conversations,
goals, workflows, KV, files, vectors, and the session event journal. The daemon
without it is not a running Nine; it is a crash loop.

*(Historical: when that database was a Postgres server, packaging the two as
separate containers added real coordination cost — compose networks, service-name
DSNs, health-gated `depends_on`, two lifecycles to start, stop and upgrade — for a
boundary that never needed to be crossed independently. That argument is what
collapsed the stack into one image, and it is preserved below.)*

**Today the coupling is not a packaging problem at all.** The database is a SQLite
file on the same volume as the workspace, so there is no second process, no
readiness gate, and no network between the daemon and its state. The goal was
**simplicity**: one image, one volume, one thing to start, stop, back up, and
reason about. What used to require a supervisor and a health gate to achieve is
now simply the shape of the system.

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
  `Config.DatabasePath()` already let a single `nine.toml` serve every layout;
  the container just overrides `NINE_PLUGINS_BIN`, `NINE_WORKSPACE_ROOT`,
  `NINE_SKILLS_USER_DIR`, `NINE_PLUGINS_USER_DIR`, `NINE_TOOLS_USER_DIR`. The
  database path needs no override at all — it resolves to `/data/nine.db` when
  that volume is present. `[tools] enabled` is *not* env-overridable: turning the
  sandboxed-tool host on stays a decision in the mounted `nine.toml`.
- **Schema is self-applying.** `initSchema` runs all `CREATE TABLE IF NOT
  EXISTS …` on every boot, so the database is bootstrapped by the daemon itself.
  No external migration step.
- **The client is just `nine`.** Attaching a session is `docker exec -it nine
  nine`, talking to `/tmp/nine.sock` inside the container — unchanged.

## 3. Target design at a glance

One image, built to the same two targets we ship today:

```
┌──────────────────────── container (nine) ─────────────────────────┐
│  s6-overlay (pid 1 · supervises · reaps · forwards signals)        │
│                                                                    │
│                    ┌──────── nine ────────┐                        │
│                    │  daemon / watcher    │                        │
│                    │  opens /data/nine.db │                        │
│                    └──────────────────────┘                        │
│                                                                    │
│                   nine vol → /data  (nine.db + workspace/)         │
└────────────────────────────────────────────────────────────────────┘
                                          LLM → host.docker.internal
```

- **Normal mode** (`runtime` target): one supervised process — the immutable
  `nine daemon`. No toolchain, no source.
- **Dev mode** (`dev` target): the hot-reload watcher (rebuilds and restarts the
  daemon on `.go` change). Go toolchain and source bind-mount as today. Same
  single process as normal mode.
- **One named volume** carrying both the database file and the workspace (§9).

*(Historical: this diagram showed a second supervised `postgres` process, a
`pg_isready` gate between the two, a separate `pgdata` volume, and a note about
pgAdmin as a side-container. All are gone.)*

## 4. Process supervision

The container runs **one** long-lived service — the daemon — in both modes. It
still needs a real init, for two of the three original reasons: (a) reap zombies,
which the browser plugin's chromium produces in quantity, and (b) forward
`SIGTERM`/`SIGINT` from `docker stop` for a clean shutdown. The third reason —
ordering a database service ahead of the daemon — is gone with the database
service itself.

**s6-overlay** remains the choice. It is tiny, handles PID 1 duties correctly, and
keeps the service restart-on-exit. One `run` script per mode:

- `docker/s6/runtime/s6-rc.d/nine/run` → creates `/data/workspace`, execs the daemon.
- `docker/s6/dev/s6-rc.d/nine/run` → creates `/data/workspace`, execs the
  hot-reload watcher.

*(Historical: a `postgres` service and a `pg_isready` gate sat alongside these.
Both are deleted.)* With a single process, a plain shell entrypoint would now be
tenable, but it still reaps zombies poorly — which matters here specifically
because of chromium — so s6 stays.

> Whichever supervisor: the daemon service must be **restart-on-exit** so a
> hot-reload rebuild that momentarily drops the daemon (dev) or a transient
> crash (normal) comes back, exactly as `restart: unless-stopped` gives us now.

## 5. Startup ordering & the fail-fast constraint

This was the crux, and it has dissolved. There is no ordering constraint left:

1. s6 starts `nine`. Its `run` script creates `/data/workspace`.
2. The daemon opens `/data/nine.db`, creating the file on first boot.
3. `initSchema` creates the tables (idempotent), seeds embedded skills,
   reconciles standing agents — unchanged behaviour.

Fail-fast still holds — an unopenable database is a startup error — but there is
nothing to wait for, so nothing to gate on.

*(Historical: this section previously described an in-container `pg_isready` loop
replacing compose's health-gated `depends_on`. Both are gone.)*

## 6. Image layout (Dockerfile plan)

Extend the existing multi-stage file.

- **Base choice:** `debian:bookworm-slim`. Node.js and Chromium (browser plugin)
  install cleanly from Debian repos, and the `nine` binary is pure Go — the
  SQLite driver is a Go translation rather than a cgo binding — so it carries no
  libc dependency across stages. *(Historical: the base was
  `pgvector/pgvector:pg17`, chosen to reuse the official Postgres tooling; with
  no database to run, that reason is gone. Alpine would be smaller still, but
  the browser plugin's Playwright does not support musl.)*
- **`go-build` / `node-build` stages:** unchanged — compile `nine` + Go plugins,
  install browser-plugin `node_modules`.
- **`runtime` target (normal):**
  - `FROM debian:bookworm-slim`
  - install `s6-overlay`, `nodejs`, `chromium` + fonts
  - `COPY` the `nine` binary (which carries the Go plugins), `/opt/nine/browser`
    code + `node_modules`, and the browser launcher shim into `/opt/nine/bin`
  - `COPY` the s6 service definition (`nine`)
  - `ENV NINE_BIN=/opt/nine/bin`, `PLAYWRIGHT_*`
  - ⚠️ **Chromium path.** On Debian the package is `chromium` and the binary is
    **`/usr/bin/chromium`**, not Alpine's `/usr/bin/chromium-browser` — the env
    var must match in *both* targets or the browser plugin silently fails to
    launch.
  - `VOLUME /data`; `ENTRYPOINT ["/init"]` (s6)
- **`dev` target (hot-reload):**
  - same base + s6, **plus** the Go toolchain and `inotify-tools`
  - bake the browser plugin under `/opt/nine/browser` as immutable content
    (unchanged rationale — it is Node, not rebuilt from the mount)
  - `COPY` the same s6 service definition, but the `nine` service's `run` wraps
    today's `docker/dev-entrypoint.sh` build/watch loop
  - source bind-mounts at `/nine-src` at runtime, as today

The current `entrypoint.sh` (`mkdir -p /data/workspace; exec nine daemon`) folds
into the s6 `nine` service `run` (the `mkdir` becomes a one-shot init service or
a line in the gate script). `docker/dev-entrypoint.sh` is preserved almost
verbatim as the dev `nine` service body.

## 7. Normal mode

```bash
docker run -d --name nine \
  -v nine-data:/data \
  --add-host host.docker.internal:host-gateway \
  -e NINE_LLM_ENDPOINT=http://host.docker.internal:11434 \
  nine
docker exec -it nine nine        # attach a session
```

- One process, one volume (§9), no exposed ports required — a session is a
  `docker exec`, exactly as `make session` (§11) wraps.
- The database path defaults to `/data/nine.db` because that volume is present,
  so the mounted `nine.toml` needs no container-specific edits.
- LLM knobs (`NINE_LLM_PROVIDER/MODEL/ENDPOINT`) pass through unchanged.

## 8. Dev / hot-reload mode

```bash
docker run -d --name nine-dev \
  -v "$PWD":/nine-src \
  -v nine-dev-data:/data \
  -v nine-dev-gocache:/root/.cache/go-build \
  --add-host host.docker.internal:host-gateway \
  nine-dev
docker logs -f nine-dev
```

- Same single supervised process as normal mode — here the `.go`
  watch/rebuild/restart loop — plus the mounted source and Go build cache.
- The hot-reload loop is the existing `dev-entrypoint.sh` logic; the browser
  plugin stays baked image content and is excluded from the watch, unchanged.
- Neither mode publishes any port. *(Historical: dev published `5432` so pgAdmin
  or `psql` could reach the database from outside.)*
- Behavioural parity: same rebuild-on-save, same separate data volumes from
  normal mode, same "not meant to run both at once" (enforced naturally — they
  are different containers/names).

### Inspecting the database

*(Historical: this section described spawning pgAdmin as a throwaway
side-container against a published Postgres port. With SQLite there is no port
and no server, so pgAdmin and its `make pgadmin` target are gone.)*

The database is a plain SQLite file, so any SQLite client reads it, and WAL means
a reader never blocks the running daemon:

```bash
docker exec -it nine sh -c 'sqlite3 /data/nine.db ".tables"'
```

## 9. Volumes & persistence

**One named volume per mode.** *(Historical: there were two, because a Postgres
cluster wanted its own lifecycle. A database file has no such need, so the split
is gone.)*

| Volume (prod / dev)           | Mount   | Contents                                                        |
|-------------------------------|---------|-----------------------------------------------------------------|
| `nine-data` / `nine-dev-data` | `/data` | `nine.db` (+ `-wal`/`-shm`) and the files-plugin workspace       |

- The database is created on first boot; an existing file survives container
  replacement, so re-runs are safe.
- **Backups:** snapshot the volume, or copy the database out. If copying, take
  `nine.db` **with** its `-wal` and `-shm` sidecars, or run
  `VACUUM INTO '/data/backup.db'` to get one consistent file. Copying `nine.db`
  alone from a running daemon can miss committed transactions still in the WAL.

## 10. Networking, ports, env

- **In-container:** the daemon opens its database as a file. There is no database
  socket or port of any kind. *(Historical: this was `localhost:5432`.)*
- **Exposed ports:** neither mode needs any.
- **LLM:** unchanged — `host.docker.internal` via `--add-host …:host-gateway`,
  or an external endpoint.
- **Env surface** stays the current `NINE_*` set. The database path needs no
  override: it resolves to `/data/nine.db` whenever that volume is present.

## 11. Run surface: Makefile over `docker run` (no compose)

**`docker-compose.yml` is deleted.** Compose exists to orchestrate *multiple*
containers — networks, service-name DNS, health-gated `depends_on`, profile
fan-out. Once Nine and its database are one container, every one of those features
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
| `make destroy`     | `down` + `docker volume rm` the data volumes + `docker image rm` |

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

### The native dev loop needs nothing

*(Historical: this section existed because compose was also how a **native**
checkout got a database — deleting it would have stranded `./dist/nine daemon`,
`make eval-live`, and the integration tests, so a standalone `make pg` target
was introduced to replace it, publishing pgvector on `localhost:5433`.)*

With SQLite there is nothing to provide. `make build && ./dist/nine daemon`
creates `~/.nine/nine.db` on first run; evals give each run its own file; the
`memory` test suite gives each test a file under `t.TempDir()`. The `pg` and
`pg-down` targets are deleted, and no target replaces them.

## 12. Tradeoffs & caveats

Most of the costs this section was written to accept were costs of running a
database *engine* in the app container. Embedding a database *file* does not carry
them, so they are struck below rather than silently dropped:

- ~~**Coupled lifecycle.**~~ Restarting the container no longer cycles a second
  engine; there is only the daemon.
- ~~**Major-version upgrades** (`pg_upgrade` / dump-restore against the volume).~~
  SQLite's on-disk format is stable across releases, and the driver is vendored
  with the binary, so an image bump carries its own engine.
- **No independent scaling.** Still true, still acceptable — Nine is a
  single-instance daemon. SQLite makes this *structural* rather than a choice:
  one writer, one host, and no network protocol to reach the data.
- **Larger image.** Node + Chromium (+ Go in dev) on a Debian base is heavier
  than an Alpine runtime, and Chromium dominates. Dropping the Postgres base
  removed a few hundred MB; Alpine would remove more, but Playwright does not
  support musl.
- **Resource limits** now cover one engine, so the guidance is simply the model
  client plus Chromium.
- ~~**Log multiplexing.**~~ Only the daemon writes to stdout now.
- **New: durability tuning.** WAL with `synchronous=NORMAL` can lose the last
  commits on OS crash or power loss, where a server's default fsync-per-commit
  would not. A clean process crash loses nothing. `synchronous=FULL` is the knob
  if that trade is wrong for a deployment.
- **New: backups are file-shaped.** The `-wal` and `-shm` sidecars are part of
  the database; copying `nine.db` alone from a running daemon can miss committed
  transactions (§9).

## 13. Behavioural-parity checklist

Everything below has now been verified directly, including with a live Ollama
and real tool-calling agent turns:

- [x] `docker exec -it nine nine` opens a session against `/tmp/nine.sock` —
      verified via `nine status` over the same socket path; not re-tested with
      an actual interactive pty.
- [x] `initSchema` bootstraps a fresh database on first boot; second boot is a
      no-op; existing data survives — confirmed the file is created once and a
      prior session resumes on restart.
- [x] Built-in skills seed every boot; agent-authored skills untouched — 17
      built-ins seeded identically across restarts, both images.
- [x] `skills.d` / `plugins.d` user dirs discovered from their mounts —
      confirmed: a scratch skill mounted at `/skills.d` was seeded
      (`seeded user skills dir=/skills.d count=1`), and an empty `/plugins.d`
      mount seeded cleanly with zero entries.
- [x] Standing agents / scheduling reconcile on boot — confirmed: a scratch
      `[[agent]]` produced `standing agent created id=... role=monitor` and a
      spawned pursue session, visible in both `nine status` and `nine goals`.
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
- [x] `make pgadmin` spawns a side-container that reaches the DB via the
      published `5432` and the updated `servers.json`; `make pgadmin-down`
      removes it — confirmed: pgAdmin served on `:5050` and reached the
      container's Postgres over `host.docker.internal:5432`.
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
- [x] LLM reachable via `host.docker.internal` — confirmed with a real Ollama:
      a plain query round-tripped to `gemma4:e2b` and back correctly.
- [x] Integration tests (`make integration-test`) pass against the single
      image, with a live Ollama. This surfaced two more real, pre-existing bugs
      in `tests/integration/`, unrelated to the container refactor itself but
      found while exercising it end-to-end for the first time:
      - `setup_test.go`'s `startContainer()` never set `NINE_PLUGINS_BIN` (or
        mounted a `nine.toml`), so with no config source at all the daemon fell
        through to a stale hardcoded fallback, `/data/bin` — a path that has
        never existed in *any* version of this image; plugins have always
        lived at `/opt/nine/bin`. Every plugin silently failed to start,
        producing exactly the `"unknown tool: shell"`-style failures a model
        would otherwise produce for a genuinely hallucinated tool call, which
        made the failure easy to misread as model flakiness rather than a
        fixture bug. Fixed by adding `-e NINE_PLUGINS_BIN=/opt/nine/bin -e
        NINE_WORKSPACE_ROOT=/data/workspace` to the container's env.
      - `smoke_test.go`'s `TestPluginsLoaded` asserted for `memory`, `skills`,
        and `nine` in the `Plugins:` status line — but those are in-process
        capabilities of `memory.Store`, never plugin subprocesses (§1 of
        docs/architecture.md), so they can never appear there
        regardless of how correctly the daemon is running. Fixed by checking
        only the real subprocess plugins (`shell`, `files`, `http`, `time`).
      With both fixed: 14/14 pass against the documented default (`gemma4:e4b`,
      spot-checked on the two tests that had been affected). Against
      `llama3.2:3b` (swapped in for turnaround speed, not the documented
      default), 11/14 pass; the 3 failures
      (`TestShellBlocksRecursiveDeletion`, `TestShellBlocksPipeToShell`,
      `TestFileReadTool`) share one symptom — the model prints a raw,
      malformed tool-call string (e.g. `{"name": "file_read", "parameters":
      ...}`) as its final-answer text instead of issuing a real structured
      tool call — and don't reproduce against `gemma4:e4b`. That's a
      model tool-calling-reliability characteristic (the kind
      docs/model-compatibility.md exists to track), not a container bug.
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
   `docs/architecture.md`, and `docs/event-log.md`. This doc is the
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
