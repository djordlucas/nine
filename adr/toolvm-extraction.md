# Design note — Extracting the sandbox runtime

**Status:** **Proposed** · **Depends on:** nothing · **Related:** `spec/contracts/toolvm.md`,
`adr/agent-boundary.md`, `adr/rich-js-tools.md`, `adr/durable-and-long-running-tools.md`,
`adr/capability-grants.md` · **Amends:** R-TVM.1, R-TVM.4, R-TVM.15 (see §6, §9)

The execution engine inside `internal/toolvm` — wazero, the QuickJS blob, the guest ABI,
capability grants, the `net.http` gate and SSRF guard, durable state — moves to its own
repository as a general sandbox: **the embedder gives it code, a grant and an input, and it
runs that code, with nothing beyond the grant, for as long as the embedder lets it.** It has
no concept of a tool. Tools — manifests, tiers, the registry, shipped tools, jobs, the
per-call deadline — stay in Nine, rebuilt on top of the library.

Getting there: remove the one Nine import, split the package inside Nine into an engine and a
tool layer until the compiler enforces the boundary, take every tool concept and every
default deadline out of the engine, give the ABI neutral names with aliases for existing
guests, then move the engine to the new repository with its history. §10 has the steps, §11
what is undecided.

---

## 1. Decisions

Recorded 2026-10-07; pure modules, cancellation and concurrency 2026-10-08.

| Decision | Consequence |
|----------|-------------|
| **No tools in the library.** | No manifest, `Tool`, input schema, tier, registry, `Status` reporting or shipped tools. The library's unit is a compiled program and a run of it. Everything tool-shaped is Nine's (§4). |
| **The embedder gives it any code.** | JavaScript source or a wasm module, plus the modules that code may import, all supplied by the caller. The library does no loading from disk and no dependency resolution; `deps/` and esbuild stay in Nine (§7). |
| **Code runs as long as needed.** | The library imposes no default deadline and no default work budget. A run ends when the program returns or the caller's `context.Context` ends. Nine's 5 s default and per-tool timeouts become Nine policy, applied through that context (§5). |
| **Its own repository.** | Separate repo, module and release cycle from the start; no `pkg/` or in-repo module stage. Nine depends on a tagged version (§8). |
| **Pure JS modules stay in Nine.** | `csv`, `date`, `diff`, `html` remain `nine:` modules, supplied through the module map like any embedder's. The library ships only the capability bindings (§7). |
| **Cancellation is the only control over a long run.** | No progress, heartbeat or liveness signal. A spinning run and a working run look the same to the library; the embedder decides when to stop one by ending its context (§5). |
| **The concurrency cap is a library option.** | `Options.MaxConcurrent`, a hard cap per runtime, unbounded by default. Nine sets it and may add per-workload pools on top (§5). |

## 2. Current coupling

- **One Nine import.** `internal/toolvm` imports `nine/internal/llm`, for `Tool.ToLLMDef()`
  only (`internal/toolvm/host.go:954`). `internal/toolvm/deps` imports nothing from Nine.
- **External dependencies.** wazero; BurntSushi/toml (the manifest, which stays in Nine);
  esbuild (`deps/bundle.go`, which stays in Nine). The engine needs only wazero.
- **Outward-facing seams already exist.** `Config` takes `StateStore` and `TouchGenerated`
  rather than reaching for the store; grants arrive as `map[string]Grant`, already parsed by
  `internal/config`; cancellation already works — the runtime is built with
  `WithCloseOnContextDone(true)` (`host.go:259`).
- **Embedded assets.** `qjs.wasm`, `harness.bc` and `stdlib/*.js` are `go:embed`ed in the
  package; `quickjs/build.sh` reproduces the blob.
- **Consumers in Nine.** `internal/runtime` (22 files), `internal/agent` (6),
  `internal/cli` (1), `tests/evals/runner` (1). They switch to Nine's tool layer, not to the
  library directly, except where they configure the engine.

About 10.9k lines of Go including tests (4.3k without), plus 1.2 MB of QuickJS. The files that
are tool layer only — `manifest.go`, `load.go`, `generated.go`, `shipped.go`, `calllog.go` — are
1.2k of the 4.3k; `host.go` mixes both and is where most of the split's work is.

## 3. The library

A runtime compiles programs; a program runs against an input under a grant. Sketch, not a
final API:

```go
rt, err := sandbox.New(ctx, sandbox.Options{
    MemoryMB:   64,          // per run; required finite, default below
    MaxConcurrent: 8,        // runs at once; 0 = unbounded (§5)
    StateStore: myStore,     // nil: the state capability is unavailable
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

| Concept | What it is | From today's `toolvm` |
|---------|-----------|----------------------|
| `Runtime` | Owns the wazero runtime and the compiled QuickJS blob. One per process. | `Host`, minus the registry |
| `Program` | Compiled code plus the module map it may import. Instantiated fresh per run. | `Tool.module`, `.source`, `.modules` |
| `Grant` | The capability set for one run. Nothing outside it exists for the guest. | `Grant`, `Mount`, `HTTPGrant`, `StateGrant` |
| `Result` | The guest's envelope: `ok`, `output`, `error`, `error_detail`, plus unrecognized fields (§6). | `Result` |
| Audit hooks | Per-run callbacks for HTTP calls and log lines. | `WithHTTPAudit`, the `log` host function |

The grant is passed per run, not per program, so the same code can run under different
authority. Nine's resolution — operator grant, generated-tier ceiling, shipped declaration —
happens before `Run` and is not the library's business.

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
| `StateStore` interface and quota enforcement | ✓ | the SQLite store |
| Manifest format, `LoadManifest`, `UserDir` loading, `Collides` | | ✓ |
| `Tool`, tiers (developer / generated / shipped), registry, `Status` | | ✓ |
| Grant resolution: `nine.toml`, the generated ceiling, shipped declarations | | ✓ |
| Default timeout, per-tool timeouts and work budgets; the concurrency cap's value; per-workload pools | | ✓ |
| Resumable jobs: `Continuation`, `JobContext`, `nine:job`, the driver, standing tools | | ✓ |
| Pure JS modules: `csv`, `date`, `diff`, `html` | | ✓ |
| Shipped tools (`shipped/*.js`) | | ✓ |
| npm dependency bundling (`deps/`, esbuild) | | ✓ |
| `ToLLMDef()` | | ✓ |

## 5. Bounds

| Bound | Library default | Nine |
|-------|-----------------|------|
| Wall clock | none — the caller's `context.Context` | `DefaultTimeout` (5 s) and `[tool.<name>] timeout`, as `context.WithTimeout` around `Run` |
| Work budget (QuickJS ops) | off | `[tools] max_ops`, per-tool overrides, passed as a run option |
| Linear memory | finite default (16 MiB, as today), set per runtime | `[tools] memory_mb` |
| Concurrent runs | unbounded; `Options.MaxConcurrent` caps it per runtime | `[tools] max_concurrent` (8) as the cap; optionally per-workload pools on top |

Memory keeps a finite default because wasm memory cannot be unbounded in practice and a
guest growing without limit is a host OOM, not a long run.

**The concurrency cap lives in the library** because it is a memory bound: a slot is held
from just before instantiation until just after the instance closes, so `MaxConcurrent ×
MemoryMB` is the runtime's worst-case linear memory, and only the library sees those two
points. A caller waiting for a slot gives up when its context ends. Unbounded by default,
like the deadline. Splitting slots by workload — so that long-running jobs and standing
tools cannot starve calls made during a turn — is scheduling, and stays with the embedder.

**Long runs and durability are separate.** With no deadline, one run can last hours inside
one instance. It does not survive the host process exiting. Work that must survive a restart
uses Nine's resumable-job pattern — return a cursor, persist it, run again — which needs
nothing from the library beyond passing the `continue` field through (§6) and the `state`
capability. A long run holds its memory and, if granted, its mounts for its whole duration;
bounding that is the embedder's job.

The library offers no progress or heartbeat signal. A program that wants to report progress
does it through a capability the embedder granted — a `state` key, an HTTP call, a `log`
line the embedder's audit hook sees — and the embedder cancels on whatever evidence it
chooses.

## 6. Guest ABI

Every guest-visible name carries the product name today. In a library with no Nine in it they
become neutral:

| Where | Today | Proposed |
|-------|-------|----------|
| Host import module (`abi.go:74`) | `nine` | `sandbox` |
| Guest exports (`abi.go:61-62`) | `nine_alloc`, `nine_run` | `sandbox_alloc`, `sandbox_run` |
| Harness export (`abi.go:68`) | `nine_harness` | `sandbox_harness` |
| Capability modules | `nine:fs`, `nine:env`, `nine:state`, `nine:caps` | `sandbox:fs`, … |
| Entry module (`harness.js:796`) | `nine:tool` | `sandbox:main` |

`sandbox` stands in for the final name (§11.1).

**The result envelope** keeps its shape. The library types `ok`, `output`, `output_b64`,
`media_type`, `error` and `error_detail`, and returns every other top-level field raw in
`Result.Extra map[string]json.RawMessage`. Nine's `continue` moves there: the library never
interprets it, Nine's job driver reads it from `Extra`, and existing guests produce exactly
the bytes they produce today.

**Existing guests.** Raw `wasm` developer tools import `nine` and export `nine_run`; JS tools
and generated tools in the store import `nine:*`. The library takes an alias option —

```go
sandbox.Options{ABIAliases: sandbox.Aliases{
    HostModule: "nine", Prefix: "nine:", ExportPrefix: "nine_",
}}
```

— under which it accepts the old names alongside the new. Nine sets it; nobody else needs it,
and the library's own docs mention it only as "accept a second set of names". Nine reports a
tool that resolved through an alias as deprecated in `nine tools`, and a store migration
rewrites the capability-module specifiers (`nine:fs`, `nine:env`, `nine:state`,
`nine:caps`) in generated tools. Nine's own modules (`nine:job`, `nine:csv`, `nine:date`,
`nine:diff`, `nine:html`) keep their names: they are Nine's, supplied through the module map
like any embedder's. The module map is resolved before aliases, so the `nine:` alias prefix
never shadows them.

## 7. Code and modules

The library takes code as given:

- **JavaScript**: one ES module source, whose default export is called with the input. Plus a
  `Modules` map, specifier → source, of everything it may import. Nothing outside the map
  resolves. This is today's R-TVM.8 module map, with the embedder filling it.
- **wasm**: a module implementing the two exports.

The library does not read files, fetch packages, bundle, or transpile. An embedder that wants
npm packages bundles them first; Nine keeps `deps/` and esbuild for `tool_write` and hands
the library the bundled source. The capability modules (`sandbox:fs` and so on) are always
available to JS, because they are bindings to host functions rather than library code, and a
binding without the matching grant reports the missing capability by name.

## 8. Repository and release

| Item | Plan |
|------|------|
| Location | `github.com/djordlucas/<name>` (§11.1) |
| History | Carried over: `git filter-repo --path internal/toolvm` on a clone, then delete the tool layer in the new repo's first commit. `blame` on the SSRF guard and ABI is worth keeping. |
| Versioning | `v0.x` until Nine has run on it for a release; breaking changes allowed under `v0` with a changelog entry. |
| Assets | `qjs.wasm` and `harness.bc` checked in with their `.sha256`, as today. A CI job rebuilds them with `build.sh` and fails on a mismatch. |
| CI | The engine's own tests: sandbox denial, SSRF, memory, cancellation, work budget, concurrency, state quotas, the `ABIAliases` path. |
| Nine | `require`s a tag and re-vendors. Cross-repo development through a local `go.work`, never a committed `replace`. |
| Process | A change spanning both lands as a library PR and tag, then a Nine PR bumping it. `sync-nine` and `sync-evals` learn that the engine's spec lives in the library. |

## 9. Spec split

| Library contract | Stays in `spec/contracts/toolvm.md` |
|------------------|------------------------------------|
| R-TVM.1 guest ABI (renamed, with aliases) | R-TVM.7 grants in `nine.toml` |
| R-TVM.2 two kinds | R-TVM.10, .11 loading, visibility, reporting |
| R-TVM.3 one instance per run | R-TVM.14 generated tier |
| R-TVM.4 bounds — mechanisms only; Nine's defaults stay with Nine | R-TVM.15 npm deps; Nine's own `nine:*` modules |
| R-TVM.5, .6 capability set, conferred never claimed | R-TVM.16 workspace file tools |
| R-TVM.8 imports are the module map | R-TVM.19 long-running tools |
| R-TVM.9 trimmed interpreter — no `std`/`os`, a security requirement the library keeps | R-TVM.20 standing tools |
| R-TVM.12 `net.http` | |
| R-TVM.18 durable state — the interface and quotas | |

Nine's contract cites the library's by version. `spec/conformance.md` records the pinned tag.

## 10. Plan

Each step is one Nine PR that leaves Nine green; steps 1–5 happen before the new repository
exists, so the boundary is proven by the compiler while a mistake is still cheap.

1. Move `ToLLMDef` out of `toolvm`. No behavior change.
2. Split `internal/toolvm` into `internal/toolvm/sandbox` (engine) and `internal/toolvm`
   (tool layer). `sandbox` must not import its parent; a test asserts its import list is
   wazero and the standard library.
3. Move tool concepts and Nine's modules out of `sandbox`: manifest, `Tool`, registry,
   tiers, `Status`, `Continuation`/`JobContext` (via `Result.Extra`), shipped tools,
   `deps/`. From `stdlib/`, `csv.js`, `date.js`, `diff.js`, `html.js` and `job.js` move to
   the tool layer, which embeds them and adds them to each program's module map; `fs.js`,
   `env.js` and `state.js` are capability bindings and stay. The harness's `Date` error
   text names `nine:date` (`harness.js:515`) and becomes generic.
4. Remove engine defaults for deadline, work budget and concurrency; the tool layer applies
   Nine's through `context` and options, and sets `MaxConcurrent`. Existing timeout tests move to the tool layer.
5. Neutral ABI names with `ABIAliases`; Nine sets the aliases. The module names are also
   in `harness.js` and `qjs_host.c`, so this rebuilds `harness.bc` and `qjs.wasm` with
   `build.sh`. Store migration for generated tools; deprecation note in `nine tools`.
6. Create the repository from `internal/toolvm/sandbox` with history (§8), tag `v0.1.0`.
7. Nine requires `v0.1.0`, deletes `internal/toolvm/sandbox`, re-vendors.
8. Split the spec (§9) and move the engine's docs to the library's README.

## 11. Open questions

1. **The name.** It becomes the repository, the Go package, the host import module, the
   export prefix and the module prefix. `sandbox` is a placeholder.
2. **Output type.** `output` is a string because a model reads it. A general library might
   want any JSON value. Changing it is an ABI change; keeping it costs embedders a
   `JSON.stringify`.

## Limits

| Limit | Detail |
|-------|--------|
| Nothing built | Proposed only; no code has moved. |
| API is a sketch | §3 shows shapes, not signatures. The real API comes out of step 2 of the plan. |
| No outside consumer yet | The library's API is designed from Nine's use alone until another embedder exists. |
| Aliases have no end date | `ABIAliases` stays as long as Nine has guests using old names; this note does not set a removal version. |
| Native plugins out of scope | Plugins (`spec/contracts/plugin.md`) are a separate backend and unaffected. |
