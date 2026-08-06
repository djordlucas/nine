#!/bin/sh
# Hot-reload body of the s6 `nine` service in the `nine-dev` image (see
# docker/s6/dev/s6-rc.d/nine/run, which execs this).
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

# s6 run scripts execute with cwd "/", not the Dockerfile's last WORKDIR, so
# the relative `./cmd/nine` build paths below need this explicit cd.
cd /nine-src

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

# Clean shutdown: s6 forwards docker stop's SIGTERM here (see run script).
trap 'stop; exit 0' INT TERM

if build; then start; else echo "[dev] initial build failed — waiting for a fix"; fi

echo "[dev] watching /nine-src for .go changes…"
while :; do
	# inotifywait runs backgrounded and joined via `wait`, rather than as the
	# loop's direct foreground command: a shell blocked in a foreground child
	# (dash's behavior as Debian's /bin/sh) defers a trapped INT/TERM until
	# that child exits, so `docker stop` would hang idle here for the full
	# grace period, then get force-killed, skipping the daemon's own graceful
	# shutdown entirely. `wait` on a backgrounded pid is interrupted by a
	# trapped signal immediately, so shutdown while idle here stays prompt.
	inotifywait -qq -r -e modify,create,delete,move \
		--exclude '(/\.git/|/vendor/|/dist/|/tmp/|/docs/|/spec/|/plugins/browser/)' /nine-src &
	WATCH=$!
	wait "$WATCH" || break
	# Coalesce editor save bursts.
	sleep 0.3
	echo "[dev] change detected — rebuilding"
	stop
	if build; then start; else echo "[dev] build failed — keeping the last daemon down until it compiles"; fi
done
