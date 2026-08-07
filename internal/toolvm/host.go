package toolvm

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
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

	wasmPageSize = 64 * 1024
)

// Config configures the host. The zero value is a disabled host, which is what
// makes this whole subsystem additive: nothing loads and nothing changes.
type Config struct {
	// UserDir holds developer tools. Empty loads none.
	UserDir string
	// Grants are the operator's `[tool.<name>]` tables, keyed by tool name.
	Grants map[string]Grant
	// Timeout and MemoryMB override the defaults above when non-zero.
	Timeout  time.Duration
	MemoryMB int

	// TouchGenerated, when set, records that a generated tool was called, for LRU
	// eviction. The daemon wires it to the store; this package has none.
	TouchGenerated func(name string)

	// AuditHTTP, when set, receives every outbound request a granted tool makes
	// (docs/sandboxed-tools.md §8 item 8). The host always logs; this is the hook
	// the daemon uses to also reach the event journal, which it can do and this
	// package cannot.
	AuditHTTP func(HTTPCall)
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
	// ManifestPath is where this tool came from, for `nine tools show`. Empty for
	// a generated tool, which came from the store rather than a file.
	ManifestPath string
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
	// imports is this tool's module allowlist: specifier -> source. Empty for a
	// developer tool, whose dependencies are already bundled into source (§4.2).
	imports map[string]string
}

// Status is the outcome of loading one candidate, for `nine tools` reporting. A
// skipped tool is surfaced here rather than silently dropped: a tool an operator
// installed and that is not running is exactly what they need told.
type Status struct {
	Name         string `json:"name"`
	ManifestPath string `json:"manifest_path"`
	Loaded       bool   `json:"loaded"`
	Generated    bool   `json:"generated,omitempty"`
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

	mu     sync.RWMutex
	tools  map[string]*Tool
	status []Status
	// generatedStatus is kept apart from status so reloading the developer-tool
	// directory does not erase the generated tier's outcomes, or vice versa.
	generatedStatus []Status
	agent           AgentConfig

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

	h := &Host{cfg: cfg, rt: rt, timeout: timeout, tools: map[string]*Tool{}}

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

// registerHostFunctions exports the "nine" module. Its entire contents are the
// `log` capability, granted to every tool because it leaks nothing and every
// non-trivial tool needs it. Anything with reach is a wazero pre-open or is
// absent; this is not the place to add one.
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
			slog.Info("sandboxed tool log", "tool", name, "msg", string(buf))
		}).
		Export("log").
		NewFunctionBuilder().
		WithFunc(h.hostHTTP).
		Export("http").
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

// writeGuest marshals resp and hands it back through the guest's own allocator,
// returning it packed the same way nine_run's result is. Calling back into
// nine_alloc is how a host function returns variable-length data without a
// second shared buffer to reason about.
func writeGuest(ctx context.Context, mod api.Module, resp httpResponse) uint64 {
	out, err := json.Marshal(resp)
	if err != nil {
		out = []byte(`{"error":"could not encode response"}`)
	}
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
	out := make([]Status, 0, len(h.status)+len(h.generatedStatus))
	out = append(out, h.status...)
	out = append(out, h.generatedStatus...)
	return out
}

// Close releases the runtime and every compiled module.
func (h *Host) Close(ctx context.Context) error { return h.rt.Close(ctx) }

// Call runs one tool against args and returns the output the model sees.
//
// The lifecycle here is the strongest property in the design: a module is
// compiled once, but *instantiated per call* and closed when the call returns.
// No state survives — not a global, not a cached credential, not a poisoned
// prototype, not a half-freed heap. Two calls to the same tool cannot observe
// each other, and a tool cannot accumulate anything across a session. That is
// true by construction rather than by review.
func (h *Host) Call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	t := h.Get(name)
	if t == nil {
		return "", fmt.Errorf("unknown sandboxed tool: %s", name)
	}
	out, err := h.call(ctx, t, args)
	if err == nil && t.Generated && h.cfg.TouchGenerated != nil {
		// Usage drives LRU eviction (§9.2). Best-effort and after the fact: a
		// bookkeeping failure must not fail the call the model is waiting on.
		h.cfg.TouchGenerated(name)
	}
	return out, err
}

// call is the execution path both Call and EvalGenerated go through, so an
// ephemeral evaluation cannot diverge from a catalogued tool's behavior.
func (h *Host) call(ctx context.Context, t *Tool, args json.RawMessage) (string, error) {
	name := t.Name

	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, toolNameKey{}, name)
	// The net.http grant travels on the context so the shared host import
	// resolves to this tool's permission and no other's.
	if t.Grant.HTTP != nil {
		ctx = context.WithValue(ctx, httpGrantKey{}, t.Grant.HTTP)
	}

	input, err := t.input(args)
	if err != nil {
		return "", err
	}

	mod, err := h.rt.InstantiateModule(ctx, t.module, h.moduleConfig(t))
	if err != nil {
		// A deadline that fires during instantiation reads as a plain
		// instantiation failure, which would send the model looking for a bug in
		// its arguments. Name the real cause.
		if ctx.Err() != nil {
			return "", fmt.Errorf("tool %q timed out after %s", name, h.timeout)
		}
		return "", fmt.Errorf("tool %q failed to start: %w", name, err)
	}
	defer mod.Close(context.WithoutCancel(ctx)) //nolint:errcheck // teardown of a discarded instance

	out, err := callGuest(ctx, mod, input)
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("tool %q timed out after %s", name, h.timeout)
		}
		return "", fmt.Errorf("tool %q: %w", name, err)
	}

	var res Result
	if err := json.Unmarshal(out, &res); err != nil {
		return "", fmt.Errorf("tool %q returned a malformed result: %w", name, err)
	}
	if !res.OK {
		// The tool's own failure, surfaced as an ordinary tool error: the model
		// can read it and try different arguments, which is exactly what it
		// should do with "date is not a valid ISO-8601 string".
		return "", fmt.Errorf("tool %q: %s", name, res.Error)
	}
	return res.Output, nil
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

// callGuest performs the ABI handshake: allocate, write, run, read.
func callGuest(ctx context.Context, mod api.Module, input []byte) ([]byte, error) {
	alloc := mod.ExportedFunction(exportAlloc)
	run := mod.ExportedFunction(exportRun)
	if alloc == nil || run == nil {
		return nil, fmt.Errorf("module does not export the Nine ABI (%s, %s)", exportAlloc, exportRun)
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
