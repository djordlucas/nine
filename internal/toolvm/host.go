package toolvm

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"nine/internal/llm"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Resource bounds, orthogonal to capabilities and always on
// (docs/sandboxed-tools.md §3).
const (
	// DefaultTimeout is the per-call wall clock. It is also, in the absence of
	// fuel metering in wazero, the *only* CPU bound: a spinning tool is closed
	// out from under itself when the context deadline fires and the model
	// observes an ordinary failure. An operator running many concurrent sessions
	// is trusting this deadline, not a work budget — which is worth knowing
	// rather than assuming.
	DefaultTimeout = 5 * time.Second

	// DefaultMemoryMB caps a call's linear memory: 16 MiB, or 256 wasm pages.
	DefaultMemoryMB = 16

	// DefaultMaxOps is the per-call work budget for a `js` tool, in operations.
	//
	// Calibrated rather than guessed. Every shipped tool finishes inside a single
	// interrupt check (under 10,000 operations); parsing 20,000 CSV rows through
	// nine:csv costs 1.7M, sorting 100,000 numbers 2.1M, and a one-million-pass
	// loop 2.0M. Fifty million is roughly a 25-million-pass tight loop, or 25x
	// the heaviest realistic workload measured.
	//
	// It is deliberately tighter than the wall clock. On the machine this was
	// calibrated on the budget runs out in about 750ms, so a runaway tool now
	// fails in well under a second instead of burning the full 5s deadline — and
	// fails with a sentence naming the key to raise, which a deadline never gave
	// anyone. The cost of that is real and worth stating: a tool that today burns
	// four seconds of CPU will meet this budget first. Tool calls here are
	// LLM-gated events where an ordinary call costs ~1.25ms, so a call doing 50M
	// operations is already 600x normal, but an operator running something
	// genuinely heavy will need [tool.<name>] max_ops — and will be told so.
	DefaultMaxOps = 50_000_000

	// opsPerCheck converts the operator's budget into the interrupt checks the
	// guest counts. QuickJS polls its interrupt handler once per
	// JS_INTERRUPT_COUNTER_INIT (10,000) polls, and a poll is a backward jump or
	// a call — loop iterations and function calls, not every bytecode op.
	//
	// So "operations" is an approximation, and deliberately the operator-facing
	// one: a number with four zeroes on the end is easier to reason about than a
	// count of interrupt checks, and the granularity of the bound (±10,000
	// operations) is irrelevant at any budget worth setting. If a future QuickJS
	// changes its constant, the budget shifts by that factor and nothing breaks.
	opsPerCheck = 10_000

	// DefaultMaxConcurrent bounds simultaneous calls across every tool.
	//
	// Without it the memory cap is per call and nothing multiplies it: each
	// concurrent call is an instance entitled to MemoryMB, and the number of
	// concurrent calls was whatever the turns in flight happened to ask for. The
	// job sweeper has had this bound since it existed ([tools] job_workers); the
	// turn-driven path had none. Eight against the 16 MiB default is 128 MiB of
	// worst case, which is high enough that an ordinary deployment never queues.
	DefaultMaxConcurrent = 8

	wasmPageSize = 64 * 1024
)

// Config configures the host. The zero value is a disabled host, which is what
// makes this whole subsystem additive: nothing loads and nothing changes.
type Config struct {
	// UserDir holds developer tools. Empty loads none.
	UserDir string
	// Grants are the operator's `[tool.<name>]` tables, keyed by tool name.
	Grants map[string]Grant
	// Timeout, MemoryMB and MaxConcurrent override the defaults above when
	// non-zero.
	Timeout       time.Duration
	MemoryMB      int
	MaxConcurrent int

	// MaxOps is the per-call work budget for a `js` tool, in operations, from
	// `[tools] max_ops`. Zero uses DefaultMaxOps; negative disables the budget,
	// which leaves the wall clock as the only bound, as it was before this
	// existed.
	MaxOps int

	// MaxOpsPerTool overrides MaxOps for named tools, from `[tool.<name>]
	// max_ops`. A tool that legitimately grinds should not force its budget onto
	// every other tool, which is the same argument per-tool timeouts already won.
	MaxOpsPerTool map[string]int

	// Timeouts overrides Timeout for named tools, from `[tool.<name>] timeout`.
	// One tool that legitimately takes twenty seconds should not force twenty
	// seconds onto a tool with an infinite loop, and this deadline is the only
	// CPU bound there is.
	Timeouts map[string]time.Duration

	// TouchGenerated, when set, records that a generated tool was called, for LRU
	// eviction. The daemon wires it to the store; this package has none.
	TouchGenerated func(name string)

	// StateStore backs the `state` capability. Nil leaves the capability
	// unusable: a tool granted state gets a named error rather than a silently
	// forgetful store, because a cache that never hits is indistinguishable from
	// a slow tool and would be debugged for hours.
	StateStore StateStore
}

// Tool is one loaded sandboxed tool, ready to dispatch.
type Tool struct {
	Name        string
	DisplayName string
	Description string
	InputSchema json.RawMessage
	Kind        Kind
	// Grant is the resolved, effective capability set — what the operator
	// conferred, never what the manifest asked for.
	Grant Grant
	// Timeout is this tool's per-call deadline: the operator's override for it,
	// or zero to use the host's. Resolved at load so a call reads one field.
	Timeout time.Duration

	// MaxOps is this tool's resolved work budget in operations, 0 when unmetered.
	// Kept for `nine tools show`; the guest is handed checks, not operations.
	MaxOps int
	// checks is MaxOps in the guest's own unit, resolved at registration because
	// it is baked into the cached envelope head.
	checks uint32
	// ManifestPath is where this tool came from, for `nine tools show`. Empty for
	// a generated tool, which came from the store rather than a file.
	ManifestPath string
	// Shipped marks a first-party tool compiled into the binary (the shipped
	// tier). Like Generated it changes nothing about how the tool runs — same
	// sandbox, same bounds, same capability resolution — only where its source
	// and its grant came from.
	Shipped bool

	// Resumable means this tool may end a call with a `continue` envelope and be
	// run as a long-running job. From the manifest; it confers no reach, so it is
	// a shape property rather than a capability.
	Resumable bool

	// Generated marks a tool Nine wrote itself (§5.2) rather than one an operator
	// installed. It changes nothing about how the tool runs — same sandbox, same
	// bounds, same capability resolution — only where its code and its ceiling
	// came from.
	Generated bool

	// module is the compiled wasm. For a KindJS tool it is the shared QuickJS
	// blob; for KindWasm it is the tool's own module. Compilation happens once,
	// instantiation happens per call.
	module wazero.CompiledModule
	// source is the tool's JavaScript, for KindJS only.
	source string
	// modules is what this tool's calls ship to the guest resolver: specifier ->
	// source, narrowed to the `nine:*` modules this source can actually name
	// (reachableStdlib). It is not the allowlist — see Tool.Imports, which reports
	// what the tool *may* import. Empty for a `wasm` tool, which has no resolver.
	modules map[string]string

	// head caches the constant part of this tool's guest input — see
	// envelopeHead. Built on first call rather than at load, so a host holding a
	// large generated catalog does not encode an envelope per tool for tools no
	// turn asks for.
	headOnce sync.Once
	head     []byte
	headErr  error
}

// Status is the outcome of loading one candidate, for `nine tools` reporting. A
// skipped tool is surfaced here rather than silently dropped: a tool an operator
// installed and that is not running is exactly what they need told.
type Status struct {
	Name         string `json:"name"`
	ManifestPath string `json:"manifest_path"`
	Loaded       bool   `json:"loaded"`
	Generated    bool   `json:"generated,omitempty"`
	Shipped      bool   `json:"shipped,omitempty"`
	Kind         string `json:"kind,omitempty"`
	Capabilities string `json:"capabilities,omitempty"`
	Err          string `json:"err,omitempty"`
}

// Host owns the wazero runtime, the compiled-module cache, and the tool
// registry. One per daemon.
type Host struct {
	cfg     Config
	rt      wazero.Runtime
	timeout time.Duration
	// memoryMB is the resolved per-call memory cap, kept so a failure can name
	// the limit it hit rather than leaving an author to guess at it.
	memoryMB int
	// maxOps is the resolved default work budget, in operations. 0 is unmetered.
	maxOps int

	mu     sync.RWMutex
	tools  map[string]*Tool
	status []Status
	// generatedStatus is kept apart from status so reloading the developer-tool
	// directory does not erase the generated tier's outcomes, or vice versa.
	generatedStatus []Status
	// shippedStatus is likewise kept apart, so neither a developer-tool reload
	// nor a generated-tool reload erases the first-party set.
	shippedStatus []Status
	// shippedWorkspace is the host directory shipped tools that declare fs are
	// mounted at, under the fixed guest path /work.
	shippedWorkspace ShippedWorkspace
	agent            AgentConfig

	// sem bounds simultaneous calls: one slot is held from just before
	// instantiation until just after the instance is closed. Buffered to the
	// resolved limit, so an operator raising it raises the worst-case memory with
	// it, deliberately.
	sem chan struct{}

	// qjs is the compiled QuickJS blob, shared by every `js` tool. Compiling it
	// is by far the most expensive thing this package does (~1 MB of wasm), so it
	// happens once at open and never per call.
	qjs wazero.CompiledModule
}

// Open creates the host and compiles the QuickJS blob. It does not load any
// tools; call Load for that.
//
// Every wazero denial the design relies on is a property of this runtime
// configuration, so it is worth being explicit about what is *not* here: no
// filesystem is mounted, no environment is passed, no network exists to
// configure, and the only host functions exported are the ones in
// registerHostFunctions. A capability absent from that list is not denied — it
// has no function to call.
func Open(ctx context.Context, cfg Config) (*Host, error) {
	memMB := cfg.MemoryMB
	if memMB <= 0 {
		memMB = DefaultMemoryMB
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	rtCfg := wazero.NewRuntimeConfig().
		// The deadline can only stop a running guest if the runtime is allowed to
		// close the module out from under it. Without this a spinning tool ignores
		// the context entirely and the timeout is decorative.
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(uint32(memMB * 1024 * 1024 / wasmPageSize)) //nolint:gosec // bounded by config

	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)

	// WASI is instantiated because the QuickJS blob is a WASI reactor and needs
	// its allocator and clock shims. It confers nothing on its own: with no
	// pre-opens, path_open has nothing to open, and preview1 has no proc_spawn
	// and no outbound socket call in the first place — os.exec and std.urlGet
	// would fail here even if this build had linked them, which it does not
	// (docs/sandboxed-tools.md §4.1).
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		rt.Close(ctx) //nolint:errcheck // failing open; the close error is noise
		return nil, fmt.Errorf("toolvm: instantiate wasi: %w", err)
	}

	maxConc := cfg.MaxConcurrent
	if maxConc <= 0 {
		maxConc = DefaultMaxConcurrent
	}
	// Negative is the operator turning the budget off, which is distinct from
	// leaving it unset — so it cannot collapse into the default the way a
	// non-positive memory or timeout does.
	maxOps := cfg.MaxOps
	if maxOps == 0 {
		maxOps = DefaultMaxOps
	} else if maxOps < 0 {
		maxOps = 0
	}

	h := &Host{
		cfg:      cfg,
		rt:       rt,
		timeout:  timeout,
		memoryMB: memMB,
		maxOps:   maxOps,
		tools:    map[string]*Tool{},
		sem:      make(chan struct{}, maxConc),
	}

	if err := h.registerHostFunctions(ctx); err != nil {
		rt.Close(ctx) //nolint:errcheck
		return nil, err
	}

	qjs, err := rt.CompileModule(ctx, quickJSBlob())
	if err != nil {
		rt.Close(ctx) //nolint:errcheck
		return nil, fmt.Errorf("toolvm: compile quickjs blob: %w", err)
	}
	h.qjs = qjs

	return h, nil
}

// registerHostFunctions exports the "nine" module: `log`, `caps`, `http`, and
// `state`.
//
// Exporting a function is not conferring a capability. `log` and `caps` leak
// nothing and are granted to every tool; `http` and `state` are exported to
// every tool because a wasm module's imports are fixed at compile time and the
// QuickJS blob is shared by all of them, but each reads its grant per call from
// the context and refuses a tool that has none. Everything else with reach is a
// wazero pre-open or is structurally absent.
func (h *Host) registerHostFunctions(ctx context.Context) error {
	_, err := h.rt.NewHostModuleBuilder(hostModule).
		NewFunctionBuilder().
		WithFunc(func(ctx context.Context, mod api.Module, ptr, size uint32) {
			// The tool name rides on the context so one log line names its
			// source; a call always has one, but a defensive read costs nothing.
			name, _ := ctx.Value(toolNameKey{}).(string)
			buf, ok := mod.Memory().Read(ptr, size)
			if !ok {
				return
			}
			msg := string(buf)
			// The daemon log keeps every line in full, as it always has; the
			// per-call buffer is the bounded copy a failure can carry back to
			// the model, which cannot read the daemon's log.
			slog.Info("sandboxed tool log", "tool", name, "msg", msg)
			logFor(ctx).add(msg)
		}).
		Export("log").
		NewFunctionBuilder().
		WithFunc(h.hostHTTP).
		Export("http").
		NewFunctionBuilder().
		WithFunc(h.hostCaps).
		Export("caps").
		NewFunctionBuilder().
		WithFunc(h.hostState).
		Export("state").
		Instantiate(ctx)
	if err != nil {
		return fmt.Errorf("toolvm: export host functions: %w", err)
	}
	return nil
}

// hostHTTP is the guest's `nine.http`. The daemon makes the request; the guest
// never touches a socket, and never learns an IP.
//
// It is exported to every tool, granted or not, because a wasm module's imports
// are fixed at compile time and the QuickJS blob is shared by all `js` tools —
// so the function must exist for the module to instantiate at all. That is not a
// capability leak: the *grant* is read per call from the context below, and a
// tool without one is refused here before anything is parsed. The capability
// still lives entirely in the grant, which is what §6.3 requires; what is shared
// is the import, not the permission.
func (h *Host) hostHTTP(ctx context.Context, mod api.Module, ptr, size uint32) uint64 {
	name, _ := ctx.Value(toolNameKey{}).(string)
	grant, _ := ctx.Value(httpGrantKey{}).(*HTTPGrant)

	if grant == nil {
		return writeGuest(ctx, mod, httpResponse{
			Error: "blocked: this tool was not granted the net.http capability"})
	}

	req, ok := mod.Memory().Read(ptr, size)
	if !ok {
		return writeGuest(ctx, mod, httpResponse{Error: "unreadable request"})
	}
	// Copy before doing anything slow: the read above aliases guest memory.
	raw := make([]byte, len(req))
	copy(raw, req)

	return writeGuest(ctx, mod, h.doHTTP(ctx, name, *grant, raw))
}

// hostCaps is the guest's `nine.caps`: the calling tool's resolved grant, as
// JSON.
//
// It confers nothing. Every capability is enforced somewhere else — the
// filesystem by wazero's pre-opens, the environment by what WithEnv passed,
// net.http by the per-call grant lookup in hostHTTP — and a tool learning its own
// grant learns nothing it could not discover by trying. What it buys is a decent
// error: `fs.read is not granted to this tool` instead of an ENOENT for a file
// that plainly exists, which is a genuinely confusing way to find out.
func (h *Host) hostCaps(ctx context.Context, mod api.Module) uint64 {
	grant, _ := ctx.Value(grantKey{}).(*Grant)
	var out []byte
	if grant == nil {
		out = []byte(`{}`)
	} else {
		var err error
		out, err = json.Marshal(grant.describe())
		if err != nil {
			out = []byte(`{}`)
		}
	}
	return writeGuestBytes(ctx, mod, out)
}

// grantKey carries the calling tool's whole resolved grant into host functions.
type grantKey struct{}

// writeGuest marshals resp and hands it back through the guest's own allocator,
// returning it packed the same way nine_run's result is. Calling back into
// nine_alloc is how a host function returns variable-length data without a
// second shared buffer to reason about.
func writeGuest(ctx context.Context, mod api.Module, resp httpResponse) uint64 {
	out, err := json.Marshal(resp)
	if err != nil {
		out = []byte(`{"error":"could not encode response"}`)
	}
	return writeGuestBytes(ctx, mod, out)
}

// writeGuestBytes hands `out` back through the guest's own allocator, packed the
// same way nine_run's result is.
func writeGuestBytes(ctx context.Context, mod api.Module, out []byte) uint64 {
	alloc := mod.ExportedFunction(exportAlloc)
	if alloc == nil {
		return 0
	}
	// +1 and a NUL, matching the input contract above: a guest is entitled to
	// treat anything the host hands it as a C string.
	res, err := alloc.Call(ctx, uint64(len(out))+1)
	if err != nil || len(res) != 1 || res[0] == 0 || res[0] > math.MaxUint32 {
		return 0
	}
	ptr := uint32(res[0]) //nolint:gosec // bounded by the MaxUint32 check above
	if !mod.Memory().Write(ptr, out) {
		return 0
	}
	if !mod.Memory().WriteByte(ptr+uint32(len(out)), 0) { //nolint:gosec // bounded by the write above
		return 0
	}
	return uint64(ptr)<<32 | uint64(uint32(len(out))) //nolint:gosec // response length is bounded by MaxBytes
}

// httpGrantKey carries the calling tool's net.http grant into the host function.
// Passing it per call, rather than binding it into the exported function, is what
// keeps one shared import from becoming one shared permission.
type httpGrantKey struct{}

// toolNameKey carries the calling tool's name into host functions.
type toolNameKey struct{}

// Tools returns the loaded tools, sorted by name.
func (h *Host) Tools() []*Tool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Tool, 0, len(h.tools))
	for _, t := range h.tools {
		out = append(out, t)
	}
	sortToolsByName(out)
	return out
}

// Get returns the named tool, or nil.
func (h *Host) Get(name string) *Tool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tools[name]
}

// Status returns a snapshot of the last load's outcome, for `nine tools`. It
// covers both tiers: an operator asking what is loaded wants one answer, not two.
func (h *Host) Status() []Status {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]Status, 0, len(h.status)+len(h.generatedStatus)+len(h.shippedStatus))
	out = append(out, h.shippedStatus...)
	out = append(out, h.status...)
	out = append(out, h.generatedStatus...)
	return out
}

// Close releases the runtime and every compiled module.
func (h *Host) Close(ctx context.Context) error { return h.rt.Close(ctx) }

// Output is one call's result. A tool returns text or bytes, never both.
//
// Bytes exist as a separate field rather than as a string because there is
// nowhere honest to put them in one: the envelope is UTF-8 JSON, and the file
// store they end up in is a TEXT column that strips NULs
// (memory.Store.FileStore). Keeping them as []byte up to the point of encoding
// means exactly one component decides how they are represented, instead of each
// layer guessing.
type Output struct {
	// Text is the string the model reads. Empty when the tool returned bytes.
	Text string
	// Bytes is a binary result, nil for the ordinary text case.
	Bytes []byte
	// MediaType describes Bytes, if the tool said ("image/png"). Advisory.
	MediaType string

	// Continue is set when the tool asked to be called again instead of
	// producing a result. Text and Bytes are empty in that case: a continuation
	// is the tool declining to finish, not a smaller answer.
	Continue *Continuation
}

// Call runs one tool and returns its text output, for callers that have nowhere
// to put bytes — the wire protocol's direct `plugin_call`, principally. A tool
// that returned bytes is reported as an error rather than silently rendered,
// since the alternative is handing back base64 that reads like a result.
func (h *Host) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	out, err := h.CallOutput(ctx, name, args)
	if err != nil {
		return "", err
	}
	if out.Bytes != nil {
		return "", fmt.Errorf(
			"tool %q returned %d bytes of %s; this surface has nowhere to put them — call it through an agent turn, where they are written to the file store",
			name, len(out.Bytes), mediaOrBinary(out.MediaType))
	}
	return out.Text, nil
}

func mediaOrBinary(mediaType string) string {
	if mediaType == "" {
		return "binary data"
	}
	return mediaType
}

// CallOutput runs one tool against args and returns the output the model sees.
//
// The lifecycle here is the strongest property in the design: a module is
// compiled once, but *instantiated per call* and closed when the call returns.
// No state survives — not a global, not a cached credential, not a poisoned
// prototype, not a half-freed heap. Two calls to the same tool cannot observe
// each other, and a tool cannot accumulate anything across a session. That is
// true by construction rather than by review.
func (h *Host) CallOutput(ctx context.Context, name string, args json.RawMessage) (Output, error) {
	t := h.Get(name)
	if t == nil {
		return Output{}, fmt.Errorf("unknown sandboxed tool: %s", name)
	}
	out, err := h.call(ctx, t, args)
	if err == nil && t.Generated && h.cfg.TouchGenerated != nil {
		// Usage drives LRU eviction (§9.2). Best-effort and after the fact: a
		// bookkeeping failure must not fail the call the model is waiting on.
		h.cfg.TouchGenerated(name)
	}
	return out, err
}

// CallJob runs one call of a long-running job: the tool is handed the cursor it
// returned last time and may return another continuation, or a final result.
//
// It is deliberately the same execution path as an ordinary call — same instance
// model, same deadline, same memory cap, same audit. Nothing about a job call is
// special except that the host passes a cursor in and expects it may get one
// back. That is the whole design: work outlives the turn because the *host*
// keeps the state, never because anything outlives the instance.
func (h *Host) CallJob(ctx context.Context, name string, args json.RawMessage, job JobContext) (Output, error) {
	t := h.Get(name)
	if t == nil {
		return Output{}, fmt.Errorf("unknown sandboxed tool: %s", name)
	}
	if !t.Resumable {
		return Output{}, fmt.Errorf("tool %q is not resumable", name)
	}
	out, err := h.callWithJob(ctx, t, args, &job)
	if err == nil && t.Generated && h.cfg.TouchGenerated != nil {
		h.cfg.TouchGenerated(name)
	}
	return out, err
}

// call is the execution path both CallOutput and EvalGenerated go through, so an
// ephemeral evaluation cannot diverge from a catalogued tool's behavior.
func (h *Host) call(ctx context.Context, t *Tool, args json.RawMessage) (Output, error) {
	return h.callWithJob(ctx, t, args, nil)
}

func (h *Host) callWithJob(ctx context.Context, t *Tool, args json.RawMessage, job *JobContext) (Output, error) {
	name := t.Name
	timeout := t.effectiveTimeout(h.timeout)

	// A shipped fs tool accepts the workspace's host path as well as its guest
	// path, because `shell` prints the host one and models pass on what they
	// read (workspace_path.go).
	if t.Shipped && len(t.Grant.FSWrite)+len(t.Grant.FSRead) > 0 {
		h.mu.RLock()
		hostRoot := h.shippedWorkspace.Host
		h.mu.RUnlock()
		args = rewriteWorkspacePath(args, hostRoot)
	}

	// A slot first, and before the tool's own clock starts. A call that queued
	// for four seconds and then got one second to run would be timing out for a
	// reason that has nothing to do with it; the wait is bounded by the caller's
	// context — the turn's — which is the deadline that actually owns it.
	release, err := h.acquire(ctx, name)
	if err != nil {
		return Output{}, err
	}
	defer release()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ctx = context.WithValue(ctx, toolNameKey{}, name)
	// The net.http grant travels on the context so the shared host import
	// resolves to this tool's permission and no other's.
	if t.Grant.HTTP != nil {
		ctx = context.WithValue(ctx, httpGrantKey{}, t.Grant.HTTP)
	}
	// The state grant travels the same way and for the same reason: one shared
	// import, one permission per call.
	if t.Grant.State != nil {
		ctx = context.WithValue(ctx, stateGrantKey{}, t.Grant.State)
	}
	// The whole grant, for nine.caps. Read-only and descriptive: it is what the
	// guest is told, never what it is allowed.
	grant := t.Grant
	ctx = context.WithValue(ctx, grantKey{}, &grant)
	// One log buffer per call, so what a tool printed can be handed back if it
	// fails. It lives exactly as long as the call does.
	printed := &callLog{}
	ctx = context.WithValue(ctx, callLogKey{}, printed)

	input, err := t.inputForJob(args, job)
	if err != nil {
		return Output{}, err
	}

	mod, err := h.rt.InstantiateModule(ctx, t.module, h.moduleConfig(t))
	if err != nil {
		// A deadline that fires during instantiation reads as a plain
		// instantiation failure, which would send the model looking for a bug in
		// its arguments. Name the real cause.
		if ctx.Err() != nil {
			return Output{}, fmt.Errorf("tool %q timed out after %s", name, timeout)
		}
		return Output{}, fmt.Errorf("tool %q failed to start: %w", name, err)
	}
	defer mod.Close(context.WithoutCancel(ctx)) //nolint:errcheck // teardown of a discarded instance

	out, err := callGuest(ctx, mod, input, t.Kind == KindJS)
	if err != nil {
		if ctx.Err() != nil {
			return Output{}, withLogs(fmt.Errorf("tool %q timed out after %s", name, timeout), printed)
		}
		return Output{}, withLogs(fmt.Errorf("tool %q: %w", name, err), printed)
	}

	var res Result
	if err := json.Unmarshal(out, &res); err != nil {
		return Output{}, withLogs(fmt.Errorf("tool %q returned a malformed result: %w", name, err), printed)
	}
	if !res.OK {
		// The tool's own failure, surfaced as an ordinary tool error: the model
		// can read it and try different arguments, which is exactly what it
		// should do with "date is not a valid ISO-8601 string".
		msg := h.explainOOM(res.Error, len(input))
		if res.ErrorDetail != nil && res.ErrorDetail.Code == CodeWorkBudget {
			msg = t.explainWorkBudget()
		}
		return Output{}, &CallError{Tool: name, Message: msg, Detail: res.ErrorDetail, Logs: printed.taken()}
	}
	if res.Continue != nil {
		// A tool that never declared itself resumable must not be able to acquire
		// a lifecycle by returning a field. R-TVM.10 makes the manifest
		// authoritative for shape, and this is shape.
		if !t.Resumable {
			return Output{}, fmt.Errorf(
				"tool %q returned a `continue` envelope but its manifest does not say resumable = true; "+
					"add it, or return a result", name)
		}
		return Output{Continue: res.Continue}, nil
	}
	if res.OutputB64 != "" {
		raw, decErr := base64.StdEncoding.DecodeString(res.OutputB64)
		if decErr != nil {
			return Output{}, fmt.Errorf("tool %q returned output_b64 that is not valid base64", name)
		}
		return Output{Bytes: raw, MediaType: res.MediaType}, nil
	}
	return Output{Text: res.Output}, nil
}

// acquire takes one of the concurrency slots, returning the function that gives
// it back. It blocks until a slot is free or ctx ends.
//
// The uncontended path is a single non-blocking send, so the bound costs a busy
// daemon nothing until it is actually reached — and when it is reached, the wait
// is logged, because a tool that is merely queued looks exactly like a tool that
// is slow from anywhere else.
func (h *Host) acquire(ctx context.Context, name string) (func(), error) {
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, nil
	default:
	}

	slog.Debug("sandboxed tool waiting for a call slot", "tool", name, "limit", cap(h.sem))
	select {
	case h.sem <- struct{}{}:
		return func() { <-h.sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf(
			"tool %q gave up waiting for one of the %d concurrent call slots ([tools] max_concurrent): %w",
			name, cap(h.sem), ctx.Err())
	}
}

// withLogs appends what the tool printed to a host-side failure — a timeout, a
// trap, a malformed result. Those are not CallErrors, because they are Nine
// failing to run the tool rather than the tool failing, but the printed lines
// are worth just as much: a tool that timed out usually said where it got to.
func withLogs(err error, printed *callLog) error {
	logs := printed.taken()
	if len(logs) == 0 {
		return err
	}
	var b strings.Builder
	b.WriteString(err.Error())
	writeLogs(&b, logs)
	return errors.New(b.String())
}

// CodeWorkBudget is the failure code the guest reports when a call runs out of
// work budget. The guest reports a code rather than a sentence because the
// sentence needs the configured number and the key to raise, and the guest knows
// neither — it counts checks, and the operator configured operations.
const CodeWorkBudget = "E_WORK_BUDGET"

// explainWorkBudget is the sentence a model and an operator both have to act on,
// so it names the budget, the key that sets it, and the key that raises it for
// this tool alone.
//
// Matching on a code rather than on message text is the difference between this
// and explainOOM below: one is a contract the guest and host agreed on, the
// other is a guess at how QuickJS words an allocation failure.
func (t *Tool) explainWorkBudget() string {
	return fmt.Sprintf(
		"exceeded its work budget of %d operations — this is a bound on work done, "+
			"not time taken, and it is set by [tools] max_ops; raise it for this tool "+
			"alone with [tool.%s] max_ops, or make the tool do less",
		t.MaxOps, t.Name)
}

// explainOOM adds the operator-facing context a bare allocation failure lacks.
//
// A guest that exhausts its linear memory reports whatever its own runtime says —
// for QuickJS, the sentence "out of memory" — which tells an author nothing about
// *which* limit they hit or where it is configured. 1 MiB of output works and
// 8 MiB does not, and nothing in that message says where the line is.
//
// Matching on the interpreter's wording is not something to be proud of, and it
// is acceptable here for one reason: the match only ever *adds* advisory context
// to a message that is already a failure. No control flow depends on it, and a
// future QuickJS that rewords this loses the hint rather than breaking the tool.
func (h *Host) explainOOM(msg string, inputLen int) string {
	if !strings.Contains(strings.ToLower(msg), "out of memory") {
		return msg
	}
	return fmt.Sprintf(
		"%s — this call is capped at %d MiB of memory ([tools] memory_mb) and its input was %d bytes; "+
			"note the result is JSON-encoded on the way out, so a large string costs roughly twice its length",
		msg, h.memoryMB, inputLen)
}

// setWorkBudget resolves a tool's work budget and stores it in both units: the
// operator's operations, for reporting, and the guest's checks, which the cached
// envelope head carries. Every path that builds a Tool calls this, so a shipped
// or generated tool is metered exactly as a developer tool is.
func (h *Host) setWorkBudget(t *Tool) {
	ops := h.maxOps
	if over, ok := h.cfg.MaxOpsPerTool[t.Name]; ok {
		if over < 0 {
			ops = 0
		} else if over > 0 {
			ops = over
		}
	}
	t.MaxOps = ops
	t.checks = uint32(ops / opsPerCheck) //nolint:gosec // bounded by config
}

// effectiveTimeout is this tool's deadline: its own override, or the host's.
func (t *Tool) effectiveTimeout(hostDefault time.Duration) time.Duration {
	if t.Timeout > 0 {
		return t.Timeout
	}
	return hostDefault
}

// moduleConfig builds the per-call wazero configuration from a resolved grant.
// This function is the capability model: what it does not add, the guest does
// not have.
func (h *Host) moduleConfig(t *Tool) wazero.ModuleConfig {
	cfg := wazero.NewModuleConfig().
		// Anonymous, so concurrent calls to the same tool do not collide on a
		// module name. Two turns calling one tool at once is ordinary.
		WithName("").
		// A reactor module initializes through _initialize rather than _start.
		WithStartFunctions("_initialize").
		// Granted to everyone: they leak nothing and every non-trivial tool needs
		// them (docs/sandboxed-tools.md §6.2). Randomness comes from crypto/rand
		// rather than a seeded source, so a tool generating an id gets a real one.
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader).
		// Not a capability, and not a channel: a tool talks to Nine through its
		// return value and through the `log` host function, both of which are
		// accounted for. Stray writes go nowhere.
		WithStdout(io.Discard).
		WithStderr(io.Discard).
		// Args are denied wholesale. There is no argv concept in a tool call, and
		// leaving the daemon's own argv reachable would be a needless leak.
		WithArgs()

	// env is per key, never wholesale (§6.2). Config.Validate has already refused
	// the NINE_* and *_API_KEY patterns, so anything reaching here is a key an
	// operator named deliberately for this one tool.
	for _, k := range t.Grant.Env {
		if v, ok := lookupEnv(k); ok {
			cfg = cfg.WithEnv(k, v)
		}
	}

	// The filesystem is the one capability wazero enforces itself: a pre-open is
	// a real capability primitive, and a tool scoped to /srv/data cannot walk out
	// of it without our writing a single check.
	if len(t.Grant.FSRead) > 0 || len(t.Grant.FSWrite) > 0 {
		fs := wazero.NewFSConfig()
		for _, m := range t.Grant.FSRead {
			fs = fs.WithReadOnlyDirMount(m.Host, m.Guest)
		}
		for _, m := range t.Grant.FSWrite {
			fs = fs.WithDirMount(m.Host, m.Guest)
		}
		cfg = cfg.WithFSConfig(fs)
	}

	return cfg
}

// callGuest performs the ABI handshake: allocate, write, run, read. For a `js`
// tool it first installs the precompiled harness, which the QuickJS blob reads
// as bytecode instead of compiling source out of the envelope.
func callGuest(ctx context.Context, mod api.Module, input []byte, js bool) ([]byte, error) {
	alloc := mod.ExportedFunction(exportAlloc)
	run := mod.ExportedFunction(exportRun)
	if alloc == nil || run == nil {
		return nil, fmt.Errorf("module does not export the Nine ABI (%s, %s)", exportAlloc, exportRun)
	}

	if js {
		if err := installHarness(ctx, mod, alloc); err != nil {
			return nil, err
		}
	}

	// One byte more than the input, and a NUL written into it. This is part of
	// the ABI, not an implementation detail (see abi.go): QuickJS requires
	// `buf[buf_len] == 0` for both JS_Eval and JS_ParseJSON, and a guest reading
	// the input as a C string is entitled to the same guarantee.
	//
	// Omitting it does not fail loudly. It fails for roughly one input length in
	// sixteen, depending on what the guest allocator happened to leave in the
	// byte past the buffer — so a tool works or does not based on nothing but its
	// own byte length. TestGuestInputIsNULTerminatedAtEveryLength is the
	// regression.
	size := uint64(len(input))
	res, err := alloc.Call(ctx, size+1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", exportAlloc, err)
	}
	if len(res) != 1 || res[0] == 0 || res[0] > math.MaxUint32 {
		return nil, fmt.Errorf("%s returned no usable memory for %d bytes", exportAlloc, size+1)
	}
	ptr := uint32(res[0]) //nolint:gosec // bounded by the MaxUint32 check above; wasm32 offsets are u32

	if !mod.Memory().Write(ptr, input) {
		return nil, fmt.Errorf("input of %d bytes does not fit in the tool's memory", len(input))
	}
	if !mod.Memory().WriteByte(ptr+uint32(len(input)), 0) { //nolint:gosec // len(input) is bounded by the write above
		return nil, fmt.Errorf("could not terminate the input buffer")
	}

	res, err = run.Call(ctx, uint64(ptr), size)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", exportRun, err)
	}
	if len(res) != 1 {
		return nil, fmt.Errorf("%s returned no result", exportRun)
	}

	// (offset << 32) | length, per the ABI.
	outPtr := uint32(res[0] >> 32)
	outLen := uint32(res[0] & 0xffffffff)
	if outLen == 0 {
		return nil, fmt.Errorf("%s returned an empty result", exportRun)
	}
	buf, ok := mod.Memory().Read(outPtr, outLen)
	if !ok {
		return nil, fmt.Errorf("%s returned a result outside the tool's memory", exportRun)
	}
	// Copy: buf aliases guest memory, which is freed when the instance closes.
	out := make([]byte, len(buf))
	copy(out, buf)
	return out, nil
}

// installHarness writes the harness bytecode into guest memory and tells the
// guest where it is, which the QuickJS blob requires before nine_run.
//
// It is a raw write rather than a field in the envelope: the envelope is JSON,
// the harness is bytes, and base64 would have cost more than the JavaScript this
// replaced. A memcpy into linear memory costs neither an escape nor a parse.
func installHarness(ctx context.Context, mod api.Module, alloc api.Function) error {
	bc := harness()
	res, err := alloc.Call(ctx, uint64(len(bc)))
	if err != nil {
		return fmt.Errorf("%s for the harness: %w", exportAlloc, err)
	}
	if len(res) != 1 || res[0] == 0 || res[0] > math.MaxUint32 {
		return fmt.Errorf("%s returned no usable memory for the %d-byte harness", exportAlloc, len(bc))
	}
	ptr := uint32(res[0]) //nolint:gosec // bounded by the MaxUint32 check above
	if !mod.Memory().Write(ptr, bc) {
		return fmt.Errorf("the %d-byte harness does not fit in the tool's memory", len(bc))
	}

	install := mod.ExportedFunction(exportHarness)
	if install == nil {
		return fmt.Errorf("the QuickJS blob does not export %s; rebuild it with make quickjs-wasm", exportHarness)
	}
	if _, err := install.Call(ctx, uint64(ptr), uint64(len(bc))); err != nil {
		return fmt.Errorf("%s: %w", exportHarness, err)
	}
	return nil
}

// ToLLMDef converts a Tool to the llm.ToolDef the agent loop and context builder
// use, mirroring plugin.ToolDefinition.ToLLMDef. Downstream of this point a
// sandboxed tool is indistinguishable from a plugin tool, which is the whole
// point of registering behind the same dispatcher.
func (t *Tool) ToLLMDef() llm.ToolDef {
	return llm.ToolDef{
		Name:        t.Name,
		Description: t.Description,
		InputSchema: t.InputSchema,
		DisplayName: t.DisplayName,
	}
}
