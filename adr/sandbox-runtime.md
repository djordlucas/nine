# Design note — A standalone sandbox runtime

**Status:** **Proposed** · **Depends on:** nothing · **Related:** `spec/contracts/toolvm.md`,
`adr/agent-boundary.md`, `adr/rich-js-tools.md`, `adr/durable-and-long-running-tools.md`,
`adr/capability-grants.md` · **Amends:** R-TVM.1, R-TVM.4, R-TVM.15 (see §6, §9)

A new library, in its own repository, runs untrusted code in a sandbox: **the embedder gives it
code, a grant and an input, and it runs that code, with nothing beyond the grant, for as long as
the embedder lets it.** It has no concept of a tool. It is built fresh on Nine's stack — wazero,
QuickJS, esbuild — with a general API from the first commit, and the security-critical parts of
`internal/toolvm` are ported into it with their tests rather than rewritten. Nine keeps
`internal/toolvm` running until a tool layer rebuilt on the library passes Nine's existing tool
tests, then switches and deletes the old engine.

§10 has the steps; §11 what is undecided.

---

## 1. Decisions

Recorded 2026-10-07 and 2026-10-08.

| Decision | Consequence |
|----------|-------------|
| **Built fresh, same stack.** | New repository, new API, wazero + QuickJS + esbuild. Not an extraction: no history carried over and no in-Nine split first. The QuickJS build, the `net.http` gate and SSRF guard, the capability bindings and their denial tests are ported, not rewritten (§8). |
| **No tools in the library.** | No manifest, `Tool`, input schema, tier, registry or shipped tools. The unit is a compiled program and a run of it. Everything tool-shaped is Nine's (§4). |
| **The embedder gives it any code.** | JavaScript (or TypeScript, §7) source or a wasm module, plus the modules that code may import, all supplied by the caller. No loading from disk, no package fetching. |
| **Code runs as long as needed.** | No default deadline, no default work budget. A run ends when the program returns or the caller's `context.Context` ends. Nine's 5 s default and per-tool timeouts are Nine policy, applied through that context (§5). |
| **Cancellation is the only control over a long run.** | No progress, heartbeat or liveness signal. The embedder decides when to stop a run by ending its context (§5). |
| **The concurrency cap is a library option.** | `Options.MaxConcurrent`, a hard cap per runtime, unbounded by default. Nine sets it and may add per-workload pools on top (§5). |
| **`output` is any JSON value.** | String, number, boolean, object, array or null. Turning a value into text a model reads is Nine's tool layer's job (§6). |
| **Pure JS modules stay in Nine.** | `csv`, `date`, `diff`, `html` remain `nine:` modules, supplied through the module map like any embedder's. The library ships only the capability bindings (§7). |
| **No Nine names, no aliases.** | The library's ABI and modules use its own name only. Nine's compatibility for existing JS guests is a set of `nine:*` modules in Nine's module map (§6). |

## 2. Why fresh rather than extracted

Extracting `internal/toolvm` is mostly removal: stripping tool vocabulary, deadlines and Nine's
names out of a package shaped around tools, then carrying aliases for the old names. Built
fresh, the general API is the design rather than what is left after removal, and the library
never contains the word `nine`.

The cost is the security work in the current package, which a rewrite could silently lose.
Those parts are ported with their tests:

| Ported | Lines today | Why not rewritten |
|--------|-------------|-------------------|
| `quickjs/build.sh`, `qjs_host.c`, the trimmed blob | 1.0k (C) | R-TVM.9 — no `std`/`os` — is a security requirement asserted by test. The interrupt hook behind the work budget lives here. |
| `ssrf.go`, `nethttp.go` | 0.7k | Two independent gates and a rejection checklist; one missed case is an SSRF. |
| fs, env, state bindings (`stdlib/fs.js`, `env.js`, `state.js`, the host functions) | — | Each maps a capability to WASI pre-opens or a host function; a binding that reaches past its grant is the failure the model exists to prevent. |
| The denial and boundary tests | part of the 6.5k test lines | They are how the new library shows it is as tight as the old one. |

The harness (`harness.js`, 0.8k) is rewritten: most of it is tool-shaped (rendering, the
continuation marker, the `nine:tool` entry), and the parts that are not — the platform layer,
error detail — are short.

## 3. The library

A runtime compiles programs; a program runs against an input under a grant. Sketch, not a
final API:

```go
rt, err := sandbox.New(ctx, sandbox.Options{
    MemoryMB:      64,     // per run; finite, default 16
    MaxConcurrent: 8,      // runs at once; 0 = unbounded (§5)
    StateStore:    store,  // nil: the state capability is unavailable
})
defer rt.Close(ctx)

prog, err := rt.CompileJS(src, sandbox.Modules{"lib:util": utilSrc})
// or: rt.CompileWasm(wasmBytes)

res, err := prog.Run(ctx, input, sandbox.Grant{
    FS:    []sandbox.Mount{{Host: dir, Guest: "/work", Write: true}},
    HTTP:  &sandbox.HTTPGrant{Allow: []string{"api.example.com"}},
    Env:   map[string]string{"TOKEN": tok},
    State: &sandbox.StateGrant{Scope: "job-42", Quota: q},
})
// err: the host could not run it (cancelled, out of memory, trapped)
// res.OK == false: the program failed on its own terms (threw, returned an error)
```

| Concept | What it is |
|---------|-----------|
| `Runtime` | Owns the wazero runtime and the compiled QuickJS blob. One per process. |
| `Program` | Compiled code plus the module map it may import. Instantiated fresh per run. |
| `Grant` | The capability set for one run. Nothing outside it exists for the guest. |
| `Result` | The guest's envelope: `ok`, `output` (any JSON value, as `json.RawMessage`), `output_b64`, `media_type`, `error`, `error_detail`, and every other top-level field raw in `Extra` (§6). |
| Audit hooks | Per-run callbacks for HTTP calls and log lines. |

The grant is passed per run, not per program, so the same code can run under different
authority. Nine's grant resolution — operator grant, generated-tier ceiling, shipped
declaration — happens before `Run`.

## 4. Library and Nine responsibilities

The library owns *how code runs and what it can reach*. Nine owns *what the code is, where it
came from, who approved it, how long it may run, and where its state lives*.

| Part | Library | Nine |
|------|:---:|:---:|
| wazero runtime, QuickJS blob, harness, `build.sh` | ✓ | |
| Guest ABI and result envelope | ✓ | |
| Capabilities: fs mounts, env, `net.http` gate, SSRF guard, state, log | ✓ | |
| Memory cap; optional deadline and work budget per run; optional concurrency cap per runtime | ✓ | |
| Capability-binding JS modules (`fs`, `env`, `state`, `caps`) | ✓ | |
| TypeScript and module-map bundling at compile time (§7, proposed) | ✓ | |
| `StateStore` interface and quota enforcement | ✓ | the SQLite store |
| Manifest format, loading from `tools.d`, name collisions | | ✓ |
| `Tool`, tiers (developer / generated / shipped), registry, `nine tools` reporting | | ✓ |
| Grant resolution: `nine.toml`, the generated ceiling, shipped declarations | | ✓ |
| Default timeout, per-tool timeouts and work budgets; the concurrency cap's value; per-workload pools | | ✓ |
| Resumable jobs: the `continue` field, `nine:job`, the driver, standing tools | | ✓ |
| Pure JS modules: `csv`, `date`, `diff`, `html` | | ✓ |
| `nine:*` compatibility modules (§6) | | ✓ |
| Shipped tools | | ✓ |
| npm resolution, the verified package cache, `[tools.agent.deps]` policy | | ✓ |
| Rendering output for the model, `llm.ToolDef` | | ✓ |

## 5. Bounds

| Bound | Library default | Nine |
|-------|-----------------|------|
| Wall clock | none — the caller's `context.Context` | `DefaultTimeout` (5 s) and `[tool.<name>] timeout`, as `context.WithTimeout` around `Run` |
| Work budget (QuickJS ops) | off | `[tools] max_ops`, per-tool overrides, passed as a run option |
| Linear memory | 16 MiB, set per runtime | `[tools] memory_mb` |
| Concurrent runs | unbounded; `Options.MaxConcurrent` caps it per runtime | `[tools] max_concurrent` (8) as the cap; optionally per-workload pools on top |

Memory keeps a finite default because a guest growing without limit is a host OOM, not a long
run.

**The concurrency cap lives in the library** because it is a memory bound: a slot is held from
just before instantiation until just after the instance closes, so `MaxConcurrent × MemoryMB`
is the runtime's worst-case linear memory, and only the library sees those two points. A caller
waiting for a slot gives up when its context ends. Splitting slots by workload — so that
long-running jobs and standing tools cannot starve calls made during a turn — is scheduling,
and stays with the embedder.

**Long runs and durability are separate.** With no deadline, one run can last hours inside one
instance. It does not survive the host process exiting. Work that must survive a restart uses
Nine's resumable-job pattern — return a cursor, persist it, run again — which needs nothing
from the library beyond passing the `continue` field through (§6) and the `state` capability.
A long run holds its memory and, if granted, its mounts for its whole duration.

The library offers no progress or heartbeat signal. A program reports progress through a
capability the embedder granted — a `state` key, an HTTP call, a `log` line the audit hook
sees — and the embedder cancels on whatever evidence it chooses.

## 6. Guest ABI

| Where | Nine today | Library |
|-------|-----------|---------|
| Host import module | `nine` | `sandbox` |
| Guest exports | `nine_alloc`, `nine_run` | `sandbox_alloc`, `sandbox_run` |
| Harness export | `nine_harness` | `sandbox_harness` |
| Capability modules | `nine:fs`, `nine:env`, `nine:state`, `nine:caps` | `sandbox:fs`, … |
| Entry module | `nine:tool` | `sandbox:main` |
| Internal globals and symbols | `__nine_*`, `Symbol.for("nine.*")` | `__sandbox_*`, `Symbol.for("sandbox.*")` |

`sandbox` stands in for the final name (§11.1). The two-export shape, the one-instance-per-run
model and the JSON-in, JSON-out envelope stay as R-TVM.1 and R-TVM.3 define them.

**`output` is any JSON value.** The harness emits the returned value itself (`undefined` →
`null`), with cycles and `BigInt` handled when encoding the envelope. Nine's tool layer renders
for the model with today's rule — string as-is, `null` → `""`, anything else compact JSON — so
model-visible text is unchanged. Bytes stay `output_b64`: JSON has no byte type, and base64 in
`output` would be indistinguishable from text.

**Extra fields.** Every top-level envelope field the library does not type is returned raw in
`Result.Extra`. A JS program sets them by returning an object marked with
`Symbol.for("sandbox.envelope")`: its fields are merged into the envelope's top level (`ok`,
`output`, `error` excepted) instead of becoming `output`. Nine's `continue` travels this way:
`nine:job`'s `again()` returns a marked object with a `continue` field, and Nine's job driver
reads it from `Extra`.

**Nine's existing guests.** No aliases in the library. Compatibility is Nine's:

| Guest | What keeps it working |
|-------|----------------------|
| JS developer tools and generated tools importing `nine:fs`, `nine:env`, `nine:state`, `nine:caps` | Nine's module map adds each as a one-line re-export of the `sandbox:` module. No source change, no store migration. |
| JS tools importing `nine:job`, `nine:csv`, `nine:date`, `nine:diff`, `nine:html` | Nine's own modules, in Nine's module map. |
| Raw `wasm` tools exporting `nine_run` and importing `nine` | Not supported; they rebuild against the new names. The only one in the repository is the test fixture `internal/toolvm/testdata/upper.wasm`. |

## 7. Code and modules

- **JavaScript**: one ES module source whose default export is called with the input, plus a
  `Modules` map, specifier → source, of everything it may import. Nothing outside the map
  resolves.
- **wasm**: a module implementing the two exports.

The capability modules (`sandbox:fs` and so on) are always importable, because they are
bindings to host functions; a binding without the matching grant reports the missing
capability by name.

**esbuild, proposed.** The library runs esbuild once at compile time, offline, over the source
and the module map: TypeScript is stripped, relative imports between map entries are bundled
into one module, and capability modules stay external. No resolution outside the map, no
network, no `node_modules` — so the library's esbuild pass cannot fetch anything. npm
resolution stays in Nine: Nine fetches and verifies packages, puts them in the module map, and
the library bundles what it is given. Undecided — §11.2.

## 8. Repository and release

| Item | Plan |
|------|------|
| Location | `github.com/djordlucas/<name>` (§11.1) |
| Start | Empty repository. Ported files (§2) arrive in one commit that names their source commit in Nine, so their history stays findable in Nine's log. |
| Versioning | `v0.x` until Nine has run on it for a release; breaking changes allowed under `v0` with a changelog entry. |
| Assets | `qjs.wasm` and `harness.bc` checked in with their `.sha256`. A CI job rebuilds them with `build.sh` and fails on a mismatch. |
| Conformance | A test suite derived from the requirements that move (§9): every capability denied without its grant, SSRF rejections, the trimmed interpreter, memory, cancellation, work budget, concurrency, state quotas. |
| Nine | `require`s a tag and vendors it. Cross-repo development through a local `go.work`, never a committed `replace`. |
| Process | A change spanning both lands as a library PR and tag, then a Nine PR bumping it. `sync-nine` and `sync-evals` learn that the engine's spec lives in the library. |

## 9. Spec split

| Library contract | Stays in `spec/contracts/toolvm.md` |
|------------------|------------------------------------|
| R-TVM.1 guest ABI (library names; `output` any JSON value) | R-TVM.7 grants in `nine.toml` |
| R-TVM.2 two kinds | R-TVM.10, .11 loading, visibility, reporting |
| R-TVM.3 one instance per run | R-TVM.14 generated tier |
| R-TVM.4 bounds — mechanisms only; Nine's defaults stay with Nine | R-TVM.15 npm deps; Nine's `nine:*` modules |
| R-TVM.5, .6 capability set, conferred never claimed | R-TVM.16 shipped tools |
| R-TVM.8 imports are the module map | R-TVM.19 long-running tools |
| R-TVM.9 trimmed interpreter — no `std`/`os` | R-TVM.20 standing tools |
| R-TVM.12 `net.http` | |
| R-TVM.18 durable state — the interface and quotas | |

Nine's contract cites the library's by version. `spec/conformance.md` records the pinned tag.

## 10. Plan

Steps 1–4 are in the new repository and change nothing in Nine. Steps 5–7 are Nine PRs that
each leave Nine green.

1. **Skeleton.** Repository, `Runtime`/`Program`/`Grant`/`Result`, wasm programs only, no
   capabilities. Conformance tests for the ABI, memory, cancellation and concurrency.
2. **QuickJS.** Port `build.sh` and `qjs_host.c` with the renames, rebuild the blob, write the
   new harness. Port the trimmed-interpreter tests and the work budget.
3. **Capabilities.** Port the fs, env and state bindings, the `net.http` gate and the SSRF
   guard, each with its denial tests.
4. **esbuild** (if §11.2 is accepted). Compile-time TypeScript and bundling over the module
   map. Tag `v0.1.0`.
5. **Nine's tool layer on the library.** A new package beside `internal/toolvm`, behind a
   config switch, holding manifests, tiers, grant resolution, timeouts, rendering, `nine:job`,
   the pure modules and the `nine:*` compatibility modules. Nine's existing toolvm tests run
   against both engines.
6. **Switch.** Make the new layer the default once both engines pass the same tests; rebuild
   `testdata/upper.wasm` against the new names.
7. **Delete** `internal/toolvm`'s engine, move npm resolution next to the new layer, split the
   spec (§9).

## 11. Open questions

1. **The name.** It becomes the repository, the Go package, the host import module, the
   export prefix and the module prefix. `sandbox` is a placeholder.
2. **esbuild in the library.** §7 proposes compile-time TypeScript and module-map bundling,
   offline. The alternative keeps esbuild entirely in Nine and the library takes one finished
   module. The proposal follows from listing esbuild in the stack; it is not yet confirmed.

## Limits

| Limit | Detail |
|-------|--------|
| Nothing built | Proposed only. |
| API is a sketch | §3 shows shapes, not signatures. |
| No outside consumer yet | The API is designed from Nine's use alone until another embedder exists. |
| Two engines during the transition | Between steps 5 and 7, Nine carries both engines and runs its tool tests twice. |
| Old raw-wasm guests break | Deliberate: a raw `wasm` tool built against `nine_run` must be rebuilt. No alias is planned. |
| Native plugins out of scope | Plugins (`spec/contracts/plugin.md`) are a separate backend and unaffected. |
