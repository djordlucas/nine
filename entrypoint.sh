#!/bin/sh
set -e

# Initialise the /data volume. Plugin binaries and code live in the image under
# /opt/nine and are run from there; built-in skills are embedded in the nine
# binary and seeded into Postgres by the daemon on boot. Postgres holds all
# durable state, so only the workspace lives in /data.
mkdir -p /data/workspace

exec nine daemon
