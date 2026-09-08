#!/bin/sh
# Hot-reload body of the s6 `nine` service in the `nine-dev` image (see
# docker/s6/dev/s6-rc.d/nine/run, which execs this).
#
# The repository is bind-mounted at /nine-src. This builds `nine` from the
# mounted source, starts the daemon, and watches for `.go` changes — on each
# change it rebuilds and restarts the daemon. The Go toolchain and inotify-tools
# come from the Dockerfile `dev` stage.
#
# That one build covers the Go plugins (shell/files/http/time): they are served
# out of the nine binary as `nine plugin serve <name>` (internal/builtins), so
# restarting the daemon picks up plugin edits with no separate build step.
#
# Nothing else is built here. A capability Nine does not implement itself is an
# [[mcp.server]] (docs/browser.md), which this loop never builds or restarts:
# the daemon respawns its bridge on the next start like any other plugin.
set -eu

# s6 run scripts execute with cwd "/", not the Dockerfile's last WORKDIR, so
# the relative `./cmd/nine` build paths below need this explicit cd.
cd /nine-src

DAEMON=""

build() {
	echo "[dev] building nine…"
	# Regenerate the OpenAPI spec from handler annotations before compiling.
	# swag is installed in the dev image (Dockerfile go-build stage); -mod=mod
	# is needed because swag parses the module graph and vendor mode blocks it.
	GOFLAGS=-mod=mod swag init -d /nine-src/internal/api -o /nine-src/internal/api/docs -g docs.go --parseDependency --parseInternal -q 2>/dev/null || \
		echo "[dev] warning: swag init failed, using committed spec"
	go build -o /usr/local/bin/nine ./cmd/nine || return 1
	echo "[dev] build ok"
}

start() {
	nine daemon &
	DAEMON=$!
	echo "[dev] daemon started (pid $DAEMON)"
	# Restart the API server so it picks up the rebuilt binary and reconnects
	# to the fresh daemon socket. s6 manages it as a separate longrun service.
	s6-svc -r /run/service/api 2>/dev/null || true
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
		--exclude '(/\.git/|/vendor/|/dist/|/tmp/|/docs/|/spec/)' /nine-src &
	WATCH=$!
	wait "$WATCH" || break
	# Coalesce editor save bursts.
	sleep 0.3
	echo "[dev] change detected — rebuilding"
	stop
	if build; then start; else echo "[dev] build failed — keeping the last daemon down until it compiles"; fi
done
