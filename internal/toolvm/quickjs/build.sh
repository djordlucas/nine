#!/usr/bin/env bash
#
# Build the QuickJS-NG interpreter to a WASI reactor module (qjs.wasm).
#
# This is NOT part of `make build`. The artifact is committed, so an ordinary
# build needs no wasi-sdk, no clang, and no clone — and, critically, the runtime
# image gains no toolchain (docs/self-modification.md). Run this only on a
# deliberate version bump, via `make quickjs-wasm`, and review the resulting
# hash change in the bump PR.
#
# THE ONE LINE THAT MATTERS: this build must not link js_init_module_std or
# js_init_module_os. Those two opt-in QuickJS modules expose a filesystem API,
# a process API (os.exec), a network fetch (std.urlGet), and two arbitrary-eval
# hooks (std.evalScript / std.loadScript) — in scope before any capability has
# been granted (docs/sandboxed-tools.md §4.1). The stock `qjs` CLI links both,
# which is why a prebuilt module is not a substitute for this script.
# TestQuickJSBlobLinksNoStdOrOS asserts their absence from the built module.

set -euo pipefail

# Two entry points, one set of pins. `--harness-only` rebuilds harness.bc alone,
# which is what an edit to harness.js needs: the bytecode must come from the same
# QuickJS the blob embeds (BC_VERSION is checked at load), and keeping both here
# is what stops the two pins from drifting apart. A harness edit does not touch
# qjs.wasm and does not need reviewing as a binary diff.
harness_only=0
if [ "${1:-}" = "--harness-only" ]; then harness_only=1; fi

QUICKJS_TAG="v0.16.1"
WASI_SDK_VERSION="33"
WASI_SDK_RELEASE="wasi-sdk-33"
WASI_SDK_ASSET="wasi-sdk-33.0"

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="${here}/.build"
out="${here}/qjs.wasm"

# ── toolchain ────────────────────────────────────────────────────────────────
# Pinned to a stable release, not an rc: a reviewer rebuilding must get the same
# compiler. clang embeds paths, so byte-identical output across machines is not
# guaranteed; the goal is that a hash mismatch is investigable, not impossible.

case "$(uname -s)-$(uname -m)" in
  Darwin-arm64)  sdk_platform="arm64-macos"  ;;
  Darwin-x86_64) sdk_platform="x86_64-macos" ;;
  Linux-aarch64|Linux-arm64) sdk_platform="arm64-linux" ;;
  Linux-x86_64)  sdk_platform="x86_64-linux" ;;
  *) echo "unsupported host platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
esac

sdk_dir="${work}/wasi-sdk-${WASI_SDK_VERSION}"
mkdir -p "${work}"

if [ ! -d "${sdk_dir}" ]; then
  url="https://github.com/WebAssembly/wasi-sdk/releases/download/${WASI_SDK_RELEASE}/${WASI_SDK_ASSET}-${sdk_platform}.tar.gz"
  echo "==> fetching ${WASI_SDK_ASSET}-${sdk_platform}"
  curl -fsSL "${url}" -o "${work}/wasi-sdk.tar.gz"
  mkdir -p "${sdk_dir}"
  tar -xzf "${work}/wasi-sdk.tar.gz" -C "${sdk_dir}" --strip-components=1
  rm -f "${work}/wasi-sdk.tar.gz"
fi

CC="${sdk_dir}/bin/clang"
SYSROOT="${sdk_dir}/share/wasi-sysroot"
[ -x "${CC}" ] || { echo "wasi-sdk clang not found at ${CC}" >&2; exit 1; }

# ── source ───────────────────────────────────────────────────────────────────
# A shallow clone at an exact tag. We depend on one reviewed commit of C, not on
# the project's release cadence (docs/sandboxed-tools.md §10.1).

src="${work}/quickjs"
if [ ! -d "${src}" ]; then
  echo "==> cloning quickjs-ng ${QUICKJS_TAG}"
  git clone --depth 1 --branch "${QUICKJS_TAG}" \
    https://github.com/quickjs-ng/quickjs.git "${src}"
fi

# quickjs.c reads VERSION at build time.
version="$(cat "${src}/VERSION" 2>/dev/null || echo "${QUICKJS_TAG#v}")"

# ── compile ──────────────────────────────────────────────────────────────────
# A reactor, not a command: the module has no _start, exports _initialize, and
# stays alive across host calls into it for the duration of one instantiation.
#
# The source list is deliberately explicit rather than a glob. It is upstream's
# own `qjs_sources` (CMakeLists.txt) — the core library — and nothing else.
# quickjs-libc.c, which is where js_init_module_std and js_init_module_os live,
# is absent; upstream gates it behind QJS_BUILD_LIBC and we simply never set it.
# That absence should be legible as a decision rather than an accident of what
# happened to match *.c.
#
# Note that upstream ships a qjs-wasi-reactor.c that looks like exactly what we
# want. It is not: it `#include "qjs.c"`, which links quickjs-libc and calls
# both init functions. It is the CLI, in reactor clothing.

sources=(
  "${src}/quickjs.c"
  "${src}/dtoa.c"
  "${src}/libregexp.c"
  "${src}/libunicode.c"
  "${here}/qjs_host.c"
)

# ── the harness, precompiled ─────────────────────────────────────────────────
# harness.js is compiled to QuickJS module bytecode and embedded in the daemon,
# because parsing it *was* the call: 4.8 ms of a 6 ms call was escaping 32 KB of
# harness into JSON, parsing that JSON, and compiling the result — every call,
# for the same program. Executing it costs 0.14 ms.
#
# qjsc comes from the same pinned checkout as the interpreter. Bytecode carries a
# BC_VERSION the reader checks, so a qjsc from anywhere else fails loudly at the
# first call rather than subtly.
#
# -s strips the source text but keeps debug info, so a stack trace through the
# harness still names lines. Tool code is never bytecode — only this file is.

qjsc="${work}/qjsc"
if [ ! -x "${qjsc}" ] || [ "${src}/quickjs.c" -nt "${qjsc}" ]; then
  echo "==> building qjsc (quickjs-ng ${QUICKJS_TAG})"
  cc -O1 -I"${src}" -o "${qjsc}" \
    "${src}/qjsc.c" "${src}/quickjs.c" "${src}/dtoa.c" \
    "${src}/libregexp.c" "${src}/libunicode.c" "${src}/quickjs-libc.c" \
    -DCONFIG_VERSION="\"${version}\"" -lm -lpthread
fi

echo "==> compiling harness.js to bytecode"
"${qjsc}" -m -s -n "nine:harness" -N nine_harness_bc \
  -b -o "${here}/harness.bc" "${here}/harness.js"

# The blob and the bytecode are built from one source file, and Go embeds the
# bytecode rather than the JavaScript. Recording the source hash is what makes an
# edit to harness.js that skipped this script a test failure instead of a harness
# that silently lags its own source (TestHarnessBytecodeMatchesItsSource).
( cd "${here}" && shasum -a 256 harness.js > harness.bc.sha256 )
echo "==> $(wc -c < "${here}/harness.bc") bytes of bytecode"

if [ "${harness_only}" = "1" ]; then
  echo "==> harness.bc only; qjs.wasm untouched"
  exit 0
fi

echo "==> building qjs.wasm (quickjs-ng ${QUICKJS_TAG}, wasi-sdk-${WASI_SDK_VERSION})"
"${CC}" \
  --target=wasm32-wasip1 \
  --sysroot="${SYSROOT}" \
  -mexec-model=reactor \
  -O2 -flto \
  -DCONFIG_VERSION="\"${version}\"" \
  -DEMSCRIPTEN=0 \
  -D_WASI_EMULATED_PROCESS_CLOCKS \
  -I"${src}" \
  -o "${out}" \
  "${sources[@]}" \
  -lwasi-emulated-process-clocks \
  -Wl,--export=nine_alloc \
  -Wl,--export=nine_run \
  -Wl,--export=nine_harness \
  -Wl,--export=nine_budget_reset \
  -Wl,--no-entry \
  -Wl,--strip-all \
  -Wl,--gc-sections

# ── record ───────────────────────────────────────────────────────────────────
# A binary artifact in the tree is only acceptable if changing it is loud. CI
# re-checks this hash on every run.

( cd "${here}" && shasum -a 256 qjs.wasm > qjs.wasm.sha256 )

cat > "${here}/VERSION" <<EOF
quickjs-ng ${QUICKJS_TAG} · wasi-sdk-${WASI_SDK_VERSION}

Built by build.sh (make quickjs-wasm), which emits qjs.wasm and harness.bc —
the harness as QuickJS bytecode, which is what the daemon embeds. Do not edit
by hand; do not rebuild casually. Bumping either pin above is a deliberate
change reviewed in its own PR, where the qjs.wasm diff and the qjs.wasm.sha256
change are the artifact under review. \`make quickjs-verify\` re-checks the hash.

Editing harness.js needs \`make harness-bc\` only, which rebuilds harness.bc and
leaves qjs.wasm alone.
EOF

echo "==> $(shasum -a 256 "${out}")"
echo "==> $(wc -c < "${out}") bytes"
