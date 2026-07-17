#!/bin/sh
set -e
OUT="${1:-../../bin/browser}"
PLUGIN_DIR="$(cd "$(dirname "$0")" && pwd)"

mkdir -p "$(dirname "$OUT")"

# Write a shell launcher that invokes node with the plugin source path.
# node_modules must be present in $PLUGIN_DIR (run npm install first).
printf '#!/bin/sh\nexec node "%s/index.js" "$@"\n' "$PLUGIN_DIR" > "$OUT"
chmod +x "$OUT"
