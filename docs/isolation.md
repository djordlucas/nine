# Isolation — capability-sandboxed tools via WebAssembly

- **Status:** **Proposed / design** — nothing implemented. This note is the
  design we are working from; it is not yet a contract.
- **Date:** 2026-08-03.
- **Scope:** run *untrusted* tools inside a WebAssembly sandbox with
  capability-based, deny-by-default access to the host, so a user-supplied or
  third-party tool cannot do anything the operator did not grant. First-party
  privileged tools stay as native processes.
- **Depends on / touches:** the plugin RPC seam (`internal/plugin`,
  `spec/contracts/plugin.md`, `docs/plugins-http-transport.md`), the tool
  dispatcher (`internal/agent/dispatcher.go`), and operator policy in
  `nine.toml` (`internal/config`).

---

## 1. TL;DR — recommendation

- **Do:** introduce a `wasm` tool runtime behind the *existing* plugin contract,
  for the **untrusted tier only** (user / third-party tools). Reuse
  `describe` / `call`; the agent loop and Dispatcher do not change. Use
  **wazero** (pure Go, no CGo) as the runtime.
- **Do:** declare each tool's required host authority as **capabilities**, and
  run every tool with `requested ∩ operator-policy` — fail-closed.
- **Do (independent of WASM, do first):** stop leaking the daemon's environment
  (including the Postgres DSN) into plugins. See §2.
- **Do not:** sandbox the agent loop. It is trusted first-party code that needs
  Postgres, the LLM client, and goroutines; isolating your own code from
  yourself buys no security and costs a great deal (§3).

## 2. The problem — tools run with full ambient authority

Tools today are **external OS processes**. The daemon's `plugin.Manager` does
`exec.Command(binary)` (`internal/plugin/manager.go`, `client.go`) and speaks
JSON-RPC 2.0 over a Unix socket / stdio — a clean `describe` / `call` seam with
`ToolDefinition` → `CallRequest` → `CallResult` (`internal/plugin/contract.go`).

That seam is good. The authority model behind it is not:

- The daemon spawns plugins with `cmd.Env = append(os.Environ(), extraEnv...)`.
  **A plugin inherits the daemon's entire environment**, including
  `database_url`, the Postgres DSN. It also inherits the daemon's filesystem
  access, network reach, and every other ambient privilege of the process.
- `internal/plugin/userplugins.go` already loads **user-supplied binaries**. So
  the untrusted-code surface is not hypothetical — it exists today, and it runs
  with full authority.

**Fix the env leak regardless of whether WASM ships.** Allowlisting the
environment handed to a plugin is a self-contained hardening step and removes the
most valuable secret (the DSN) from the untrusted tier immediately.

## 3. Why WASM, and where it does *not* belong

The value of WASM here is **capability-based security**, not "wasm is
sandboxed." A WASM module under wazero can do *nothing* except call the host
functions you hand it: deny-by-default filesystem, network, and environment.
You grant a tool exactly the capabilities it declares, and nothing more. That is
a stronger and — critically — *portable* guarantee than the OS-sandbox
alternatives (seccomp / landlock / macOS `sandbox-exec`), which are per-OS and
weaker.

Three ideas were considered; they are not equally good.

- **Tools → WASM: yes, scoped.** This is the real win. But scope it to the
  untrusted tier. A shell tool, or the browser plugin (shared mutable state,
  `max_concurrent=1`, async jobs), inherently needs host power and stays a
  native process. Two tiers, **one contract**.
- **Agent loop → WASM: no.** The loop is trusted code needing Postgres, HTTP,
  and goroutines. Sandboxing it from yourself gains no security and costs
  enormously (I/O through host shims, no clean goroutine story, GC overhead).
  Isolation is for code you do not trust.
- **Validate before use: yes, but reframed.** WASM is statically verifiable in a
  way a native binary is not — you can inspect a module's *imports* and reject
  anything asking for a capability you will not grant. But validation confirms
  *what a module may reach for*; the guarantee comes from the runtime only ever
  supplying the host functions you approved. You do not prove arbitrary logic
  "safe" — you make malice **inert** by starving it of capabilities. Validation
  is a fail-closed pre-check on top of the sandbox, not the thing securing it.

## 4. The capability declaration

### 4.1 Where it attaches

The enforcement boundary is the wasm **instance**, which maps to the module
(= plugin = `DescribeResult`), not the individual tool. The instinct is
therefore "capabilities live on `DescribeResult`." There is a better option:
because the sandbox is instantiated **at call time** — host imports built fresh
per invocation, or drawn from a pool keyed by grant — a *different, narrower*
capability set can be bound **per tool** even when several tools share one
module. wazero supports different host-module bindings per instantiation, so
this is real.

Decision: **authoritative declaration on `ToolDefinition`**, with an optional
module-level default in `DescribeResult` that a tool may only **narrow**, never
widen.

### 4.2 The types (sketch)

```go
// Capabilities is a tool's declared REQUEST for host authority — not a grant.
// The daemon runs the tool with the intersection of (what it requests, what
// operator policy allows). The zero value requests nothing: a pure-compute tool
// with no filesystem, network, env, or clock. Absent on a native/MCP plugin
// (the trusted tier) it is simply ignored.
type Capabilities struct {
	FS     *FSCaps  `json:"fs,omitempty"`
	Net    *NetCaps `json:"net,omitempty"`
	Env    []string `json:"env,omitempty"`   // allowlist of env-var NAMES the tool may read
	Host   []string `json:"host,omitempty"`  // curated Nine host-function groups, e.g. "memory:read"
	Clock  bool     `json:"clock,omitempty"` // wall-clock + monotonic time; off = deterministic
	Rand   bool     `json:"rand,omitempty"`  // CSPRNG; off = no entropy source
	Limits *Limits  `json:"limits,omitempty"`
}

type FSCaps struct {
	Read  []string `json:"read,omitempty"`  // preopened dirs, read-only
	Write []string `json:"write,omitempty"` // preopened dirs, read-write
}

type NetCaps struct {
	// Outbound host:port patterns, e.g. "api.github.com:443", "*.svc.local:*".
	// Enforced by a host-provided dial function; raw WASI sockets are NEVER
	// exported to the guest, so this list is the only way out.
	Dial []string `json:"dial,omitempty"`
}

type Limits struct {
	MemoryMB       int    `json:"memory_mb,omitempty"`        // linear-memory ceiling
	Fuel           uint64 `json:"fuel,omitempty"`             // wazero instruction budget (CPU DoS guard)
	TimeoutMS      int    `json:"timeout_ms,omitempty"`       // wall-clock per call
	MaxOutputBytes int    `json:"max_output_bytes,omitempty"` // caps CallResult.Output
}
```

The additive change to the existing contract (`internal/plugin/contract.go`):

```go
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	DisplayName string          `json:"display_name,omitempty"`
	Caps        *Capabilities   `json:"caps,omitempty"` // NEW — wasm tier only; nil = requests nothing
}

type DescribeResult struct {
	// ...existing fields...
	DefaultCaps *Capabilities `json:"default_caps,omitempty"` // NEW — module baseline; a tool may only narrow it
}
```

### 4.3 Semantics — three distinct sets, fail-closed

The model hinges on keeping these separate:

1. **Requested** — what the author put in `Caps` (module default, narrowed per
   tool). Untrusted input; never enforce off it directly.
2. **Policy ceiling** — what the operator allows, from `nine.toml` /
   `internal/config`. The trust anchor.
3. **Effective** = `Requested ∩ Ceiling`. What the sandbox is actually built
   with. Requesting more than the ceiling is **not** an error — it is clamped
   (and logged). Example: a tool requesting `net.dial: ["*:*"]` under a ceiling
   of `["api.github.com:443"]` gets only GitHub.

Merge rules:

- Module `DefaultCaps` → per-tool `Caps` is **narrow-only**. A tool under a
  module default of `fs.read: ["/a","/b"]` may list `["/a"]` (allowed) but not
  add `/c` (rejected at registration — fail-closed, never silently granted).
- Absent `Caps` = zero value = nothing. A tool that forgets to declare gets a
  pure-compute sandbox and fails the instant it touches the world. That is the
  correct default.

## 5. The validation gate

Two checks at registration, before a tool is ever dispatchable:

- **Import audit.** Inspect the wasm module's import section. Any import outside
  the host modules exported for its *effective* caps → refuse the module. The
  static half — cheap, and impossible with a native binary.
- **Policy admission.** If effective caps include anything privileged
  (`fs.write`, any `net`, any `host:*` group), require explicit operator
  approval rather than auto-registering — same shape as a permission prompt.
  Pure-compute tools register silently.

## 6. Why this fits Nine cleanly

- **Additive; no `ProtocolVersion` bump.** `caps` / `default_caps` are
  `omitempty` fields on describe output. v1/v2 plugins never set them; the daemon
  reads nil as "requests nothing." The native and MCP tiers ignore them. Nothing
  existing breaks.
- **The agent loop and Dispatcher never see it.** Capabilities are consumed by
  the wasm runtime when it builds the sandbox for a call. `CallRequest` /
  `CallResult` are untouched — the runtime slots in as a new executor kind
  behind the same seam ("native process" | "MCP" | *"wasm"*).
- **Honest UI.** Effective caps yield a real, enforceable permission string for
  the TUI — "this tool wants: network → api.github.com, write → ~/.cache" — not
  a promise.

## 7. Frictions to price in

- **Toolchain tax on authors.** "Any binary" becomes "compile to `wasm32-wasi`"
  (TinyGo, Rust, …) — a narrower ecosystem than today's process plugins. The
  biggest adoption cost.
- **Runtime choice: wazero.** Pure Go, no CGo, embeds into the binary — matches
  Nine's `-mod=vendor` and "docs compiled into the binary" ethos. Avoid
  wasmtime-go (drags in CGo).
- **Stateful / long-lived tools + async jobs (protocol v2).** WASM instances are
  naturally per-call or pooled; a long-lived stateful WASM tool with `job_id`
  semantics is more design than the happy path. Pool instances for hot tools.

## 8. Open questions

- **`Host` groups in v1?** Recommendation: ship the `Host` field in the schema
  but define **no groups** initially. Filesystem / network / env cover real tool
  needs, and every host group exported to guests is trusted surface to
  threat-model. Add groups one at a time, later.
- **Pooling vs per-call instantiation.** Per-call is simplest and cleanest for
  the capability-per-tool story; pooling (keyed by effective grant) is the
  performance answer for hot tools. Decide per tool, or start per-call and add
  pooling behind the same interface.
- **Policy-ceiling shape in `nine.toml`.** Not yet drafted — the config that
  backs set #2 in §4.3. Next design step.

## 9. Staged plan

1. **Now, cheap, independent:** allowlist the plugin environment — stop leaking
   `os.Environ()` (incl. the DSN) into plugins (§2).
2. **The feature:** a `wasm` runtime behind the existing plugin contract,
   wazero-backed, capability-gated, untrusted tier only. Reuse `describe` /
   `call`; Dispatcher and agent loop unchanged.
3. **The gate:** import-validation + policy admission at registration (§5).
4. **Never:** the agent loop (§3).
