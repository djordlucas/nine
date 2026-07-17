#!/bin/sh
# Hot-reload entrypoint for the `nine-dev` docker-compose service.
#
# The repository is bind-mounted at /nine-src. This builds `nine` and the Go
# plugins from the mounted source, starts the daemon, and watches for `.go`
# changes — on each change it rebuilds and restarts the daemon. The Go toolchain
# and inotify-tools come from the Dockerfile `dev` stage.
#
# The browser plugin is not part of this loop: it is Node, and the `dev` stage
# bakes it under /opt/nine/browser as immutable image content. Rebuilding it
# needs an image rebuild, so its source is excluded from the watch below.
set -eu

BIN="${NINE_BIN:-/opt/nine/bin}"
DAEMON=""

build() {
	echo "[dev] building nine + plugins…"
	go build -o /usr/local/bin/nine ./cmd/nine || return 1
	mkdir -p "$BIN"
	for p in shell files http time; do
		go build -o "$BIN/$p" "./plugins/$p" || return 1
	done
	echo "[dev] build ok"
}

start() {
	nine daemon &
	DAEMON=$!
	echo "[dev] daemon started (pid $DAEMON)"
}

stop() {
	[ -n "$DAEMON" ] || return 0
	kill "$DAEMON" 2>/dev/null || true
	wait "$DAEMON" 2>/dev/null || true
	DAEMON=""
}

# Clean shutdown on `docker compose down` / Ctrl-C.
trap 'stop; exit 0' INT TERM

if build; then start; else echo "[dev] initial build failed — waiting for a fix"; fi

echo "[dev] watching /nine-src for .go changes…"
while inotifywait -qq -r -e modify,create,delete,move \
	--exclude '(/\.git/|/vendor/|/dist/|/tmp/|/docs/|/spec/|/plugins/browser/)' /nine-src; do
	# Coalesce editor save bursts.
	sleep 0.3
	echo "[dev] change detected — rebuilding"
	stop
	if build; then start; else echo "[dev] build failed — keeping the last daemon down until it compiles"; fi
done
