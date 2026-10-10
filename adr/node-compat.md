# Node compatibility for `js` tools

- **Status:** **Proposed** (2026-10-10).
- **Scope:** the `js` tool kind in all three tiers (shipped, developer, generated);
  `internal/toolvm/stdlib`, `internal/toolvm/deps`, `internal/runtime/generated_tools.go`.
  No change to the QuickJS blob (`qjs.wasm`), the harness, the ABI, or any capability.
- **Amends:** R-TVM.15 item 4 ("Refuse Node builtins"), and the "No Node compatibility"
  position in `adr/rich-js-tools.md` §2.

**Decision.** `js` tools get Node compatibility as a *source-level* layer, not as a runtime.
Each supported `node:*` builtin is an in-house shim module served from the same host-side
module map as `nine:*`, and built only from what a tool can already reach: pure JavaScript,
or a `nine:*` module behind an existing capability. The external-dependency bundler stops
refusing builtins and inlines the same shims instead, which is what makes `require("fs")`
inside an npm package work. Builtins with no capability to stand on (`child_process`, `net`,
`worker_threads`, …) resolve to a module that throws on use. No capability is added, no
host import is added, and the blob is not rebuilt.

The measure of success is the share of a fixed npm corpus (§6, phase 4) that bundles and
passes its smoke test, not the share of Node's API that exists.

---

## 1. Motivation

`adr/rich-js-tools.md` §2 called the absence of Node "a feature": a package touching a
builtin fails to bundle, which removes much of npm before policy is consulted. Two things
have changed since.

| Then | Now |
|---|---|
| `fs` and `env` were unreachable from JS | `nine:fs` and `nine:env` exist, capability-gated |
| No timers, `URL`, `TextEncoder` | All present in the harness |
| External deps were new and rare | `[tools.agent.deps]` is the supported way to avoid reinventing libraries |

The refusal now costs more than it protects. The packages it rules out are mostly not
dangerous ones: they reach for `Buffer`, `path`, `events` or `util` to do pure computation
(`iconv-lite`, `xml2js`, `csv-parse`, `ajv`'s formatters). The dangerous builtins stay
unreachable whatever this note decides, because the reach behind them does not exist in the
sandbox — R-TVM.5 makes it structurally absent, not refused.

The cost today is felt by the model, not the operator: a generated tool that imports a
popular package fails at write time with an esbuild error naming a builtin, and the model
rewrites the library badly inside the 5-second budget.

---

## 2. Options weighed

| Option | Verdict | Reason |
|---|---|---|
| **A. Status quo** | Rejected | See §1. |
| **B. In-house shims in the module map, inlined by the bundler** | **Chosen** | No new reach, no blob rebuild, pays only for modules a tool names. |
| C. Vendor npm polyfills (`node-stdlib-browser`, `buffer`, `readable-stream`) | Rejected as a dependency; used as a porting source | They target browsers (`window`, `self`), carry transitive trees, and R-TVM.15 requires stdlib modules to be in-house and known to run under the trimmed blob. |
| D. Node API in C inside the blob (the LLRT / txiki.js approach) | Rejected | Rebuilds `qjs.wasm` (a deliberate, reviewed artifact), grows every tool's interpreter, and moves the compatibility surface into C where it is harder to test and to audit against the capability table. |
| E. A `node` tool kind running real Node | Rejected | Puts Node in the runtime image (`docs/docker-image.md`), and replaces wazero's structural confinement with an OS sandbox Nine would have to build. A native plugin already covers "I need real Node", under operator control. |

---

## 3. Design

### 3.1 Builtin policy

Every Node builtin falls in exactly one row. A builtin in no row (a future Node addition)
is refused at write time by name.

| Class | Builtins | Behaviour |
|---|---|---|
| **Pure shim** | `buffer`, `path` (POSIX only; `path.win32` throws), `events`, `util` (subset), `string_decoder`, `url`, `querystring`, `assert`, `assert/strict`, `timers`, `timers/promises`, `os`, `tty`, `perf_hooks` | Full or near-full API in plain ES2023. No capability. |
| **Capability-backed shim** | `fs`, `fs/promises`, `process` | Built on `nine:fs` / `nine:env`. Importing grants nothing; a call without the grant throws the same sentence `nine:fs` throws, with a Node `code` (`EACCES`). |
| **Phase-5 shim** | `crypto` (hashes, HMAC, `randomBytes`, `randomUUID`), `stream`, `zlib`, `http`/`https` (client only, over `net.http`) | Added only if the corpus (§6, phase 4) shows they are needed. See §3.6. |
| **Throws on use** | `child_process`, `net`, `tls`, `dgram`, `dns`, `cluster`, `worker_threads`, `vm`, `http2`, `inspector`, `module`, `v8`, `repl`, `readline`, `async_hooks`, `diagnostics_channel`, `wasi`, `sqlite`, `test` | Import succeeds; every export is a function or getter that throws `node:<name> is not available in Nine's tool sandbox`. |

**Throw on use, not on import**, because packages routinely import a builtin at top level and
use it only on a code path the tool never takes (`if (isNode) spawn(...)`, optional worker
pools). Failing the import would lose those packages for no security gain: there is nothing
behind the module to reach either way.

### 3.2 Two resolution paths, one set of files

The shims live in `internal/toolvm/stdlib/node/<name>.js` and are embedded with the rest of
`stdlib/`. They reach a tool in one of two ways.

```text
                          tool source
                               │
           ┌───────────────────┴────────────────────┐
           │ imports only nine:* / node:*           │ imports an npm package
           │ (any tier; deps off or on)             │ (generated tier, deps on)
           ▼                                        ▼
  module map (R-TVM.8)                     deps.Bundler.esbuild
  "node:path" → stdlib/node/path.js        OnResolve  ^(node:)?<builtin>$ → ns "nine-node"
  served verbatim by                       OnLoad     ns "nine-node" → same file, INLINED
  nine_module_normalize                    Inject     Buffer, process, global (only if referenced)
           │                                        │
           ▼                                        ▼
  guest imports node:path from map         one ESM module; only nine:* imports remain
```

**Module map path.** `stdlibSpecifiers` gains a `node:<name>` key per shim. The guest's
loader (`qjs_host.c`, `nine_module_normalize`) already serves any verbatim key of the
envelope's `modules` object, so no blob change is needed. `reachableStdlib` already walks
transitively, so `node:fs` pulling in `nine:fs` ships both. Developer and shipped tools get
this path too, since `load.go` and `shipped.go` use the same `reachableStdlib`.

Only the `node:`-prefixed spelling is served from the map. A bare `"fs"` would make the
substring scan in `reachableStdlib` match nearly every source, and a bare specifier in
unbundled source is refused at write time with `write "node:fs", not "fs"`.

**Bundler path.** npm packages use bare specifiers and CommonJS `require`. Marking builtins
external would leave esbuild's `__require` stub in an ESM bundle, which throws `Dynamic
require of "fs" is not supported` at call time. So the bundler **inlines** the shim instead:
an `OnResolve` for `^(node:)?(<builtin list>)(/.*)?$` maps to namespace `nine-node`, and
`OnLoad` returns the embedded file. esbuild wraps an ESM shim that is `require`d in
`__toCommonJS`, so both `import` and `require` work. The shim's own `nine:*` imports stay
external, through the existing `nine-stdlib-external` resolver.

### 3.3 Globals

| Global | Unbundled source | Bundled source |
|---|---|---|
| `Buffer` | `import { Buffer } from "node:buffer"` | esbuild `Inject`, included only if referenced |
| `process` | `import process from "node:process"` | `Inject`, as above |
| `global` | not provided | `Define: { global: "globalThis" }` |
| `require`, `module`, `exports`, `__dirname`, `__filename` | refused at write time with a hint | esbuild's CJS wrapper provides them per module |

The harness is unchanged. Installing `Buffer` and `process` as harness globals would cost
every call, including the ~1.3 ms `time` call that `adr/rich-js-tools.md` measured, for
tools that never use them. `Inject` is free for a bundle that never names them.

A source that references `Buffer`, `process` or `require` without importing them gets a
write-time hint rather than a call-time `ReferenceError`. Until W1 of
`adr/generated-tool-authoring-loop.md` (parse at write time) ships, the check is a token scan
and only adds a hint to the write result; it does not refuse. With W1 it becomes a check on
the parsed tree's unresolved identifiers.

### 3.4 Capability mapping

Each capability-backed shim is a translation layer over a module the tool could already
import. It adds no host call and no path check of its own.

| Node API | Backed by | Ungranted behaviour | Notes |
|---|---|---|---|
| `fs.readFileSync`, `readFile` (callback), `fs/promises.readFile` | `nine:fs.readFile` / `readFileText` | throws `EACCES` with the `nine:fs` sentence | Encoding argument honoured via `node:buffer`. |
| `writeFileSync`, `appendFileSync`, `mkdirSync`, `renameSync`, `unlinkSync`, `rmSync` (non-recursive), `copyFileSync` | the matching `nine:fs` call | as above | `rmSync({recursive: true})` throws: `nine:fs.remove` is deliberately non-recursive. |
| `readdirSync`, `statSync`, `existsSync` | `readDir`, `stat`, `exists` | `existsSync` returns `false`, as Node does on any error | `Stats` object carries `isFile`/`isDirectory`/`size`/`mtime`. |
| `createReadStream`, `createWriteStream` | `readRange` / `appendFile` | as above | Phase 5, with `stream`. |
| `process.env` | `nine:env.all()`, snapshotted at import | `{}` | An unset key reads `undefined`, as in Node. No reach is gained or hidden. |
| `process.cwd()` | first `fs.read` mount from `nine:fs.mounts()`, else `"/"` | `"/"` | |
| `process.nextTick` | `queueMicrotask` | — | |
| `process.stdout.write`, `stderr.write` | `console.log` / `console.error` | — | `log` is granted by default. |
| `process.exit(code)` | throws a `ProcessExit` the harness reports as the call's error | — | Code 0 with a pending result is not special-cased. |
| `process.hrtime`, `performance.now` | `Date.now()` | — | Millisecond resolution; virtual-time timers are unaffected. |

### 3.5 Bundler resolution settings

`PlatformNeutral` stays, because `PlatformNode` would mark builtins external and emit the
`__require` stub (§3.2). Two settings change:

| Setting | Today | Proposed | Why |
|---|---|---|---|
| `MainFields` | none (neutral default) | `module`, `main` | CJS-only packages with only `main` currently fail to resolve. |
| `Conditions` | none | `import`, `require`, `node`, `default` | Packages gate their non-browser entry behind `node`. `browser` stays excluded: those builds assume a DOM. |

`ExternalImports` must skip every builtin name, bare and prefixed. Without that, a tool
importing `"fs"` with deps on would make the resolver fetch the npm package named `fs`, a
squatted placeholder. The same exclusion keeps `builder.go`'s approval gate from treating a
builtin as an external dependency.

### 3.6 Phase-5 shims

| Shim | Approach | Constraint |
|---|---|---|
| `crypto` | Pure-JS SHA-1/256/512, MD5, HMAC first; `randomBytes`/`randomUUID` over the existing `crypto` global | A Go host function would be faster, but a new host import means rebuilding `qjs.wasm`. Measure `max_ops` per MB before deciding. Shares its hash code with `nine:hash` (W4 of `adr/generated-tool-authoring-loop.md`): whichever lands first, the other re-exports it. |
| `stream` | Port of `readable-stream` v4, in-house | Largest single shim (~60 KB); precompiling it to bytecode (§8) may be needed. |
| `http`/`https` | `request`/`get` over `fetch`, client only | Inherits `net.http`'s SSRF checks and `allow_hosts`. A bundle using it needs `net.http` declared, so the deps/`net.http` interlock still applies. `createServer` throws. |
| `zlib` | Port of `fflate` | Pure computation; bounded by `max_ops`. |

---

## 4. Invariants

| Invariant | How it holds |
|---|---|
| R-TVM.5: ungranted reach is structurally absent | Shims call only `nine:*` modules and harness globals. No new host import exists for them to call. |
| R-TVM.8: closed allowlist, host-side resolution | The `node:*` keys are a fixed list in `stdlibSpecifiers`. Bundled builtins are inlined at write time. |
| R-TVM.9 / I-TVM.5: no `std`/`os` | The blob is untouched. A test asserts `qjs.wasm.sha256` is unchanged by phases 1–4. |
| R-TVM.15: resolved before the call | Both paths resolve at write or load time. Nothing resolves in the guest. |
| The deps/`net.http` interlock | Unchanged. `node:http` adds no egress a tool without `net.http` could use. |
| The capability table describes everything a tool holds | Every row in §3.4 is a rename of a row the table already has. |

The one widening is in what bundles: more npm packages now resolve, so an operator running
`[tools.agent.deps] mode = "open"` admits more code than before. That code is still bounded
by the tool's declared capabilities (R-TVM.15's containment argument), and `allowlist` mode
admits only named packages as before.

---

## 5. Spec and docs changes

| File | Change |
|---|---|
| `spec/contracts/toolvm.md` R-TVM.15 | Item 4 becomes "**Inline Node builtin shims**: a supported builtin is inlined from the `node:*` set; any other builtin is refused with an actionable error." The stdlib paragraph names the `node:*` set beside `nine:*`, and "authored in-house" becomes "authored or ported in-house, with no runtime dependency". |
| `spec/contracts/toolvm.md` | New requirement (next free `R-TVM` number): the builtin policy table in §3.1 and the capability mapping in §3.4. |
| `spec/conformance.md` | Rows for the new requirement and the amended R-TVM.15. |
| `docs/sandboxed-tools.md` §4.2–4.4 | Replace "no Node standard library" with the `node:*` set, the classes, and the unbundled-source `node:` prefix rule. |
| `docs/writing-sandboxed-tools.md` | A section on Node builtins; a developer-tool bundling recipe (§8, question 3). |
| `README.md` §js | Fix the existing drift (it says there is no `setTimeout` or `URL`; both exist) and add `node:*`. |
| `adr/rich-js-tools.md` | Unchanged as a record; `adr/README.md` lists it as superseded in part. |
| `skills/` (the `tool_write` authoring skill) | Tell the model that `node:*` exists and that unbundled source must import `Buffer`/`process`. |

---

## 6. Implementation plan

Each phase is one PR. Each leaves `main` releasable, and none rebuilds `qjs.wasm`.

### Phase 1: resolution plumbing

| Change | Where |
|---|---|
| `Builtins` list and `IsBuiltin(spec)` (bare, `node:`, and subpaths) | new `internal/toolvm/deps/builtins.go` |
| `ExternalImports` skips builtins | `internal/toolvm/deps/bundle.go` |
| `node:*` keys in `stdlibSpecifiers`, served from `stdlib/node/`; first shims `path` and a generated throws-on-use module per §3.1 row | `internal/toolvm/stdlib.go`, `internal/toolvm/stdlib/node/` |
| Bundler `OnResolve`/`OnLoad` for namespace `nine-node`; `MainFields`, `Conditions`, `Inject`, `Define` | `internal/toolvm/deps/bundle.go` |
| Write-time refusals for a bare builtin in unbundled source and an unknown `node:x`; a hint for free `require`/`Buffer`/`process` (§3.3) | `internal/runtime/generated_tools.go` |

Tests:
- A generated tool importing `node:path` runs, with deps off.
- Bare `"path"` with deps off fails with the `node:` hint.
- A fixture package doing `require("path")` bundles and runs, through the existing frozen-cache bundler tests.
- `node:child_process` imports, and `spawn()` throws the sandbox sentence.
- `ExternalImports("import fs from 'fs'")` is empty.
- `qjs.wasm.sha256` is unchanged.

**Exit:** a CJS npm package that requires `path` works end to end.

### Phase 2: pure shims

`buffer`, `events`, `util`, `string_decoder`, `url`, `querystring`, `assert`, `timers`,
`timers/promises`, `os`, `tty`, `perf_hooks`.

| Concern | Approach |
|---|---|
| Correctness | Port the relevant cases from Node's `test/parallel/test-<module>-*.js` into table tests that run through the real blob (`internal/toolvm/node_compat_test.go`). |
| Size | `reachableStdlib` ships only what a tool names. Record each shim's bytes and the call-latency delta for a tool importing it, against the 7 ms baseline. |
| `util.inspect` | Subset: objects, arrays, Map/Set, errors, depth. No colours, no `showProxy`. |
| `os` | Constant stubs: `platform()` → `"linux"`, `EOL` → `"\n"`, `cpus()` → `[]`, `tmpdir()` → `"/tmp"`. |

**Exit:** each shim passes its ported cases, and no shim adds more than 1 ms to a call that imports it.

### Phase 3: capability-backed shims

`fs`, `fs/promises`, `process`, per the mapping in §3.4.

Tests:
- Every row of §3.4 in both states, granted and ungranted.
- A bypass test in the style of `rich-js-tools` M5: with no grant, the shim's underlying `nine:fs` primitive finds nothing to open. This proves the refusal is structural, not the shim's own check.
- `process.env` contains exactly the granted keys.
- `process.exit(1)` surfaces as a call error.

**Exit:** a developer tool written against `node:fs` behaves identically to the same tool written against `nine:fs`.

### Phase 4: npm corpus

Freeze about 20 packages into a test cache under `internal/toolvm/deps/testdata/corpus/`,
with each smoke test written as a generated tool. Record the results here as a table.
Candidates, chosen for being common tool-writing dependencies that touch a builtin:

| Package | Builtins touched |
|---|---|
| `js-yaml`, `papaparse`, `date-fns`, `semver`, `uuid`, `minimatch` | none or `path`; regression guard |
| `csv-parse`, `csv-stringify` | `buffer`, `stream` |
| `iconv-lite` | `buffer`, `string_decoder` |
| `xml2js`, `sax` | `events`, `stream`, `string_decoder`, `timers` |
| `marked`, `turndown` | none / DOM probe |
| `ajv`, `ajv-formats` | `url`, `util` |
| `mime-types`, `content-type` | `path` |
| `chalk` / `supports-color` | `tty`, `process`, `os` |
| `cheerio` | `buffer`, `stream`, `events` |
| `jszip` | `buffer`, `stream`, `zlib`-shaped |

Also add an eval case through the `sync-evals` skill: a `tool_write` task that is natural to
solve with `iconv-lite` or `xml2js`, run with deps on.

**Exit:** the corpus table exists. Its failures decide which phase-5 shims are built.

### Phase 5: corpus-driven shims

`stream`, then `crypto`, `zlib` and `http` in the order the corpus ranks them (§3.6). Each
is its own PR with corpus rows flipping to pass. A `crypto` host function, if measurement
calls for one, is a separate PR that rebuilds the blob under `VERSION`'s procedure.

### Phase 6: docs and spec

Apply §5 through the `sync-nine` skill and tag the version bump. Move this document to
**Implemented** in `adr/README.md`.

---

## 7. Sequencing and size

| Phase | Depends on | Rough size |
|---|---|---|
| 1 | — | ~400 lines Go, ~150 lines JS |
| 2 | 1 | ~2,500 lines JS (`buffer` ~1,200), ~800 lines tests |
| 3 | 1 | ~500 lines JS, ~400 lines tests |
| 4 | 2, 3 | fixtures plus ~300 lines tests |
| 5 | 4 | 1–4 PRs, size set by the corpus |
| 6 | 3 (may trail 5) | docs only |

Phases 2 and 3 can run in parallel once phase 1 merges.

---

## 8. Open questions

| # | Question | Recommendation |
|---|---|---|
| 1 | What should `process.platform`, `process.version` and `process.versions.node` report? | `"linux"`, and a fixed `v22.0.0` documented as the API level the shims target. Omitting `versions.node` makes packages take browser paths that assume a DOM. |
| 2 | Should `Conditions` include `node` (§3.5)? | Yes, but decide from phase-4 data: run the corpus both ways. |
| 3 | How does a developer tool get the bundler path, since developer tools bundle themselves? | Phase 1 docs an esbuild recipe (an `--alias` from bare builtins to `node:*` plus `--external:node:*`, which works for ESM but not for `require`). A `nine tools bundle <entry>` subcommand that runs the same in-process pass offline is the complete answer and a small follow-up. |
| 4 | May shims be ported from MIT code (Node's `path`/`events`, feross/`buffer`)? | Yes, with licence headers kept. Requires the R-TVM.15 wording change in §5. |
| 5 | Should large shims be precompiled to bytecode, as the harness is? | Not until phase 2 measures them. The harness precedent was 4.8 ms of a 6 ms call; a shim only costs tools that import it. |

---

## Limits

| Limit | Detail |
|---|---|
| Not a Node runtime | No event loop phases, no real I/O concurrency, no native addons (`.node`), no `worker_threads`. Code depending on any of these fails. Deliberate. |
| Timers stay virtual | `setTimeout` inside a Node package drains in deadline order after the tool settles (`adr/rich-js-tools.md` §3, decision 1). Packages that measure elapsed time see zero. |
| `fs` is the mounts only | Absolute host paths, symlinks, `chmod`, file descriptors and recursive delete are absent, as they are in `nine:fs`. |
| No server APIs | `http.createServer`, `net.listen` and similar throw. Deliberate: a sandboxed tool cannot accept connections. |
| Unbundled source needs the `node:` prefix and explicit globals | Only bundled npm code gets bare specifiers, `require` and injected `Buffer`/`process`. |
| Developer tools miss the bundler path until question 3 ships | A developer bundle containing `require("<builtin>")` fails at call time unless it is bundled with the documented recipe. |
| Per-call cost for unbundled imports | A shim imported through the module map is JSON-marshalled into every call, like every `nine:*` module. |
