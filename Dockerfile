FROM golang:1.26-alpine AS go-build

RUN apk add --no-cache git

WORKDIR /nine-src
COPY . .

# One binary: the shell/files/http/time plugins are served out of nine itself
# (`nine plugin serve <name>`, internal/builtins), so there is no per-plugin
# build loop and nothing to copy into /opt/nine/bin at all.
RUN go build -mod=vendor -o /usr/local/bin/nine ./cmd/nine

# ── s6-overlay fetch stage (shared by dev + runtime) ──────────────────────────
# s6-overlay supervises the daemon (adr/single-container.md): it reaps orphaned
# children (an MCP server's process tree, for one), forwards docker stop's
# SIGTERM, and restarts the service if it exits. Fetched once here and copied
# into both final stages rather than downloaded twice.
FROM debian:bookworm-slim AS s6-fetch
ARG S6_OVERLAY_VERSION=3.2.3.2
ARG TARGETARCH
RUN apt-get update && apt-get dist-upgrade -y && apt-get install -y --no-install-recommends \
      curl xz-utils ca-certificates && \
    rm -rf /var/lib/apt/lists/*
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
    curl -fsSL -o /tmp/noarch.tar.xz \
      "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz" && \
    curl -fsSL -o /tmp/arch.tar.xz \
      "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-${S6_ARCH}.tar.xz" && \
    tar -C /out -Jxpf /tmp/noarch.tar.xz && \
    tar -C /out -Jxpf /tmp/arch.tar.xz

# ── Dev stage (hot-reload) ────────────────────────────────────────────────────
# The source tree is bind-mounted at runtime; docker/dev-entrypoint.sh (wrapped
# by the s6 `nine` service) builds nine and rebuilds/restarts the daemon on any
# .go change — which now covers the Go plugins too, since they are served out of
# that same binary. The Go toolchain lives here (not in the runtime image), so
# this stage is dev-only.
FROM debian:bookworm-slim AS dev

COPY --from=s6-fetch /out/ /

# Debian bookworm's `golang-go` apt package is far behind go.mod's `go 1.26`
# directive, so the toolchain comes from the go-build stage instead (its Go
# binaries are statically linked and run fine on glibc, unrelated to that
# stage's own Alpine base) — this also keeps the dev toolchain in lockstep with
# whatever golang:1.26-alpine tag go-build uses, with nothing to track here.
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

# ── Runtime stage (production) ────────────────────────────────────────────────
FROM debian:bookworm-slim AS runtime

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

ENV NINE_BIN=/opt/nine/bin

COPY --from=go-build /usr/local/bin/nine /usr/local/bin/nine

# /opt/nine/bin is the standalone-plugin directory ([plugins].bin, NINE_BIN).
# mkdir is required, not decorative: it used to be created as a side effect of
# copying compiled plugin binaries in, and those are gone now that the built-ins
# live in the nine binary. The daemon still resolves the path, and a user plugin
# mounted at runtime lands here. The dev stage does the same.
RUN mkdir -p /opt/nine/bin

COPY docker/s6/common/user-bundles.d/ /etc/s6-overlay/user-bundles.d/
COPY docker/s6/runtime/s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod +x /etc/s6-overlay/s6-rc.d/nine/run /etc/s6-overlay/s6-rc.d/api/run

# /data carries all durable state: the SQLite database and the workspace.
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/init"]
