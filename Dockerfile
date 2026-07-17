FROM golang:1.26-alpine AS go-build

RUN apk add --no-cache git

WORKDIR /nine-src
COPY . .

RUN go build -mod=vendor -o /usr/local/bin/nine ./cmd/nine && \
    mkdir -p /out/bin && \
    for p in shell files http time; do \
      go build -mod=vendor -o /out/bin/$p ./plugins/$p; \
    done

# ── Node build stage (browser plugin npm deps) ───────────────────────────────
# Shared by both dev and runtime: the browser plugin is Node, so its deps are
# installed here rather than built from the mounted source.
FROM node:alpine AS node-build

WORKDIR /nine-src/plugins/browser
COPY plugins/browser/package.json plugins/browser/package-lock.json ./
RUN npm ci --production

# ── Dev stage (hot-reload) ────────────────────────────────────────────────────
# The source tree is bind-mounted at runtime; docker/dev-entrypoint.sh builds
# nine + the Go plugins and rebuilds/restarts the daemon on any .go change. The
# Go toolchain lives here (not in the runtime image), so this stage is dev-only.
FROM golang:1.26-alpine AS dev
RUN apk add --no-cache \
    git \
    inotify-tools \
    chromium \
    nss \
    freetype \
    harfbuzz \
    ca-certificates \
    ttf-freefont \
    eudev \
    nodejs
ENV GOFLAGS=-mod=vendor \
    NINE_BIN=/opt/nine/bin \
    PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

# The browser plugin is Node, so dev-entrypoint.sh cannot build it from the
# mounted source the way it builds the Go plugins. Bake it in as immutable image
# content exactly as runtime does, under /opt/nine (never the /nine-src mount):
# the daemon then finds /opt/nine/bin/browser on every start, and the hot-reload
# loop neither rebuilds nor restarts it. Editing plugins/browser/ therefore needs
# an image rebuild, which is the same deal as production.
COPY plugins/browser/ /opt/nine/browser/
COPY --from=node-build /nine-src/plugins/browser/node_modules/ /opt/nine/browser/node_modules/
RUN mkdir -p /opt/nine/bin && \
    printf '#!/bin/sh\nexec node /opt/nine/browser/index.js "$@"\n' \
      > /opt/nine/bin/browser && chmod +x /opt/nine/bin/browser

WORKDIR /nine-src
ENTRYPOINT ["sh", "docker/dev-entrypoint.sh"]

# ── Runtime stage (production) ────────────────────────────────────────────────
FROM alpine AS runtime

# Chromium, system deps, and Node.js runtime for the browser plugin.
# No Go toolchain, git, or source tree: Nine no longer modifies itself.
# Alpine uses eudev, not udev.
RUN apk add --no-cache \
    chromium \
    nss \
    freetype \
    harfbuzz \
    ca-certificates \
    ttf-freefont \
    eudev \
    nodejs

ENV PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/usr/bin/chromium-browser
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

COPY --from=go-build /usr/local/bin/nine /usr/local/bin/nine

# Plugin binaries and browser plugin code are immutable image content under
# /opt/nine — not seeded into the /data volume.
COPY --from=go-build /out/bin/ /opt/nine/bin/
COPY plugins/browser/ /opt/nine/browser/
COPY --from=node-build /nine-src/plugins/browser/node_modules/ /opt/nine/browser/node_modules/

# Browser plugin launcher: node runs index.js from the baked-in plugin dir.
RUN printf '#!/bin/sh\nexec node /opt/nine/browser/index.js "$@"\n' \
      > /opt/nine/bin/browser && chmod +x /opt/nine/bin/browser

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV NINE_BIN=/opt/nine/bin

VOLUME /data
ENTRYPOINT ["/entrypoint.sh"]
