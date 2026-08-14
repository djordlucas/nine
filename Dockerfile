FROM golang:1.26-alpine AS go-build

RUN apk add --no-cache git

WORKDIR /nine-src
COPY . .

# One binary: the shell/files/http/time plugins are served out of nine itself
# (`nine plugin serve <name>`, internal/builtins), so there is no per-plugin
# build loop and nothing to copy into /opt/nine/bin but the browser launcher.
RUN go build -mod=vendor -o /usr/local/bin/nine ./cmd/nine

# ── Node build stage (browser plugin npm deps) ───────────────────────────────
# Shared by both dev and runtime: the browser plugin is Node, so its deps are
# installed here rather than built from the mounted source.
FROM node:alpine AS node-build

WORKDIR /nine-src/plugins/browser
COPY plugins/browser/package.json plugins/browser/package-lock.json ./
RUN npm ci --production

# ── s6-overlay fetch stage (shared by dev + runtime) ──────────────────────────
# s6-overlay supervises the daemon (docs/single-container.md): it reaps the
# zombies the browser plugin's chromium leaves behind, forwards docker stop's
# SIGTERM, and restarts the service if it exits. Fetched once here and copied
# into both final stages rather than downloaded twice.
FROM debian:bookworm-slim AS s6-fetch
ARG S6_OVERLAY_VERSION=3.2.3.2
ARG TARGETARCH
RUN apt-get update && apt-get install -y --no-install-recommends \
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

RUN apt-get update && apt-get install -y --no-install-recommends \
      git \
      inotify-tools \
      chromium \
      nodejs \
      ca-certificates \
      fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

ENV PATH=/usr/local/go/bin:$PATH \
    GOFLAGS=-mod=vendor \
    GOCACHE=/root/.cache/go-build \
    NINE_BIN=/opt/nine/bin \
    PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

# The browser plugin is Node, so the hot-reload loop cannot rebuild it from the
# mounted source the way it rebuilds the Go plugins. Bake it in as immutable
# image content exactly as runtime does, under /opt/nine (never the /nine-src
# mount): the daemon then finds /opt/nine/bin/browser on every start, and the
# hot-reload loop neither rebuilds nor restarts it. Editing plugins/browser/
# therefore needs an image rebuild, which is the same deal as production.
COPY plugins/browser/ /opt/nine/browser/
COPY --from=node-build /nine-src/plugins/browser/node_modules/ /opt/nine/browser/node_modules/
RUN mkdir -p /opt/nine/bin && \
    printf '#!/bin/sh\nexec node /opt/nine/browser/index.js "$@"\n' \
      > /opt/nine/bin/browser && chmod +x /opt/nine/bin/browser

COPY docker/s6/common/user-bundles.d/ /etc/s6-overlay/user-bundles.d/
COPY docker/s6/dev/s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod +x /etc/s6-overlay/s6-rc.d/nine/run

WORKDIR /nine-src

# /data carries all durable state: the SQLite database and the workspace.
VOLUME /data
ENTRYPOINT ["/init"]

# ── Runtime stage (production) ────────────────────────────────────────────────
FROM debian:bookworm-slim AS runtime

COPY --from=s6-fetch /out/ /

# Chromium, system deps, and Node.js runtime for the browser plugin.
# No Go toolchain, git, or source tree: Nine no longer modifies itself.
RUN apt-get update && apt-get install -y --no-install-recommends \
      chromium \
      nodejs \
      ca-certificates \
      fonts-liberation \
    && rm -rf /var/lib/apt/lists/*

ENV PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
    NINE_BIN=/opt/nine/bin

COPY --from=go-build /usr/local/bin/nine /usr/local/bin/nine

# Browser plugin code is immutable image content under /opt/nine — not seeded
# into the /data volume. It is the only plugin left with its own artifact.
COPY plugins/browser/ /opt/nine/browser/
COPY --from=node-build /nine-src/plugins/browser/node_modules/ /opt/nine/browser/node_modules/

# Browser plugin launcher: node runs index.js from the baked-in plugin dir.
RUN printf '#!/bin/sh\nexec node /opt/nine/browser/index.js "$@"\n' \
      > /opt/nine/bin/browser && chmod +x /opt/nine/bin/browser

COPY docker/s6/common/user-bundles.d/ /etc/s6-overlay/user-bundles.d/
COPY docker/s6/runtime/s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod +x /etc/s6-overlay/s6-rc.d/nine/run

# /data carries all durable state: the SQLite database and the workspace.
VOLUME /data
ENTRYPOINT ["/init"]
