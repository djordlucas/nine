#!/usr/bin/env bash
# Build the raw-wasm test fixture. Run from the repo root after `make
# quickjs-wasm` has fetched the SDK, or with WASI_SDK pointing at one:
#
#   WASI_SDK=internal/toolvm/quickjs/.build/wasi-sdk-33 sh internal/toolvm/testdata/build.sh
#
# The artifact is committed because it is a few hundred bytes and because the
# test suite must not need a C toolchain — the same reasoning as the QuickJS
# blob, at 1/2000th the size.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
SDK="${WASI_SDK:-${here}/../quickjs/.build/wasi-sdk-33}"
"${SDK}/bin/clang" \
  --target=wasm32-wasip1 --sysroot="${SDK}/share/wasi-sysroot" \
  -mexec-model=reactor -Os \
  -o "${here}/upper.wasm" "${here}/upper.c" \
  -Wl,--export=nine_alloc -Wl,--export=nine_run -Wl,--strip-all -Wl,--gc-sections
shasum -a 256 "${here}/upper.wasm"
wc -c < "${here}/upper.wasm"
