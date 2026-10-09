# Base images are pinned by manifest-list digest, not by tag. A tag is mutable,
# so a digest is the only way a rebuild of an old commit produces the image that
# commit was tested against. Dependabot's docker ecosystem bumps these (see
# .github/dependabot.yml); the tag beside each digest is what it reads.
FROM golang:1.26.9-alpine@sha256:cdfd4fe2da6b225d8b40c6b7a105736e548e83ff56d5d8f9394446eeb5eb84e0 AS go-build

WORKDIR /nine-src
COPY . .

# VERSION is what `nine version` reports and what the image's
# org.opencontainers.image.version label carries. The release workflow passes
# the git tag; a local build falls back to "dev" rather than silently claiming
# a release number.
ARG VERSION=dev

# One binary: the `shell` plugin is served out of nine itself
# (`nine plugin serve shell`, internal/builtins) and the file, fetching and clock
# tools are shipped sandboxed tools embedded in it, so there is no per-plugin
# build loop and nothing to copy into /opt/nine/bin at all.
#
# CGO_ENABLED=0 makes it a static binary — the SQLite driver is a pure-Go
# translation, not a cgo binding, so nothing needs libc. -trimpath strips local
# filesystem paths from the binary so the output does not depend on where it was
# built. -w -s drop DWARF and the symbol table.
RUN CGO_ENABLED=0 go build \
      -mod=vendor \
      -trimpath \
      -ldflags "-s -w -X main.Version=${VERSION}" \
      -o /usr/local/bin/nine ./cmd/nine

# ── s6-overlay fetch stage (shared by dev + runtime) ──────────────────────────
# s6-overlay supervises the daemon (adr/single-container.md): it reaps orphaned
# children (an MCP server's process tree, for one), forwards docker stop's
# SIGTERM, and restarts the service if it exits. Fetched once here and copied
# into both final stages rather than downloaded twice.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS s6-fetch
ARG S6_OVERLAY_VERSION=3.2.3.2
ARG TARGETARCH
RUN apt-get update && apt-get dist-upgrade -y && apt-get install -y --no-install-recommends \
      curl xz-utils ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Each s6-overlay tarball is verified against its published SHA-256 before it is
# unpacked. Without this the build trusts whatever the release URL returns, and
# an unverified tarball unpacks as root into /.
COPY docker/s6-overlay.sha256 /tmp/s6-overlay.sha256
RUN case "$TARGETARCH" in \
      amd64)   S6_ARCH=x86_64 ;; \
      arm64)   S6_ARCH=aarch64 ;; \
      arm)     S6_ARCH=arm ;; \
      386)     S6_ARCH=i686 ;; \
      ppc64le) S6_ARCH=powerpc64le ;; \
      s390x)   S6_ARCH=s390x ;; \
      riscv64) S6_ARCH=riscv64 ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    mkdir -p /out && \
    curl -fsSL -o /tmp/s6-overlay-noarch.tar.xz \
      "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz" && \
    curl -fsSL -o "/tmp/s6-overlay-${S6_ARCH}.tar.xz" \
      "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-${S6_ARCH}.tar.xz" && \
    cd /tmp && \
    grep -E "  (s6-overlay-noarch|s6-overlay-${S6_ARCH})\.tar\.xz\$" /tmp/s6-overlay.sha256 > /tmp/want.sha256 && \
    test "$(wc -l < /tmp/want.sha256)" -eq 2 && \
    sha256sum -c /tmp/want.sha256 && \
    tar -C /out -Jxpf /tmp/s6-overlay-noarch.tar.xz && \
    tar -C /out -Jxpf "/tmp/s6-overlay-${S6_ARCH}.tar.xz"

# ── Dev stage (hot-reload) ────────────────────────────────────────────────────
# The source tree is bind-mounted at runtime; docker/dev-entrypoint.sh (wrapped
# by the s6 `nine` service) builds nine and rebuilds/restarts the daemon on any
# .go change — which now covers the Go plugins too, since they are served out of
# that same binary. The Go toolchain lives here (not in the runtime image), so
# this stage is dev-only.
#
# This stage runs as root, unlike runtime. It is never published: it exists to
# rebuild bind-mounted source and own a shared Go cache, both of which want the
# host uid. `make up-hot` builds it locally.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS dev

COPY --from=s6-fetch /out/ /

# Debian bookworm's `golang-go` apt package is far behind go.mod's `go 1.26`
# directive, so the toolchain comes from the go-build stage instead (its Go
# binaries are statically linked and run fine on glibc, unrelated to that
# stage's own Alpine base) — this also keeps the dev toolchain in lockstep with
# whatever golang:1.26-alpine digest go-build uses, with nothing to track here.
# The nine binary itself is pure Go — the SQLite driver is a Go translation of
# SQLite, not a cgo binding — so it likewise carries no libc dependency across
# stages.
COPY --from=go-build /usr/local/go /usr/local/go

# nodejs + npm are here for `npx`-launched MCP servers, which is how the dev
# image reaches anything Nine does not build itself — a browser included
# (docs/browser.md). The dev image carries them where runtime does not, because
# this is where an operator experiments with a server before committing to it.
# No browser: `npx @playwright/mcp@<ver> install-browser chrome-for-testing`
# fetches a matched build on first use, into the /data volume via
# PLAYWRIGHT_BROWSERS_PATH, so it survives a container restart without bloating
# the image for everyone who never browses. (That is the command, not `npx
# playwright install chromium` — see docs/browser.md §1.)
RUN apt-get update && apt-get dist-upgrade -y && apt-get install -y --no-install-recommends \
      git \
      inotify-tools \
      nodejs \
      npm \
      ca-certificates \
      fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

ENV PATH=/usr/local/go/bin:$PATH \
    GOFLAGS=-mod=vendor \
    GOCACHE=/root/.cache/go-build \
    NINE_BIN=/opt/nine/bin \
    PLAYWRIGHT_BROWSERS_PATH=/data/.playwright

# /opt/nine/bin is the standalone-plugin directory ([plugins].bin, NINE_BIN).
# Nothing ships into it now that the Go built-ins live in the nine binary, but
# the daemon still resolves it, and a user plugin mounted at runtime lands here.
RUN mkdir -p /opt/nine/bin

COPY docker/s6/common/user-bundles.d/ /etc/s6-overlay/user-bundles.d/
COPY docker/s6/dev/s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod +x /etc/s6-overlay/s6-rc.d/nine/run /etc/s6-overlay/s6-rc.d/api/run

WORKDIR /nine-src

# /data carries all durable state: the SQLite database and the workspace.
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/init"]

# ── Runtime stage (production, published) ─────────────────────────────────────
# This is the stage published to ghcr.io/djordlucas/nine and docker.io/djordlucas/nine.
FROM debian:bookworm-slim@sha256:3783cc01769c7b2b1b83a5c5ad96c815348e28ed7da68e2e3687004faa906251 AS runtime

COPY --from=s6-fetch /out/ /

# No Go toolchain, git, or source tree: Nine no longer modifies itself. No Node
# and no chromium either — the runtime image ships what Nine itself needs, and
# an MCP server is by definition something Nine does not build. An operator who
# wants one in production derives an image from this and installs its runtime,
# or points a [[mcp.server]] at a hosted url. docs/browser.md walks through both
# for the browser case.
RUN apt-get update && apt-get dist-upgrade -y && apt-get install -y --no-install-recommends \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# The daemon and the API server run as this unprivileged user. s6-overlay itself
# stays root — it supervises, reaps orphans, and forwards signals, which need
# it — and each service drops to `nine` via s6-setuidgid in its run script.
#
# uid/gid 1000 is deliberate: it matches the first ordinary user on most Linux
# hosts, so a bind-mounted /data from the host is writable without a chown.
RUN groupadd --system --gid 1000 nine && \
    useradd --system --uid 1000 --gid 1000 --home-dir /data --shell /usr/sbin/nologin nine

ENV NINE_BIN=/opt/nine/bin

COPY --from=go-build /usr/local/bin/nine /usr/local/bin/nine

# /opt/nine/bin is the standalone-plugin directory ([plugins].bin, NINE_BIN).
# mkdir is required, not decorative: it used to be created as a side effect of
# copying compiled plugin binaries in, and those are gone now that the built-ins
# live in the nine binary. The daemon still resolves the path, and a user plugin
# mounted at runtime lands here. The dev stage does the same.
#
# /data is created and owned here so the image works with no volume at all. A
# named or bind-mounted volume shadows this, which is what init-perms fixes at
# boot.
RUN mkdir -p /opt/nine/bin /data/workspace && \
    chown -R nine:nine /data && \
    chmod 0755 /opt/nine/bin

# A default config so `docker run ghcr.io/djordlucas/nine` works with no clone
# and no mounted file. Conservative on purpose — sandboxed tools off, every path
# under /data. An operator's own file mounted at /nine.toml shadows it, and the
# NINE_LLM_* environment overrides work without one.
COPY docker/nine.toml /nine.toml

# The runtime bundle adds init-perms on top of the shared nine + api services.
# It is stage-specific because the dev stage has no init-perms service, and a
# bundle naming a service that does not exist fails the s6 boot.
COPY docker/s6/common/user-bundles.d/  /etc/s6-overlay/user-bundles.d/
COPY docker/s6/runtime/user-bundles.d/ /etc/s6-overlay/user-bundles.d/
COPY docker/s6/runtime/s6-rc.d/        /etc/s6-overlay/s6-rc.d/
COPY docker/s6/runtime/scripts/        /etc/s6-overlay/scripts/
RUN chmod +x /etc/s6-overlay/s6-rc.d/nine/run \
             /etc/s6-overlay/s6-rc.d/api/run \
             /etc/s6-overlay/scripts/init-perms.sh

# /data carries all durable state: the SQLite database and the workspace.
VOLUME /data
EXPOSE 8080

# `nine status` reaches the daemon over its Unix socket, so it probes the thing
# that matters without adding curl to the image.
#
# The grep is required, not defensive: with no daemon reachable, `nine status`
# prints "no daemon running" and exits 0, because the query itself succeeded.
# A bare `CMD nine status` would therefore report healthy against a dead daemon.
# "Uptime:" is the first line of a real status (internal/cli.printStatus), and
# tests/docker asserts the container reaches healthy.
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD /usr/local/bin/nine status 2>&1 | grep -q '^Uptime:'

ENTRYPOINT ["/init"]

# Labels last: they change on every release, and a trailing layer of metadata
# does not invalidate the cache for anything above it. VERSION and the two
# git-derived values are passed by the release workflow.
ARG VERSION=dev
ARG VCS_REF=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="nine" \
      org.opencontainers.image.description="Self-contained AI agent daemon" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${VCS_REF}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.source="https://github.com/djordlucas/nine" \
      org.opencontainers.image.documentation="https://github.com/djordlucas/nine/blob/main/docs/docker-image.md" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.vendor="The Nine Authors" \
      org.opencontainers.image.base.name="debian:bookworm-slim"
