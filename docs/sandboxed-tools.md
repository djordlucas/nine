# Sandboxed tools

Nine executes tools as wasm modules in-process, each with the capability set the
operator conferred and nothing else. This document is the design rationale; the
normative contract is `spec/contracts/toolvm.md` (`R-TVM.*`) and the authoring
guide is `writing-sandboxed-tools.md`.

**Three tiers write tools; one runtime executes them.** **Shipped** tools are
compiled into the binary (§5.4), **developer** tools are a file and a manifest the
operator installs (§5.1), and **generated** tools are rows Nine writes itself
(§5.2). All three share the ABI, the instance model, the capability model, and the
audit trail.

- **Motivation:** three capabilities that otherwise have no home. (1) A
  **developer** wants to add a permanent tool without writing a Go plugin, building
  a binary, and rebuilding the image. (2) **Nine** wants to write a tool for a job
  it does not have a tool for — the gap its `gap_report` already names but cannot
  close. (3) Nine's **own** first-party capabilities ran as plugins, which are
  subprocesses holding the daemon's uid, rather than under a capability model
  (§5.4).
- **Additive.** The native plugin system (`spec/contracts/plugin.md`) is untouched:
  same transport, same `plugin.ProtocolVersion`, same lifecycle. Sandboxed tools
  are a *second* backend behind the *same* dispatcher, and a deployment that
  enables none of it behaves exactly as it does today.
- **Depends on:** the tool dispatcher (`spec/contracts/dispatcher.md`), the memory
  store (generated tools are rows), the event journal (`event-journal.md`, audit),
  HITL approval gates (`hitl.md`, optional review), and the pull-not-push
  discipline of `event-journal.md` (how a new tool becomes visible).

---

## 1. Summary

| # | Piece | Shape |
|---|---|---|
| 1 | **Wasm host** (§3) | `toolvm`, built on **wazero** (pure Go, no CGO). One wasm instance **per call**, torn down after. Registers handlers on the existing `Dispatcher` exactly like `RegisterPlugin` does. |
| 2 | **JS is a guest, not the host** (§4) | QuickJS-NG compiled to wasm is *one pre-supplied guest module*. A developer may equally ship a raw `.wasm` built from Rust/TinyGo/Zig. Same ABI, same capability model. |
| 3 | **Three authors, three trust tiers** (§5) | **Shipped tools** are compiled into the binary and granted what they declare. **Developer tools** are files on disk with a manifest, installed by the operator. **Generated tools** are rows in SQLite, authored by Nine. Different ceilings, one runtime. |
| 4 | **Capabilities are conferred, never claimed** (§6–§8) | Default is the empty set: no filesystem, no network, no env, no clock. Every capability is an explicitly-exported host function or a wazero pre-open. A manifest *declares a need*; only operator config *grants*. |
| 5 | **Dependencies never resolve at call time** (§4.2) | Developers **pre-bundle** their deps at dev time; generated tools get a curated, vendored `nine:*` stdlib. Module resolution is always host-side against a closed allowlist. |
| 6 | **External packages: opt-in and allowlisted** (§4.4) | Off by default. When enabled, an operator **names the permitted packages** (transitive deps included); Nine resolves, integrity-checks, and bundles them **in-process at write time** via esbuild — never in the sandbox, never at call time. No install scripts ever run. `deps` + `net.http` is refused by default. |
| 7 | **Visibility is next-turn** (§9) | A new tool reaches every agent loop, including the session that wrote it, at that loop's next turn. The loop pulls the change at its turn boundary; there is no push machinery. |

**Do not take `github.com/fastschema/qjs`** (§10). Use wazero directly.

---

## 2. What this is not

This does not let Nine modify Nine. There is no path here by which the agent
edits Go source, rebuilds the binary, alters `nine.toml`, writes a native plugin,
or changes a built-in skill. Every one of those stays removed
(`self-modification.md`). Nine's executable shape remains fixed.

What it does allow is narrower. `R-PLUG.7` states the boundary:

> **R-PLUG.7:** No agent-reachable tool or path may write plugin source, build a
> plugin, start a new plugin binary, hot-swap, or roll back a plugin. This is a
> self-modification boundary: **Nine cannot grant itself capabilities.**

The final sentence is the invariant, and it is preserved verbatim. The clause
about *writing code* is the one a generated tool changes. The distinction:

| | Code | Capabilities |
|---|---|---|
| **Native plugin** | operator (build time) | operator (`nine.toml`) |
| **Shipped sandboxed tool** | Nine's maintainers (build time) | the tool's own declaration (§5.4) |
| **Developer sandboxed tool** | developer (file on disk) | operator (`nine.toml`) |
| **Generated sandboxed tool** | **Nine** (runtime) | operator (`nine.toml`) |

Nine gains one column and never the other. **The agent writes the code; the
operator writes the grants; these are never the same actor.** A generated tool
that could grant itself filesystem access would be a shell with extra steps —
that is the failure mode this whole design exists to prevent.

The old rationale — "runtime code generation makes the running system drift from
its source" — was aimed at *native* code: a rebuilt binary genuinely does drift,
and cannot be reasoned about from the repo. A generated tool does not drift the
binary. It is **store state**, exactly like a goal, a workflow, or an agent skill:
listable, readable, exportable, deletable, and journalled. `nine tools` shows
you the whole set, and `nine tools show <name>` its source, grant and
provenance. The property that `docs/` and the binary match its version is
untouched.

---

## 3. The host: wazero, one instance per call

`toolvm` owns a wazero runtime, a compilation cache, and a registry.
Compilation happens once per module; **instantiation happens per call**, and the
instance is closed when the call returns.

That sounds wasteful and is not. Tool calls are gated behind LLM turns — they are
already ≥100ms events, and Nine makes at most a handful per turn. Against that,
instantiating a cached module is noise. In exchange, per-call instantiation buys
the strongest property in the design:

**No state survives a call implicitly.** Not a global, not a cached credential,
not a poisoned prototype, not a half-freed heap. Two calls to the same tool cannot
observe each other *through the machine*, and a tool accumulates nothing across a
session except through a capability it was granted (§6.4). A long-lived shared
runtime would need all of that reasoned about; a fresh instance makes it true by
construction.

It also removes the entire class of use-after-free bugs that a long-lived
Go↔JS value bridge carries (§10).

```text
Dispatch(tool, args)
  └ toolvm handler
      ├ compile (cached)            wazero.CompileModule    — once per module
      ├ resolve grants              §7                      — per call
      ├ instantiate                 ModuleConfig + host fns — per call
      ├ call  run(argsJSON) → out   ctx with deadline       — per call
      └ close                       everything reclaimed
```

**Resource bounds** (orthogonal to capabilities, always on):

| Bound | Mechanism | Default |
|---|---|---|
| Wall clock | `WithCloseOnContextDone(true)` + context deadline | 5s (`[tools] timeout`, overridable per tool) |
| Memory | `WithMemoryLimitPages`, per call | 256 pages (16 MiB) (`[tools] memory_mb`) |
| Concurrency | a host semaphore held across instantiation | 8 calls (`[tools] max_concurrent`) |
| Output | the dispatcher's existing cap + spill (R-DISP.2) | 2048 tokens |
| Work | QuickJS interrupt handler, `js` tools only | 50M operations (`[tools] max_ops`, overridable per tool) |

The wall clock and the work budget are the two an operator may name per tool,
with `[tool.<name>] timeout` and `[tool.<name>] max_ops`. Without the override a
single global value forces the most permissive tool's requirement onto every other
tool: raising the deadline for one tool that legitimately takes twenty seconds
hands twenty seconds to a tool that is merely stuck. Both are resource bounds
rather than capabilities, so they sit outside `[capabilities]` and confer nothing.
Memory has no per-tool form.

Memory and concurrency are one bound in two halves: `memory_mb` is what a single
call may hold, `max_concurrent` is how many calls may hold it at once, and their
product is the host's worst case — 128 MiB at the defaults. A call that arrives
with every slot taken waits, and the wait is charged to the turn's context rather
than to the tool's own deadline, so queueing never shortens the time a tool gets
to run.

**The work budget bounds what a call does; the deadline bounds how long it
takes.** The difference is that a deadline is a property of the machine — the
same tool passes on an idle host and fails on a loaded one — while a budget is a
property of the tool, so a tool that is too expensive fails the same way
everywhere.

It is a real bound, not a suggestion. QuickJS throws an **uncatchable** error
when the budget runs out, so a tool cannot wrap its loop in `try`/`catch` and
carry on; the unwinder skips every handler and an `async` function propagates
instead of rejecting. Exhaustion is sticky, so nothing resumes after the throw.

The unit is approximate and deliberately so: QuickJS polls its interrupt handler
once per 10,000 backward jumps and calls, so "operations" means loop iterations
and function calls rather than bytecode ops. At any budget worth setting, the
±10,000 granularity is irrelevant.

**A `wasm` tool is not metered.** The budget is the interpreter's interrupt
handler, and a raw module has no interpreter to interrupt — wazero itself offers
no fuel metering. For those tools the wall clock remains the only bound, which is
a stated limitation rather than an assumption.

---

## 4. JS is a guest, not the host

The host is a **wasm host**. It knows nothing about JavaScript. A tool is a wasm
module exporting `run`, and there are two ways to get one:

- **`wasm` tools** — a developer ships a `.wasm` built from Rust, TinyGo, Zig, or
  C. Full speed, any language, no interpreter in the middle.
- **`js` tools** — the module is the *pre-supplied QuickJS-NG interpreter*, and
  the tool's source is JavaScript handed to it. Nothing is compiled at install
  time.

An LLM writes correct JavaScript far better than it writes Rust that compiles
to wasm, and **there is no build step**, so the runtime image needs no toolchain
(`self-modification.md`).

Both kinds are the same to everything downstream: same ABI, same capability
model, same dispatcher registration, same audit trail. QuickJS is an
implementation detail of one tool *kind*, not an architectural layer.

**The ABI is deliberately tiny**, because the tool contract it has to satisfy
already is:

```go
CallRequest{Tool string, Args json.RawMessage} → CallResult{Output string}
```

JSON in, string out. So the guest exports exactly:

```text
run(argsPtr, argsLen) → (resultPtr, resultLen)   // JSON in, JSON out
alloc(n) → ptr                                    // host writes args here
```

For a JS tool the harness is a few lines wrapping the author's default export.
No struct marshalling, no proxy objects, no reference counting across the
boundary. Everything crossing the boundary is a UTF-8 JSON byte slice.

**ABI versioning.** This introduces `toolvm.ABIVersion`, independent of
`plugin.ProtocolVersion` (which this design does not touch — see
`versioning.md`). A module declaring an unsupported ABI is refused at load.

### 4.1 The interpreter surface must be trimmed

QuickJS-NG ships two built-in modules that are **separate, opt-in init calls** —
`js_init_module_std` and `js_init_module_os` — and the stock `qjs` CLI links both.
At v0.16.1 they expose:

| Module | Exposes |
|---|---|
| `os` | `open` `close` `read` `write` `remove` `rename` `mkdir` `readdir` `stat` `lstat` `realpath` `symlink` `readlink` `chdir` `getcwd` **`exec`** `waitpid` `kill` `pipe` `dup` `dup2` `getpid` … |
| `std` | **`getenv`** **`getenviron`** **`urlGet`** `loadFile` `writeFile` `popen` `open` **`evalScript`** **`loadScript`** `exit` … |

A filesystem API, a process API, a network fetch, and two arbitrary-eval hooks —
in scope by default, before any capability has been granted.

**The blob links neither.** Only `bjson` (and a trimmed `std` for
`printf`/`strerror` if the harness needs it) is initialised. This is a `build.sh`
concern (§10.1) and it is a hard requirement, not a hardening nicety.

wazero's denials are the backstop, not the control: under WASI `os.exec` has no
`proc_spawn` to call and `std.urlGet` has no socket, so both fail. But
`os.readdir` and `os.open` map onto `fd_readdir`/`path_open`, which work fine
against **any pre-open we granted** — so a tool granted `fs.read` on `/srv/data`
silently gets a whole second, undeclared file API over it. Defence in depth is
real here, but a capability table that says `fs.read` while the guest also holds
`std.loadFile` and `os.stat` is a table that lies.

**This is a rule about `std`/`os`, not about a filesystem existing at all.** The
blob does expose a narrow, capability-gated `nine:fs` and `nine:env`, added once
it was clear that `fs` and `env` were grantable and unreachable from the kind of
tool most people write (`nine docs rich-js-tools`). Two things keep that
consistent with the rule above. They are built on ordinary libc calls in
`qjs_host.c`, which route through WASI to exactly the pre-opens and env pairs the
host configured — so a tool with no grant sees an empty filesystem, and
confinement stays wazero's rather than becoming a path check of ours. And they
carry none of what made `std`/`os` unacceptable: no `exec`, no `urlGet`, no
`evalScript`. The objection was never "a tool can read a file it was granted"; it
was "a tool holds an API the capability table does not describe."

This is also the sharpest argument for building the blob ourselves (§10.1): the
prebuilt QuickJS wasm modules are built as **`qjs` CLI replacements**, so they
link `std` and `os` by design. Grabbing one and shipping it would import exactly
this surface, invisibly.

### 4.2 Dependencies: bundled by developers, curated for the agent

QuickJS is ES2023 and nothing else. There is no npm, no `require`, no Node
standard library — no `fs`, `http`, `path`, `Buffer`, `process`, or `crypto`. Any
package touching a Node builtin simply fails, which rules out a large share of
npm before policy even enters the picture. Pure-ESM, zero-dependency packages
work fine.

Nine therefore **never resolves a dependency**. It has no package manager, no
lockfile, no registry client, and no network at load time — and adding any of
those to the runtime image would undo `self-modification.md` exactly as a
compiler would. Instead, the split follows the two authors again:

| | Developer tool | Generated tool |
|---|---|---|
| Dependencies | **pre-bundled**, one file | curated `nine:*`, plus external packages **if the operator enables them** (§4.4) |
| Resolved by | the developer's bundler, at dev time | nobody, or Nine's in-process resolver at *write* time — never at call time |
| Reviewed by | the developer, in their own repo | the operator, via policy + a lockfile |
| Arbitrary `import` at **call** time | no | no |

**Developer tools bundle at development time.** `esbuild --bundle --format=esm`
(or rollup) collapses the tool and its dependencies into the single `.js` file the
manifest points at. Nine loads a file; it does not know a dependency ever existed.
This is the right place for the risk to live — the developer already has a
`package.json`, a lockfile, `npm audit`, and a review process, and the bundle
lands in their repo as a reviewable artifact. Nine inherits none of that
machinery and none of that responsibility.

**Generated tools get a curated standard library and no imports.** A small,
pinned, vendored set under a `nine:` namespace — the same treatment as the
QuickJS blob itself, rebuilt only on a deliberate bump:

```js
import { parse, format } from "nine:csv";
import { parseDate, isoWeek } from "nine:date";
export default ({ csv }) => ({ rows: parse(csv).length });
```

The set is deliberately boring, and each module it *cannot* import is a wheel a
tool-writing agent reinvents badly inside a 5-second deadline:

| Module | Contents | Gated by a capability |
|---|---|---|
| `nine:csv` | parse and format delimited text | no |
| `nine:date` | date arithmetic and ISO-8601 formatting | no |
| `nine:diff` | a textual diff | no |
| `nine:html` | text extraction from a page — a tokenizer, not a DOM | no |
| `nine:fs` | ranged read, write, append, rename, remove, mkdir | `fs.read` / `fs.write` |
| `nine:env` | the granted environment keys | `env` |
| `nine:state` | the host-owned store that outlives a call (§6.4) | `state` |
| `nine:job` | ending a call with a cursor (§6.5) | `resumable` in the manifest |
| `nine:process` | `next()`, `turn()` and `report()`, for a tool running as a live process | refused unless the tool was started as a live process |

Importing one of the gated modules grants nothing: a tool with no grant gets a
sentence saying so rather than reach.

### 4.3 Imports are a capability

The rule that makes the above enforceable:

> **Module resolution happens in the host, against a closed allowlist, before
> instantiation. The guest never receives a resolver that can touch disk or
> network.**

QuickJS lets the embedder own the module loader callback, so this is a matter of
supplying one that consults a map rather than a filesystem. Without it, `import`
is a capability-model bypass hiding in plain sight: a generated tool granted
nothing at all could `import` a *developer* tool's bundle and execute it. It
would not inherit that tool's grants — capabilities are resolved per tool, per
call, by the host — but it would be reading and running code the operator
approved for a different purpose, and an fs-reading resolver is an ungranted
`fs.read` by another name.

So: no relative imports, no absolute paths, no URLs, no dynamic `import()` of
anything not in the allowlist. For a developer tool the allowlist is empty — the
bundle is already whole. For a generated tool it is the `nine:*` set, plus
whatever §4.4's policy admits.

The rule is unconditional and survives §4.4 intact, because **external packages
are resolved at write time and bundled away**. By the time a tool is callable it
has no imports left at all.

**Bytecode.** The harness is precompiled to QuickJS bytecode at build time and
read back with `JS_ReadObject`, because compiling it *was* the call. Handing the
guest 32 KB of harness JavaScript inside the envelope — escaped into JSON by the
host, JSON-parsed by the guest, then compiled — cost 4.8 ms of a 6 ms call, on
every call, to produce the same program each time. Executing the harness costs
0.14 ms by comparison. A `time` call went from 7.1 ms to 1.3 ms.

The bytecode does not travel in the envelope: the envelope is JSON and bytecode
is bytes, so base64 would have cost more than the source it replaced. The host
writes it into guest memory and names it through a `nine_harness` export the blob
provides for the purpose.

The `nine:*` modules are **not** precompiled. They are ordinary source, and a
call ships only the ones its tool can name (§4.2), which is cheaper than
precompiling all eight would be.

### 4.4 External dependencies — opt-in, allowlisted, resolved at write time

A tool-writing agent that cannot use a library is a tool-writing agent that
rewrites CSV parsing badly, every time. Three uses are worth enabling: **testing
a library** to see how it behaves, **iterating** on a tool against a couple of
candidate libraries, and **shipping** a generated tool that leans on a known-good
one.

This is **off by default** and is the single riskiest switch in the design. npm is
the canonical software supply chain attack surface, and "the agent may import
anything" is arbitrary remote code execution wearing a bow tie. What follows is
the set of properties that make it defensible when an operator turns it on.

#### The pipeline: resolution happens once, in the daemon, at write time

The decisive structural choice is **when** a dependency is resolved. Not at call
time, in the sandbox — at `tool_write` time, in the daemon, once.

```text
tool_write(source with `import { chunk } from "lodash-es"`)
  1. extract imports                esbuild resolver callback
  2. POLICY CHECK                   §4.4 allowlist — refuse here, loudly
  3. resolve versions               registry metadata (packument)
  4. fetch tarballs                 the DAEMON's network — never the guest's
  5. verify integrity               sha512 from dist.integrity, per package
  6. extract                        read files out of the tar; NO install scripts
  7. bundle                         esbuild, in-process, custom OnResolve/OnLoad
  8. freeze                         store bundle + lockfile + hash; journal it
                                    ─────────────────────────────
  at call time:  ONE self-contained ESM file. Zero imports. Zero network.
```

Step 7 is why this works. **esbuild is a pure Go library** (MIT, 40k stars,
v0.28.1) with `OnResolve`/`OnLoad` plugin hooks and in-memory input, so Nine
bundles in-process — no Node, no npm binary, nothing added to the runtime image.
And because *we* supply the resolver, esbuild resolves nothing on its own: every
import goes through a Go callback that serves only from the verified cache and
refuses everything else. §4.3's rule is not weakened by this feature; it is
implemented by it.

**A generated tool with dependencies becomes a developer tool, mechanically.**
Same artifact — one
self-contained ESM file with no imports. The only difference is who ran the
bundler and when. Everything downstream (§3 execution, §6 capabilities, §9 audit)
is unchanged and does not need to know.

#### The allowlist

The operator names what may be imported. There is no "import whatever."

```toml
[tools.agent.deps]
mode     = "allowlist"        # "off" (default) | "allowlist" | "open"
registry = "https://registry.npmjs.org"   # or an internal mirror/proxy

# Exact packages, with a semver range the operator is willing to stand behind.
allow = [
  { name = "lodash-es",   version = "^4.17.21" },
  { name = "date-fns",    version = "^4.1.0"   },
  { name = "papaparse",   version = "^5.4.1"   },
  { name = "js-yaml",     version = "^4.1.0"   },
]

# Budgets — a transitive tree is how a small allowlist becomes a large one.
max_packages    = 24          # including transitive
max_bundle_kb   = 2048
max_depth       = 4
frozen          = false       # true = lockfile only, no new resolution
```

- **`mode = "off"`** is the default and the shipped posture. `nine:*` only.
- **`mode = "allowlist"`** is the recommended enabled posture and the answer to
  "a known and trusted external library." **Transitive dependencies are checked
  against the same allowlist** — otherwise a four-package allowlist smuggles in
  four hundred, and the allowlist is decoration. A transitive dep outside the
  list fails the write with a named error.
- **`mode = "open"`** admits anything from the registry within the budgets. It
  exists for the *iteration* and *library-testing* use cases, and it should be
  understood as a **development posture**: an instance where the agent is
  exploring, not one serving a production workload. Pairing it with `frozen =
  true` afterwards is the intended path — iterate open, then freeze the lockfile
  and stop resolving.

#### Why this is containable

Four properties do the real work, and they compound:

1. **No install scripts, ever.** npm's dominant attack vector is the
   `preinstall`/`postinstall` lifecycle hook. Nine never runs npm — it reads
   files out of a tarball. There is no install step to hijack, so the single
   most-exploited path into a build machine **structurally does not exist** here.
2. **A malicious package is bounded by the tool's capabilities**, and a tool that
   declares none has none (§6.1, §5.2). This is the containment argument and the
   reason the feature is defensible at all: a compromised dependency inside a
   zero-capability sandbox can return a wrong answer, burn its 5 seconds, and
   nothing else — no filesystem, no network, no environment. **The blast radius
   of "arbitrary npm" is exactly the blast radius the operator already granted.**

   Read §7.1 alongside this. The shipped ceiling permits workspace `fs.read`, so
   a tool that *declares* it hands its dependencies a view of the workspace. The
   containment still holds — reading without egress is a bounded harm, and the
   §4.4 interlock is what keeps egress off the table — but "can only return a
   wrong answer" is true of a tool that declared nothing, not of every tool.
3. **Node builtins fail at bundle time**, with a clear error. `Platform:
   PlatformNeutral` means a package importing `fs`, `http`, `child_process`, or
   `crypto` cannot bundle. This silently filters a large share of npm — including
   most of the packages whose compromise would matter — and it hands the agent an
   actionable message ("this library needs Node APIs; pick another") rather than
   a mystery.
4. **Everything is pinned, hashed, and journalled.** Each resolution records
   name, version, resolved integrity, and the requesting tool into a lockfile and
   the event journal (`event-journal.md`). `nine tools deps` lists the whole
   set; `nine tools show <name>` prints a tool's lockfile. "What third-party code
   is in this daemon, and who asked for it" has an exact answer.

#### The interlock that actually matters

> **`deps` + `net.http` on the same tool is refused unless the operator sets
> `allow_network_deps = true`.**

Property 2 is what makes external dependencies safe, and network egress
dissolves it. A package that can reach the network can exfiltrate
whatever the tool sees — its arguments, its filesystem grants, anything in scope
— and the sandbox is no longer a boundary, just a delay. The two features are
individually reasonable and jointly a data-exfiltration primitive, so the
combination is a config error by default rather than an emergent surprise.

The same logic applies, less sharply, to `fs.write`. Both are called out in the
`nine tools show` output so an operator reviewing a tool sees the combination
rather than having to infer it.

#### Caching, offline, and reproducibility

Resolved packages live in a content-addressed cache under `[tools].cache_dir`,
keyed by integrity hash, shared across tools. A second tool wanting the same
`lodash-es@4.17.21` resolves from cache with no network. With `frozen = true` the
resolver refuses anything not already in the lockfile, which makes an
air-gapped or reproducible deployment straightforward: iterate on one instance,
copy the lockfile and cache, freeze in production.

#### The ephemeral case

"Have Nine test a library" often does not want a persisted tool at all — it wants
one execution and a discard. That falls out for free: a generated tool is a row,
so a scratch tool can be written, called once, and deleted within a turn. It is
worth surfacing in the `tool_write` guidance, because the alternative is an agent
that accretes single-use tools into the catalog and quietly degrades its own tool
ranking (§9.2).

---

## 5. Three authors, three trust tiers

### 5.1 Developer tools — permanent, operator-installed

Modelled on user plugins (`R-PLUG.9`), whose ergonomics are already right:

```text
$NINE_TOOLS_USER_DIR/
  csvstats.toml          # manifest — the gate
  csvstats.js
  imageresize.toml
  imageresize.wasm
```

```toml
# csvstats.toml
name       = "csv_stats"
kind       = "js"              # "js" | "wasm"
entrypoint = "./csvstats.js"
description = "Summary statistics over a CSV string."
input_schema = "./csvstats.schema.json"

# What the tool NEEDS. Not what it gets — see §7.
[capabilities]
fs  = ["read"]
net = ["http"]
```

The manifest is a **gate**, exactly as for user plugins: a `.js` or `.wasm` file
with no manifest beside it is never loaded. It carries `input_schema` because —
unlike a native plugin — there is no process to ask `plugin.describe`; the
manifest is authoritative for a sandboxed tool.

Loading is the R-PLUG.9 sequence: deterministic name order, malformed manifest
skipped without executing anything, **tool-name collisions skip the whole tool
("no override, ever")** against built-ins, native plugins, *and* generated tools,
and any single failure is logged and surfaced by `nine tools` without aborting
the rest.

### 5.2 Generated tools — authored by Nine

A new core-intercepted tool, `tool_write`, in the shape of `skill_write`:

```json
{
  "name": "iso_week_of",
  "description": "Return the ISO-8601 week number for a date string.",
  "input_schema": { "type": "object", "properties": { "date": {"type":"string"} } },
  "capabilities": {},
  "source": "export default ({date}) => ({ week: isoWeek(new Date(date)) });"
}
```

The row lands in a `tools` table with `source = agent`, mirroring how skills
already split built-in from agent-authored (`self-modification.md`). Kind is
always `js` — the agent cannot supply a `.wasm` blob, because a binary blob is
not reviewable and there is no reason to accept one.

**A generated tool declares what it needs, exactly as a developer tool does**
(§6.3). The declaration is checked against the operator's ceiling (§7) and is
what the approval gate keys on (§9.4). A tool that declares nothing — the common
case — gets nothing, regardless of how permissive the ceiling is. Least privilege
is per tool, not per tier.

**What `tool_write` answers.** The result names when the tool can be called,
because a model that cannot tell waits or writes it again. A name new to the
loop is "callable from your next turn"; a tool the loop already carries is
callable in this turn, and a rewrite of it takes effect on the next call, since
the handler resolves the source by name at call time.

Two writes are refused rather than stored, both because the refusal is the
useful answer (§7):

- **A write that changes nothing.** Byte-identical source, description, schema
  and declaration means nothing was written, and reporting success is what lets a
  model rewrite the same source turn after turn. The refusal says the tool exists
  and to call it instead.
- **An `input_schema` that is not a JSON object.** It becomes the tool's
  `parameters` in every request that advertises the tool, and a provider that
  rejects the malformed field fails the whole turn — one bad tool would break
  every turn of every session that loads it. A row stored before this check is
  skipped at load.

**Deleting one.** Nine deletes its own tools with `tool_delete`; the operator
deletes them with `nine tools delete <name>`, `DELETE /api/v1/tools/{name}` or
`/tools delete <name>` in the TUI. Both go through the same store and refuse the
same tools: a shipped or operator-installed tool cannot be deleted this way, and
the refusal names which it is. Deleting a tool also deletes its standing run.

The symmetry with skills is deliberate:

| | Skill | Generated tool |
|---|---|---|
| Written by | `skill_write` | `tool_write` |
| Stored in | `skills` table | `tools` table |
| Built-ins immutable | yes | yes |
| Effect | model *reads* it | daemon *executes* it, sandboxed |
| Capabilities | n/a | declared per tool, bounded by an operator ceiling |

The last two rows are the whole difference, and they are why a generated tool
needs a capability model where a skill needs none.

### 5.3 Ephemeral execution — `js_eval`

Two of the three motivating uses for external libraries (§4.4) — *testing* a
library and *iterating* on a tool against candidates — do not want a persisted
tool at all. They want one execution and a discard.

`js_eval` is that: run a JS snippet in the sandbox, return its output, persist
nothing. No name, no schema, no row, no catalog entry.

```json
{ "source": "import { chunk } from 'lodash-es'; export default () => chunk([1,2,3,4], 2);" }
```

It is not a third trust tier. It runs under **exactly** the generated-tool rules
— same sandbox (§3), same ceiling (§7), same dependency policy (§4.4), same
audit (§9.3) — and it is strictly *less* persistent, since nothing survives the
call. The reason it earns a separate surface is §9.2: without it, every
experiment transits the tool catalog, and single-use tools accreting into the
catalog is precisely what degrades tool ranking for *everything else*. `js_eval`
keeps iteration out of the namespace.

The intended arc: `js_eval` to explore and get it working, `tool_write` once to
keep it. That also gives the agent a cheap way to *verify* a tool before
committing it, which is the difference between a catalog of working tools and a
catalog of plausible ones.

Resolved dependencies are cached (§4.4), so iterating on the same library across
several `js_eval` calls hits the network once.

### 5.4 Shipped tools — first-party, in the binary

Nine's own capabilities are sandboxed tools whose source is compiled into the
daemon binary: the workspace file tools (`read_file`, `write_file`, `edit_file`,
`move_file`, `copy_file`, `delete_file`, `diff_file`, `restore_file`,
`trash_list`), the fetching tools (`http_get`, `http_post`, `web_page_read`,
`web_search`), and `time`. They run through the same host, ABI, instance model and
bounds as every other tool.

They were plugins, and a plugin is a subprocess holding the daemon's uid — so a
tool that read a clock had, in principle, the reach to read the operator's home
directory. Under this tier `time` declares nothing and therefore has nothing.

**A shipped tool is granted what it declares**, which is the one way the tier
differs from the other two. It is a reduction rather than a new trust: the
operator already ran this code as a plugin with strictly more authority. The grant
still appears in `nine tools`, and the host refuses a declaration it cannot
enforce rather than registering a tool whose capability silently does nothing.

| Property | Detail |
|---|---|
| Load order | **First**, before developer and generated tools. The namespace rule is first-registered-wins, so loading them last would let another tier take a first-party name. |
| `fs` mount | The operator's `[workspace] root`, at the fixed guest path `/work`. A tool declaring `fs` with no workspace configured fails to load; the daemon creates the root if it is absent. |
| Host paths | The host maps a `[workspace] root`-relative or absolute host path in a `path` argument onto the mount, because a model that copies a path out of `shell` output otherwise names a file the guest has no name for. |
| Deletion | Recoverable: a removed or replaced file moves under `.nine/trash/`, listed by `trash_list` and restored by `restore_file`, bounded by both age and total size. Identical content is not trashed on overwrite. `.nine/` is refused to the write tools. |
| Review | The write tools take `preview`, which returns the diff and writes nothing; the same preview renders inside an approval prompt (`hitl.md`). |
| No ambient host state | `time` reports **UTC**, where the plugin it replaced reported the daemon's local zone. The guest has no timezone database, and a tool cannot know the host's zone unless the host confers it. |

---

## 6. The capability model

### 6.1 The default is empty

wazero denies everything unless configured: no filesystem (`path_open` →
`ENOSYS`), **no network APIs at all**, stdio discarded, env denied, args denied.
The design does not have to build a sandbox — it has to *decide what to poke
holes in*, one hole at a time.

So every capability is one of exactly two things:

1. A **wazero pre-open** — the filesystem, where wazero itself enforces the scope.
2. A **host function we export** — everything else. If we do not export it, it
   does not exist in the guest.

Capabilities that are not in the table below are not "denied". They are
**structurally absent**: there is no host function to call, so there is nothing
to bypass. A sandboxed tool cannot spawn a process, open a socket, load a native
library, or reach another tool, because none of those verbs exist inside a wasm
module and none are exported to it.

### 6.2 The capability set

| Capability | Grant parameters | Default | Enforced by |
|---|---|---|---|
| `fs.read` | list of host paths → guest paths | **none** | wazero `WithReadOnlyDirMount` |
| `fs.write` | list of host paths → guest paths | **none** | wazero `WithDirMount` |
| `net.http` | host allowlist, methods, max bytes | **none** | host fn (§8) |
| `env` | explicit key allowlist | **none** | `WithEnv`, per key |
| `clock` | — | **granted** | `WithSysWalltime` |
| `random` | — | **granted** | `WithRandSource` |
| `log` | — | **granted** | host fn → `slog` + journal |
| `state` | scope (required), quotas, ttl | **none** | host fn → the `tool_state` store (§6.4) |

`clock`, `random`, and `log` are on by default because they leak nothing and
every non-trivial tool needs them. Everything with reach — the filesystem, the
network, the process environment — starts at nothing.

`fs.write` includes creating a directory and its missing parents, recursively and
idempotently. Without it a granted tool could write `a.txt` and not `notes/a.txt`,
with no other way to make the directory — and confinement stays the pre-open's,
since the guest has nothing but its mount to resolve a path against.

A `net.http` request is bounded at four fifths of the time the call has left, so
a slow remote host surfaces as an HTTP timeout the tool can catch and report
rather than as the whole call being killed under it.

`env` deserves its explicit-allowlist treatment rather than an all-or-nothing
flag: the daemon's environment holds LLM provider API keys. A tool granted "env"
wholesale is a credential exfiltration primitive. Grants are per key, and the
`NINE_*` and `*_API_KEY` patterns are refused outright as a config error.

### 6.4 Durable state

An instance is destroyed when its call returns, so nothing in the interpreter survives —
which is the strongest property here and is not being given up. What `state` adds is a
store the *host* owns: keys scoped to one tool, bounded by a quota, conferred by the
operator like any other capability.

The invariant is therefore narrowed rather than dropped. It used to read "no state
survives a call"; it now reads **"no state survives a call *implicitly*"**. The guest's
globals, heap and interpreter realm still go, so no carryover happens by accident; what
persists does so because a tool asked, an operator granted, and the roster shows it.

**Scope is the decision that matters.** `scope = "tool"` shares one namespace across every
caller, which is what a cache wants and is also a channel from one conversation into
another that needs no other capability: a tool's arguments come from the model, and a
call in one session can write them down for a call in another to read.
`scope = "conversation"` keys the namespace per conversation and closes that. Neither is
wrong; which one applies is why the parameter is required and has no default.

The full reasoning, including what the amended invariant gives up and what it keeps, is
`adr/durable-and-long-running-tools.md` §2.

### 6.5 Long-running work

A call runs to completion under a deadline, which made the tool tier strictly
request/response. A tool that declares `resumable = true` may instead do a bounded slice
and hand back a cursor, and the host calls it again — as many times as it takes, across
turns and across restarts.

The instance model is untouched, and that is the point. Each call is created and destroyed
exactly as before, under the same deadline and the same memory cap. Work outlives the turn
because the *host* holds the cursor, never because anything outlives the instance. The
alternatives — keeping an instance alive, or detaching one onto a goroutine — were rejected
for the same reason: the deadline and the work budget are both per call, and detaching
from a call leaves neither.

It runs as a **job**, in the registry long-running plugin work already uses, and an agent
sees no difference: `job_check`, `job_wait`, `job_list`, `job_cancel`, unchanged. Two
things a tool job does that a plugin job cannot, both consequences of the state being a row
rather than a process: it resumes after a restart, and cancelling is exact rather than a
polite request.

Nine writing *itself* something that runs for an hour is a separate decision from Nine
writing itself a date formatter, so the generated tier is gated by
`[tools.agent] allow_long_running`, off by default. The capability ceiling cannot stand in
for that: it bounds what a tool may *reach*, and duration is not reach.

The design is `adr/durable-and-long-running-tools.md` §4.

### 6.6 Standing tools

The same resumable tool can also be run **standing**: indefinitely, on its own cadence,
declared in `nine.toml` rather than started by a turn. It is a second run mode, not a
second kind of tool.

The gap it fills is narrow. Recurring work in Nine has always
gone through a standing agent, a goal session, or a session-plan routine — and all three
put an LLM turn in the loop, so watching a file every ten seconds costs thousands of turns
a day to be told nothing happened. For work with no judgement in it, that is the wrong
tier. The alternatives outside Nine are worse: a native plugin is a process with the
daemon's uid, a cron script has the user's, and neither is visible to `nine tools` or
subject to the capability model.

What it deliberately does not do is decide anything. A standing tool has no model, cannot
delegate, and cannot address an agent — it leaves a note on the human feed, and only when
it has something to say. Work that needs judgement still belongs to a standing agent, which
can call the tool.

The design, including the two shapes rejected and why, is `adr/standing-tools.md`.

**Processes Nine writes.** With `[tools.agent] allow_processes`, `tool_write` takes a
`process` block (`every` or `schedule`, `args`, `role`, `budget`, `report_to`) and the
tool runs as a process from the moment it is written ([processes.md](processes.md)). A
tool that is not resumable is a **live** program that loops on `next()` and asks the model
with `turn()`; a resumable one is a slice process, a cycle per tick, as above. Its turns
run under a role from `process_roles` — by default only `process`, which has no tools. It
pipes only into another process Nine wrote, and its tool is never evicted from the
catalog while the process exists.

### 6.3 Conferred, never claimed

**A manifest declares a need. Only config grants.** These are different documents
written by different people, and the runtime uses only the second.

```text
manifest [capabilities]   — what the tool needs      (developer, in the repo)
nine.toml [tool.<name>]   — what the tool gets       (operator, on the host)
effective = granted, and granted only
```

If a tool declares a capability the operator has not granted, it **fails to load
with a named error** rather than starting up crippled. Silent degradation would
mean a tool that half-works in ways neither the developer nor the operator
predicted; a loud failure at load is the only honest outcome, and `nine tools`
reports it.

The manifest's declaration is therefore not a security control — it is
documentation and a pre-flight check. **A manifest that lies gains nothing**,
because nothing reads it at call time.

---

## 7. Grants in `nine.toml`

Following the existing plural-subsystem / singular-instance split (`[plugins]` vs
`[plugin.<name>]`, R-PLUG.10):

```toml
[tools]
enabled   = true
user_dir  = "/etc/nine/tools.d"     # developer tools; unset disables
timeout   = "5s"
memory_mb = 16

# ── A developer tool: named grants, reviewed by the operator ──────────────
[tool.csv_stats]
timeout = "20s"       # this tool only; a resource bound, not a capability (§3)
max_ops = 200000000   # this tool only; negative turns the budget off

[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]

[tool.csv_stats.capabilities.net.http]
allow_hosts = ["api.example.com"]     # exact, "*.example.com", or a bare "*"
methods     = ["GET"]
max_bytes   = 1048576

[tool.geocode.capabilities.state]      # §6.4
scope        = "conversation"          # required: "tool" or "conversation"
max_keys     = 128
max_value_kb = 64
ttl          = "24h"                   # omit for no expiry

# ── The ceiling for everything Nine writes itself ─────────────────────────
[tools.agent]
enabled  = true
eval     = true       # allow js_eval (§5.3)
max_tools = 64        # catalog pressure — §9

allow_long_running = false   # may a generated tool run as a job? — §6.5
allow_processes    = false   # may Nine write processes (tool_write's process block)? — §6.6
process_roles      = ["process"]   # the roles their turns may run under
# how many run at once is [processes] max_running, shared by every process
allow_network_deps = false   # lift the deps + net.http interlock — §4.4

# Gate on substance, not on every write (§9.4):
#   "on_capability" | "always" | "never"
require_approval = "on_capability"

# The MAXIMUM a generated tool may be granted — not an automatic grant.
# A tool that declares nothing still gets nothing. Omit the fs table and the
# ceiling defaults to [workspace].root, read and write, at /work; an fs grant
# here replaces that default rather than adding to it.
[tools.agent.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]
```

Four properties:

- **`[tools.agent.capabilities]` is a ceiling, not a default.** A generated tool
  cannot request its way past it, and does not receive it merely by existing. It
  must declare `fs = ["read"]` (§5.2) to get workspace read; a tool that declares
  nothing runs with nothing. The ceiling bounds what is *grantable*, §5.2's
  declaration decides what is *granted*, and the two are deliberately separate so
  that widening the ceiling does not retroactively widen every existing tool.
- **Refusal is a usable signal.** If Nine writes a tool reaching for `net.http`
  and the ceiling excludes it, `tool_write` **fails with a message the model can
  read** — "capability net.http is not available to generated tools" — so the
  agent rewrites without it or calls `capability_request` to ask an operator. That
  failure path is a feature, and `capability_request` is what makes it a path
  rather than a dead end: see §7.2.
- **Grants are per named tool.** There is no wildcard `[tool."*"]`. An operator
  granting filesystem access to a *developer* tool does so to a tool they have
  read.
- **Read at boot, reconciled into the store.** The generated tier's ceiling lives
  in the database, and `nine.toml` is written into it at every boot — the file's
  grants as `config` rows, the workspace fallback as `default` rows, an operator's
  approvals as `approved` rows that a boot leaves alone. `nine grants` shows all
  three with their source. Developer-tool grants (`[tool.<name>]`) are still read
  at boot only: `nine tools reload` re-scans `user_dir` and re-reads manifests
  against the grants already held, so it picks up a new or edited *tool*, and a
  changed `[tool.<name>]` grant needs a restart.

### 7.1 On the default workspace ceiling

The default ceiling is the workspace, `fs.read` **and** `fs.write`, derived from
`[workspace].root` at boot rather than written in the file — a deliberate choice
for usefulness, with one consequence worth naming precisely.

It grants the *agent* no new reach. The shipped file tools already read and write
the workspace, and `shell` runs in it, so nothing becomes reachable to Nine that
was not already. What changes is the reach of **a tool's dependencies** (§4.4).
With an empty ceiling, a hostile npm package can only return a wrong answer. With
the workspace ceiling it can *see* the workspace, and — this is what the write
grant adds — *modify* it: a tool the agent called to reformat one file could, via a
compromised dependency, rewrite another.

Neither can leave the machine, because of the §4.4 interlock, which is the only
thing standing between a compromised transitive dependency and your source tree.
Three things follow, none optional:

1. `allow_network_deps` stays off. Turning it on with this ceiling is the one
   combination that makes a supply-chain compromise materially dangerous.
2. `deps.mode = "allowlist"` is strongly preferred over `"open"` on any instance
   whose workspace holds anything you would not publish.
3. The workspace is a working directory, not a source of truth. `.nine/trash/`
   retains overwritten files (`docs/operations.md`), but a workspace holding the
   only copy of something is a workspace one bad tool ruins.

An operator whose workspace holds secrets — `.env`, private keys, credentials —
should grant a narrower mount explicitly, which replaces the derived default
rather than adding to it.

---

### 7.2 Requesting a capability the ceiling excludes

A refusal ends in `capability_request`: the agent names the capability, why it needs
it, and the narrowest scope that would work. That records a pending request and posts
it to `nine notifications`. Nothing is granted.

An operator decides, from whichever surface they are already in:

```
nine grants                     # requests waiting, and the ceiling in force
nine grants approve <id>        # confer it
nine grants deny <id>
nine grants revoke <grant-id>   # withdraw one you approved earlier
```

`/grants` in the TUI takes the same verbs, and the API has `GET /capabilities` and
`POST /capabilities/{id}/decision`. All three reach one service in the daemon.

**An approval applies to the running daemon.** The ceiling is recomputed, installed
on the live host, and every stored generated tool is re-resolved against it — so the
tool that could not load becomes callable on the next turn, with no restart. A
revocation does the same in reverse: a tool that no longer fits stops loading.

The ceiling is stored, and `nine.toml` is reconciled into it at every boot:

| `source` | Comes from | Survives a boot |
|---|---|---|
| `default` | `[workspace].root`, derived | rewritten each boot |
| `config` | `[tools.agent.capabilities]` | rewritten each boot |
| `approved` | an operator's decision | yes, until revoked |

The file therefore stays authoritative for what it declares while an approval
outlives a restart. A grant you add to `nine.toml` appears at the next boot, one you
edit changes, and **one you remove stops applying** — which is the case that matters,
because a ceiling an operator narrows in the file and that does not narrow is a
control that lies. Only an `approved` grant is revocable from the CLI; a `config` or
`default` grant would reappear at the next boot, so narrowing one is an edit to the
file.

A requested scope is checked against the same validators the file is held to, at
request time — an empty `allow_hosts`, a relative mount, a reserved `NINE_*` key are
refused to the model, which can rewrite the request, rather than to you.

The invariant is untouched. `capability_request` inserts a pending row and can do
nothing else; no tool reaches a method that confers a capability. The agent writes
the code, the operator writes the grants, and they are never the same actor.

## 8. `net.http` — the one capability that needs real work

The filesystem is easy: wazero pre-opens are a battle-tested capability
primitive, and scoping a tool to `/srv/data` is one config line that wazero
enforces without our help.

The network is not, because wazero has no network at all — so `net.http` is
entirely **our** host function, and its security is entirely **our** problem.
Getting it wrong turns every generated tool into an SSRF primitive.

The daemon makes the request; the guest never touches a socket:

```text
guest:  nine_http(requestJSON) → responseJSON
host:   1. parse, enforce method allowlist
        2. resolve host → IPs
        3. reject link-local, loopback, RFC1918, ULA, multicast     ← the big one
        4. match resolved IPs against the grant's allowlist
        5. re-check after every redirect (or refuse to follow)
        6. strip Authorization/Cookie on cross-origin redirect
        7. cap response bytes; enforce timeout
        8. journal the call: tool, method, host, status, bytes
```

Step 3 is the one that is always forgotten and always exploited. Without it,
`http://169.254.169.254/latest/meta-data/iam/security-credentials/` is reachable
from a generated tool on any cloud host, and the sandbox has bought nothing —
the tool never escaped the VM, it just asked the VM politely for its credentials.
Blocking by *hostname* is not enough; the check must be on the **resolved IPs**,
after DNS, and re-done after each redirect, or DNS rebinding walks straight
through it.

`allow_hosts` **may be a bare `"*"`**, meaning any host. The wildcard grants any
*host* and never any *address*: every connection is checked at dial time, so
loopback, link-local, private ranges and multicast stay refused whatever the
allowlist says, and being at dial time that check also survives redirects and DNS
rebinding. Refusing the wildcard used to point an operator at a native plugin
instead, which is a subprocess with the daemon's uid and none of the checks above
— less safety, not more. A tool whose hosts *are* knowable still names them:
`web_search` is granted its three search endpoints, and the wildcard is for the
fetching tools that exist to retrieve whatever URL a model chose.

---

## 9. Lifecycle, visibility, and catalog pressure

### 9.1 A new tool is visible next turn

**Write it this turn, use it next turn**, in every loop, including the session that
wrote it. A session's loop lives as long as the session, so it cannot rely on being
rebuilt. Instead, each turn starts by comparing the host's catalog with the one the loop
last saw. On a change, it re-syncs the loop's dispatch handlers
against the host and re-assembles its advertised tool list, so `tool_search` and
`tool_list` see the change too. The host replaces a tool's record on every load, so a
rewrite of an existing name counts as a change.

In-flight turns keep the tool set they started with, because a tool set that mutated
mid-turn would make a turn unreplayable, and `event-journal.md` depends on replay. This
is the pull-not-push discipline of `event-journal.md`: new capability *enriches a later
turn's context* rather than interrupting a live one.

Core and plugin tools keep any name they share with a sandboxed tool, at build and on
every re-sync. Plugin tools themselves are not re-synced: a plugin started by `plugins
reload` reaches only loops built after it.

### 9.2 Catalog pressure is the sleeper problem

An agent that can write tools will write tools. Every one competes for the
context builder's tool budget (`tool-selection.md`), and a
catalog of 200 half-redundant generated tools **degrades the ranking for the
built-in tools too** — the agent poisons its own tool selection and gets worse at
everything, not just at the generated tools.

So generated tools are capped (`[tools.agent].max_tools`, default 64) with LRU
eviction on last-called-at, and `tool_write` on an existing name replaces rather
than duplicates. The `tool_search` path (`tool-selection.md`) is the
intended discovery route once the catalog is non-trivial.

This argues for a deliberately **high bar in the `tool_write` prompt guidance**:
a generated tool should be a *reusable deterministic transform*, not a one-shot
computation the agent could have done inline. The eval that matters here is not
"can it write a tool" but "does the catalog stay small and get used."

### 9.3 Audit

Every generated-tool write, every load failure, every capability-gated host call
(`net.http` target and status, `fs` mount grants) is journalled
(`event-journal.md`). A generated tool is store state, so "what code ran, with
what reach, on whose authority" must be answerable after the fact. `nine tools
show <name>` prints source, grants, and provenance.

### 9.4 Human review

`[tools.agent].require_approval` selects when `tool_write` and `js_eval` route
through the existing HITL approval gate (`hitl.md`, R-HITL.5). The gate is
reused wholesale — no new prompt surface.

| Value | Gates when |
|---|---|
| `"on_capability"` *(default)* | the tool declares any capability, **or** resolves any external dependency |
| `"always"` | every write and every eval |
| `"never"` | never — the ceiling is the only control |

The default gates on **substance rather than frequency**, and that is the whole
design intent. Approval fatigue is what defeats approval gates: prompting a human
on a pure-computation date-formatting tool trains them to approve without
reading, and then the one prompt that genuinely matters — a tool asking for
workspace read, or pulling a package nobody has heard of — meets the same reflex.
A gate that fires rarely is a gate that gets read.

The two triggers are exactly the two things §7.1 and §4.4 identify as
consequential: **reach** and **third-party code**. A tool with neither is
genuinely inert, and there is nothing for a human to usefully evaluate.

**A process write is never inert**: under `on_capability` it prompts even when it
declares nothing, since a process runs until somebody stops it. Under `never` it
does not prompt, like any other write; then `allow_processes` and `process_roles`
are what bound the processes Nine writes (§6.6).

In non-interactive deployments there is no gate at all, so the ceiling in
`[tools.agent.capabilities]` and `deps.mode` are the only controls — which is why
§7.1's guidance about narrowing the workspace mount matters most there.

---

## 10. Library choice: wazero directly, not `fastschema/qjs`

`github.com/fastschema/qjs` is the obvious candidate — it is QuickJS-NG on
wazero, CGO-free, MIT, and does what it says. It should still be declined.

**Maintenance.** As of 2026-08: **v0.0.6**, released 2025-10-28, last commit
2025-12-22, 24 open issues, 597 stars, single-vendor. That is a pre-1.0 project
with no release in ten months. wazero, by contrast, is v1.12.0 (2026-05), pushed
within days, 6.3k stars. Putting a stalled v0.0.x dependency underneath a
**security boundary** is the wrong trade at any star count.

**Fit.** The deeper objection is that qjs's value proposition is rich
bidirectional interop — `GoStructToJs`, `ProxyValue` zero-copy, `JsSetToGo[T]`,
Promise bridging, and a mandatory `value.Free()` discipline. Every one of those
is machinery for an interface Nine does not have. The tool contract is
`json.RawMessage → string`. Adopting qjs means importing a use-after-free
footgun, a stalled dependency, and ~23 transitive imports to serve an interface
that is one byte slice wide in each direction.

**What to take instead.** wazero, plus a QuickJS-NG wasm blob we **build and
vendor ourselves from a pinned tag** (§10.1). Prebuilt modules exist
(`aperturerobotics/go-quickjs-wasi-reactor`, `paralin/go-quickjs-wasi`, both MIT
and actively pushed) and are the right way to spike stage 2, but they are not
what ships.

New Go module dependencies, in full:

| Module | Version | For | Needed when |
|---|---|---|---|
| `tetratelabs/wazero` | v1.12.0 | the wasm host (§3) | always |
| `evanw/esbuild/pkg/api` | v0.28.1 | in-process dependency bundling (§4.4) | only if external deps are built |

Both are **pure Go**, MIT, and heavily used — consistent with the
`modernc.org/sqlite` choice and with `go build -mod=vendor`. esbuild is the
larger addition and removes a bigger one: without it, bundling means Node in the
image, and Node in the image means a JavaScript runtime and a package manager
sitting outside the sandbox. It is also only pulled in for
stage 6; stages 1–5 need wazero alone.

### 10.1 Building the QuickJS blob

**Decision:** the interpreter is built from source at a pinned tag, committed as a
binary artifact with a recorded hash, and rebuilt only on a deliberate version
bump. It is the code inside the trust boundary; its provenance should be as
legible as the rest of the tree, and "which interpreter is this, exactly" should
have a one-line answer.

| Pin | Version | Why |
|---|---|---|
| `quickjs-ng/quickjs` | **v0.16.1** (2026-08-04) | MIT, 3.5k stars, actively released; the maintained fork of Bellard's QuickJS |
| `WebAssembly/wasi-sdk` | **wasi-sdk-33** (2026-04-30) | latest stable; 34 is at rc.2 — pin stable, not an rc |

```text
internal/toolvm/quickjs/
  VERSION            # quickjs-ng v0.16.1 · wasi-sdk-33
  qjs.wasm           # the artifact (~1 MB, committed)
  qjs.wasm.sha256    # recorded hash, verified in CI
  build.sh           # clone at tag → wasi-sdk → emit qjs.wasm + harness.bc
  harness.js         # the run(argsJSON) → JSON wrapper (§4)
  harness.bc         # harness.js as QuickJS bytecode — this is what ships
  harness.bc.sha256  # hash of harness.js, so stale bytecode fails a test
  stdlib/            # the curated `nine:*` modules (§4.2), pinned + vendored
```

`build.sh` **must not link `js_init_module_std` or `js_init_module_os`** (§4.1).
That is the single most consequential line in it, and the reason a stock `qjs`
build is not a substitute. A test asserts the absence of both from the compiled
module's exports, so a future bump cannot quietly reintroduce them.

Three properties this has to hold:

- **Editing the harness does not rebuild the blob.** `make harness-bc`
  recompiles `harness.bc` alone, leaving `qjs.wasm` and its hash untouched, so
  ordinary harness work is not a binary-artifact review. It uses the same pinned
  checkout as the blob, because bytecode carries a `BC_VERSION` the interpreter
  checks — a mismatch fails at the first call rather than subtly. A test compares
  `harness.bc.sha256` against `harness.js`, so an edit that skips the target is
  caught rather than shipped.
- **Not in the default build.** `make quickjs-wasm` is a separate target,
  invoked only on a version bump. The blob is committed, so an ordinary
  `make build` needs no wasi-sdk, no clang, no clone — and critically **the
  runtime image gains no toolchain**, which `self-modification.md` insists
  on and this design must not quietly undo.
- **Verified, not trusted.** CI re-checks `qjs.wasm.sha256` on every run, and the
  version bump PR is where a human reviews the diff. A binary artifact in the
  tree is only acceptable if changing it is loud.
- **Reproducible enough to audit.** `build.sh` pins both the source tag and the
  toolchain release, so a reviewer can rebuild and compare hashes. Byte-identical
  output across machines is not guaranteed (clang embeds paths); the goal is that
  a mismatch is *investigable*, not that it is impossible.

**On pinning a v0.x project** — having just declined `fastschema/qjs` partly for
being pre-1.0, pinning v0.16.1 deserves a word. The risk profiles are not
comparable. A live Go module dependency tracks upstream, carries a transitive
dependency surface, and its staleness is *inflicted* on us — a stalled v0.0.x
means unfixed bugs underneath a security boundary with no recourse. Vendored
source at a pinned tag is the opposite: the artifact is byte-identical until we
choose to move it, there is no transitive surface, and upstream health matters
only at bump time — where QuickJS-NG is strong (released two days before this
note was written). We are not depending on the project's release cadence; we are
depending on one reviewed commit of C.

---

## 11. Limits

| Limit | Detail |
|-------|--------|
| `net.http` is the hard capability | wazero has no network, so `net.http` is entirely a host function and its security is entirely Nine's problem. It carries its own SSRF, rebinding and redirect-laundering checks (§8). Getting it wrong turns every generated tool into an SSRF primitive. |
| Grants are read at boot | A change to `[tool.<name>]` needs a daemon restart: `nine tools reload` re-scans the directory but resolves against the grants read at assembly. A *tool* added, edited or reloaded is visible at the next turn without a restart (§9.1). |
| `wasm` tools are unmetered | The work budget is QuickJS's interrupt handler, and wazero offers no fuel metering, so for a raw module the wall clock is the only bound. The kind is specified but unsupported — JavaScript is the supported language (`writing-sandboxed-tools.md`). |
| No per-tool memory cap | `timeout` and `max_ops` are overridable for one named tool; `memory_mb` is global, so one memory-hungry tool raises the host's worst case for every concurrent call. |
| Generated tools are `js` only | `tool_write` takes source, never a `.wasm` blob: a binary blob is not reviewable, and there is no reason to accept one. |
| No implicit state | A module is instantiated fresh per call and torn down after it — no globals, no cached credentials, no parsed index. What persists does so through the granted `state` store (§6.4), named by the tool and bounded by quota. |
| Trimmed JS surface | The interpreter surface is deliberately narrowed (§4.1). Globals a Node or browser author expects are absent, and the import surface is closed. |
| External deps are off by default | `allow_network_deps` gates them, and turning it on removes the property that makes external dependencies safe. The `net.http` interlock (§4.4) is then the only thing between a compromised transitive dependency and your source tree. |
| The generated tier's bound is its ceiling, not its switch | `[tools.agent] enabled` defaults to true; what bounds the tier is `[tools.agent.capabilities]`, which defaults to the workspace and grants nothing a tool has not declared. The agent writes code; the operator writes grants; they are never the same actor. |
| `scope = "tool"` is cross-conversation | A tool-scoped namespace is shared by every caller, which is what a cache wants and is also a channel from one conversation into another. `scope = "conversation"` closes it. |
