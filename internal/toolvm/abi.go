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

	// OutputB64 is a result that is bytes rather than text, base64-encoded
	// because the envelope is UTF-8 JSON and a JSON string cannot hold arbitrary
	// bytes. Set instead of Output, never alongside it.
	//
	// Bytes are not something a language model can read, so this does not reach
	// it directly: the dispatcher writes them to the file store and hands the
	// model a path (adr/tool-output-spill.md). What a tool gets is a way to
	// produce an artifact — a rendered image, a compressed archive — and hand it
	// onward without inventing a place to put it.
	OutputB64 string `json:"output_b64,omitempty"`

	// MediaType optionally describes those bytes ("image/png"). Advisory: it is
	// shown to the model and used to pick a file extension, and nothing branches
	// on it.
	MediaType string `json:"media_type,omitempty"`

	// ErrorDetail is optional structure behind Error. It exists because "the
	// upstream is down" and "your argument was malformed" are different
	// instructions to the model, and a bare sentence makes them the same one.
	//
	// Additive on purpose: `error` stays the message, so a guest that never sets
	// this — every tool written before it existed — behaves exactly as it did.
	// That is why it needs no ABIVersion bump (adr/rich-js-tools.md §8).
	ErrorDetail *ErrorDetail `json:"error_detail,omitempty"`
}

// ErrorDetail is the structured half of a failure. Every field is optional; a
// guest fills in what it knows.
//
// For a `js` tool the harness populates it from the thrown Error — which already
// carries `name`, and by convention `code`, and since ES2022 a `cause` chain that
// was previously discarded. For a `wasm` tool it is two more fields in the JSON
// the tool already writes (see nine_fail_code in nine.h).
type ErrorDetail struct {
	// Name is the error's class — "RangeError", "TypeError". Diagnostic rather
	// than actionable, but it distinguishes a bug in the tool from a bad argument.
	Name string `json:"name,omitempty"`

	// Code is the tool's own stable identifier for this failure, e.g. "E_RANGE".
	// Stable is the point: a model that saw it once can recognize it again, and an
	// operator can grep for it, neither of which survives a reworded sentence.
	Code string `json:"code,omitempty"`

	// Retryable, when set, says whether trying again could plausibly work. This is
	// the field that carries the distinction the whole type exists for, so it is a
	// pointer: unset means "the tool did not say", which is different from "no".
	Retryable *bool `json:"retryable,omitempty"`

	// Cause is the chain behind the failure, outermost first, flattened to
	// messages. Flattened rather than nested because the consumer is a language
	// model reading a sentence, not a debugger walking a tree.
	Cause []string `json:"cause,omitempty"`
}
