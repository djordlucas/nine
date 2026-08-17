// Package toolvm is the sandboxed tool host: a wasm runtime that executes tools
// with an explicitly conferred set of capabilities and nothing else.
//
// It is a second backend behind the same dispatcher the native plugin system
// uses (spec/contracts/plugin.md), not a replacement for it. Nothing about
// plugins changes, and a deployment that leaves [tools] enabled unset behaves
// exactly as it did before this package existed.
//
// The design note is docs/sandboxed-tools.md; the normative contract is
// spec/contracts/toolvm.md (R-TVM.*); the authoring guide is
// docs/writing-sandboxed-tools.md.
//
// Two things are worth knowing before reading further:
//
//   - The host knows nothing about JavaScript. A tool is a wasm module exporting
//     the ABI below. The `js` kind works by making the module a QuickJS
//     interpreter and the tool's source its input — an implementation detail of
//     one kind, not an architectural layer.
//   - Capabilities that are not in the grant table are not "denied"; they are
//     structurally absent. wazero exports no network, no process spawn, and no
//     filesystem beyond the pre-opens it is handed, so there is nothing to
//     bypass rather than a check to defeat.
package toolvm

import _ "embed"

// CHeader is nine.h: this ABI rendered as a C header, for tools written in C.
//
// It is embedded rather than shipped as a file to copy, for the reason docs/
// and spec/ are (docs/embed.go): a header describing the ABI must match the
// binary that implements it, and a copy on disk drifts silently. `nine tool
// header` writes the one belonging to the running version, and
// TestCHeaderMatchesABIVersion keeps its NINE_ABI_VERSION honest.
//
//go:embed nine.h
var CHeader string

// ABIVersion is the version of the guest contract: the exported functions
// below, their signatures, and the meaning of the bytes crossing between them.
//
// It is independent of plugin.ProtocolVersion, which versions the native plugin
// wire contract and is untouched by this package (docs/versioning.md). A module
// declaring an unsupported ABI is refused at load rather than called and left to
// fail in some module-specific way.
//
// v1 is the initial contract.
const ABIVersion = 1

// The guest ABI. A tool module — hand-written wasm, or the QuickJS blob acting
// for a `js` tool — must export exactly these two functions.
//
//	nine_alloc(size i32) -> i32
//	  Reserve `size` bytes in the guest's linear memory and return the offset.
//	  The host writes the call's input there before calling nine_run.
//
//	  The host always asks for one byte more than it intends to write, and
//	  writes a NUL into it, so the input at `ptr` is BOTH length-delimited and
//	  NUL-terminated. A guest may therefore treat it as a C string. This is a
//	  guarantee, not an accident: QuickJS requires `buf[buf_len] == 0` for
//	  JS_Eval and JS_ParseJSON, and a host that skipped it would produce a
//	  parser that fails for about one input length in sixteen — depending on
//	  nothing but what the guest allocator left in the next byte.
//
//	nine_run(ptr i32, len i32) -> i64
//	  Run the tool against the `len` bytes of UTF-8 JSON at `ptr`, and return
//	  the result packed as (offset << 32) | length — one i64 rather than a
//	  multi-value return, because every guest language can build an i64 and
//	  that keeps a raw-.wasm author's job trivial.
//
// There is deliberately no `free`: the instance is destroyed when the call
// returns (docs/sandboxed-tools.md §3), so every allocation is reclaimed
// wholesale and a guest that tracks lifetimes gains nothing for the trouble.
const (
	exportAlloc = "nine_alloc"
	exportRun   = "nine_run"
)

// hostModule is the name of the one module of host functions the guest may
// import. Its whole contents are the `log` capability; everything with reach is
// either a wazero pre-open or absent.
const hostModule = "nine"

// Result is what a guest returns: the JSON document nine_run hands back. It is
// an envelope rather than a bare string so a tool's own failure — a thrown
// exception, a bad argument — arrives as a normal tool error the model can read
// and retry against, distinct from the host failing to run it at all.
type Result struct {
	OK     bool   `json:"ok"`
	Output string `json:"output,omitempty"`
	Error  string `json:"error,omitempty"`
}
