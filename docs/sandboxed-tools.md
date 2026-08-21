# Sandboxed tools — a Wasm tool host with conferred capabilities

- **Status:** **Stages 1–6 built** (rev 1) — the design is fully implemented. This
  note remains the design rationale; the normative contract for what exists is
  `spec/contracts/toolvm.md` (`R-TVM.*`) and the authoring guide is
  `docs/writing-sandboxed-tools.md`. Stage 6 (the `nine:*` stdlib, §4.2, and
  external npm dependencies with the write-time esbuild bundler, §4.4, incl. the
  `deps`+`net.http` interlock) is `R-TVM.15`.
  **Built:** the wazero host and ABI (§3–§4), the `js` kind with a trimmed
  QuickJS blob (§4.1) and a closed import allowlist (§4.3), developer tools with
  manifests (§5.1), the capability model end to end (§6–§7) — `fs` and `env`,
  conferred never claimed — **`net.http` with the full §8 checklist** and its
  adversarial tests (SSRF, DNS rebinding, redirect laundering, credential
  stripping), and now the **generated tier** (§5.2–§5.3, §9): `tool_write` /
  `tool_delete` / `js_eval`, the `tools` table, the operator ceiling with per-tool
  declarations, the catalog cap with LRU eviction, the conditional `require_approval`
  gate (§9.4), and write/delete audit to the daemon log (§9.3). See `R-TVM.14`.
  Stage 6 completed the set: the `nine:*` stdlib (§4.2), and external npm
  dependencies (§4.4) resolved + integrity-checked + bundled at write time via
  esbuild in-process, with the `deps`+`net.http` interlock. Nothing designed here
  remains unbuilt.
- **Date:** 2026-08-06 (proposed), stages 1–3 landed 2026-08-06, stage 4 on
  2026-08-07, stages 5–6 on 2026-08-08.
- **Motivation:** two capabilities that today have no home. (1) A **developer**
  wants to add a permanent tool without writing a Go plugin, building a binary,
  and rebuilding the image. (2) **Nine** wants to write a tool for a job it does
  not have a tool for — the gap its `gap_report` already names but cannot close.
- **Additive.** The native plugin system (`spec/contracts/plugin.md`) is untouched:
  same transport, same `plugin.ProtocolVersion`, same lifecycle. Sandboxed tools
  are a *second* backend behind the *same* dispatcher, and a deployment that
  enables none of it behaves exactly as it does today.
- **Depends on:** the tool dispatcher (`spec/contracts/dispatcher.md`), the memory
  store (generated tools are rows), the event journal (`event-journal.md`, audit),
  HITL approval gates (`docs/hitl.md`, optional review), and the pull-not-push
  discipline of `event-journal.md` (how a new tool becomes visible).

---

## 1. TL;DR — recommendation

| # | Piece | Shape |
|---|---|---|
| 1 | **Wasm host** (§3) | `internal/toolvm`, built on **wazero** (pure Go, no CGO). One wasm instance **per call**, torn down after. Registers handlers on the existing `Dispatcher` exactly like `RegisterPlugin` does. |
| 2 | **JS is a guest, not the host** (§4) | QuickJS-NG compiled to wasm is *one pre-supplied guest module*. A developer may equally ship a raw `.wasm` built from Rust/TinyGo/Zig. Same ABI, same capability model. |
| 3 | **Two authors, two trust tiers** (§5) | **Developer tools** are files on disk with a manifest, installed by the operator. **Generated tools** are rows in SQLite, authored by Nine. Different ceilings, one runtime. |
| 4 | **Capabilities are conferred, never claimed** (§6–§8) | Default is the empty set: no filesystem, no network, no env, no clock. Every capability is an explicitly-exported host function or a wazero pre-open. A manifest *declares a need*; only operator config *grants*. |
| 5 | **Dependencies never resolve at call time** (§4.2) | Developers **pre-bundle** their deps at dev time; generated tools get a curated, vendored `nine:*` stdlib. Module resolution is always host-side against a closed allowlist. |
| 6 | **External packages: opt-in and allowlisted** (§4.4) | Off by default. When enabled, an operator **names the permitted packages** (transitive deps included); Nine resolves, integrity-checks, and bundles them **in-process at write time** via esbuild — never in the sandbox, never at call time. No install scripts ever run. `deps` + `net.http` is refused by default. |
| 7 | **Visibility is next-turn** (§9) | A new tool is picked up by subsequently-built agent loops. This is already how `plugins reload` behaves — no new push machinery. |

**Do not take `github.com/fastschema/qjs`** (§10). Use wazero directly.

---

## 2. What this is not

This does not let Nine modify Nine. There is no path here by which the agent
edits Go source, rebuilds the binary, alters `nine.toml`, writes a native plugin,
or changes a built-in skill. Every one of those stays removed
(`docs/self-modification.md`). Nine's executable shape remains fixed.

What changes is narrower and needs to be stated precisely, because
`docs/self-modification.md` and `R-PLUG.7` currently forbid it in passing:

> **R-PLUG.7:** No agent-reachable tool or path may write plugin source, build a
> plugin, start a new plugin binary, hot-swap, or roll back a plugin. This is a
> self-modification boundary: **Nine cannot grant itself capabilities.**

The final sentence is the invariant that actually matters, and this design
**preserves it verbatim**. The clause that must be amended is the one about
*writing code*, because that is what a generated tool is.

The distinction the amendment must draw:

| | Code | Capabilities |
|---|---|---|
| **Native plugin** | operator (build time) | operator (`nine.toml`) |
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
listable, readable, exportable, deletable, and journalled. `nine tools list` shows
you the whole set. The property that `docs/` and the binary match its version is
untouched.

---

## 3. The host: wazero, one instance per call

`internal/toolvm` owns a wazero runtime, a compilation cache, and a registry.
Compilation happens once per module; **instantiation happens per call**, and the
instance is closed when the call returns.

That sounds wasteful and is not. Tool calls are gated behind LLM turns — they are
already ≥100ms events, and Nine makes at most a handful per turn. Against that,
instantiating a cached module is noise. In exchange, per-call instantiation buys
the strongest property in the design:

**No state survives a call.** Not a global, not a cached credential, not a
poisoned prototype, not a half-freed heap. Two calls to the same tool cannot
observe each other, and a tool cannot accumulate anything across a session. A
long-lived shared runtime would need all of that reasoned about; a fresh instance
makes it true by construction.

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
| Wall clock | `WithCloseOnContextDone(true)` + context deadline | 5s |
| Memory | `WithMemoryLimitPages` | 256 pages (16 MiB) |
| Output | the dispatcher's existing cap + spill (R-DISP.2) | 2048 tokens |
| CPU | **none — see below** | — |

wazero has **no fuel/gas metering**. The wall-clock deadline is the only CPU
bound, and it is enforced by closing the module out from under the guest. This is
adequate (a spinning tool dies in 5s and the model observes a normal failure) but
it must be written down rather than assumed: an operator running many concurrent
sessions is trusting the deadline, not a work budget.

---

## 4. JS is a guest, not the host

The host is a **wasm host**. It knows nothing about JavaScript. A tool is a wasm
module exporting `run`, and there are two ways to get one:

- **`wasm` tools** — a developer ships a `.wasm` built from Rust, TinyGo, Zig, or
  C. Full speed, any language, no interpreter in the middle.
- **`js` tools** — the module is the *pre-supplied QuickJS-NG interpreter*, and
  the tool's source is JavaScript handed to it. Nothing is compiled at install
  time.

This is what makes generated tools tractable. An LLM writes correct JavaScript
far better than it writes Rust that compiles to wasm, and — decisively — **there
is no build step**, so no toolchain in the runtime image (`docs/self-modification.md`
is emphatic that there is none, and this design does not add one).

Both kinds are the same to everything downstream: same ABI, same capability
model, same dispatcher registration, same audit trail. QuickJS is an
implementation detail of one tool *kind*, not an architectural layer.

**The ABI is deliberately tiny**, because the tool contract it has to satisfy
already is (`internal/plugin/contract.go`):

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
`docs/versioning.md`). A module declaring an unsupported ABI is refused at load.

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
those to the runtime image would undo `docs/self-modification.md` exactly as a
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

The candidate set is deliberately boring — CSV, date arithmetic, YAML/TOML,
a diff, maybe a string-distance function. These are the things a tool-writing
agent actually reaches for, and each one it *cannot* import is a wheel it will
reinvent badly inside a 5-second deadline.

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

**Bytecode.** The harness and the `nine:*` modules are the same bytes on every
call, so they are precompiled to QuickJS bytecode once and instantiated from
that. With per-call instantiation (§3) this matters: it moves parsing off the hot
path entirely, and only the tool's own source is compiled per call.

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

The payoff is a convergence worth stating plainly: **a generated tool with
dependencies becomes a developer tool, mechanically.** Same artifact — one
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

Property 2 is what makes external dependencies safe, and network egress is
precisely what dissolves it. A package that can reach the network can exfiltrate
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

## 5. Two authors, two trust tiers

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
already split built-in from agent-authored (`docs/self-modification.md`). Kind is
always `js` — the agent cannot supply a `.wasm` blob, because a binary blob is
not reviewable and there is no reason to accept one.

**A generated tool declares what it needs, exactly as a developer tool does**
(§6.3). The declaration is checked against the operator's ceiling (§7) and is
what the approval gate keys on (§9.4). A tool that declares nothing — the common
case — gets nothing, regardless of how permissive the ceiling is. Least privilege
is per tool, not per tier.

Symmetry with skills is deliberate and load-bearing:

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
| `net.http` | host allowlist, methods, max bytes, timeout | **none** | host fn (§8) |
| `env` | explicit key allowlist | **none** | `WithEnv`, per key |
| `clock` | — | **granted** | `WithSysWalltime` |
| `random` | — | **granted** | `WithRandSource` |
| `log` | — | **granted** | host fn → `slog` + journal |

`clock`, `random`, and `log` are on by default because they leak nothing and
every non-trivial tool needs them. Everything with reach — the filesystem, the
network, the process environment — starts at nothing.

`env` deserves its explicit-allowlist treatment rather than an all-or-nothing
flag: the daemon's environment holds LLM provider API keys. A tool granted "env"
wholesale is a credential exfiltration primitive. Grants are per key, and the
`NINE_*` and `*_API_KEY` patterns are refused outright as a config error.

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
[tool.csv_stats.capabilities.fs]
read = [{ host = "/srv/data", guest = "/data" }]

[tool.csv_stats.capabilities.net.http]
allow_hosts = ["api.example.com"]     # exact or "*.example.com"; no bare "*"
methods     = ["GET"]
max_bytes   = 1048576

# ── The ceiling for everything Nine writes itself ─────────────────────────
[tools.agent]
enabled  = true
eval     = true       # allow js_eval (§5.3)
max_tools = 64        # catalog pressure — §9

# Gate on substance, not on every write (§9.4):
#   "on_capability" | "always" | "never"
require_approval = "on_capability"

# The MAXIMUM a generated tool may be granted — not an automatic grant.
# A tool that declares nothing still gets nothing.
[tools.agent.capabilities.fs]
read = [{ host = "${NINE_WORKSPACE}", guest = "/workspace" }]
```

Four properties worth stating:

- **`[tools.agent.capabilities]` is a ceiling, not a default.** A generated tool
  cannot request its way past it, and does not receive it merely by existing. It
  must declare `fs = ["read"]` (§5.2) to get workspace read; a tool that declares
  nothing runs with nothing. The ceiling bounds what is *grantable*, §5.2's
  declaration decides what is *granted*, and the two are deliberately separate so
  that widening the ceiling does not retroactively widen every existing tool.
- **Refusal is a usable signal.** If Nine writes a tool reaching for `net.http`
  and the ceiling excludes it, `tool_write` **fails with a message the model can
  read** — "capability net.http is not available to generated tools" — so the
  agent rewrites without it or calls `gap_report` for a human to decide. That
  failure path is a feature.
- **Grants are per named tool.** There is no wildcard `[tool."*"]`. An operator
  granting filesystem access to a *developer* tool does so to a tool they have
  read.
- **Read at load.** A grant change reaches a running tool only on `nine tools
  reload` or restart, matching R-PLUG.10.

### 7.1 On the default workspace-read ceiling

Shipping `fs.read` over the workspace as the default ceiling is a deliberate
choice for usefulness, and it has one consequence worth naming precisely.

It grants the *agent* no new reach: the `files` plugin already reads the
workspace, so nothing becomes visible to Nine that was not already. What changes
is the reach of **a tool's dependencies** (§4.4). With an empty ceiling, a hostile
npm package can only return a wrong answer. With workspace read, it can *see the
workspace* — and if it could also reach the network, exfiltrate it.

It cannot, and that is the whole point of the §4.4 interlock. But the interlock
shifts from belt-and-braces to **load-bearing**: it is now the only thing standing
between a compromised transitive dependency and your source tree. Two things
follow, and neither is optional:

1. `allow_network_deps` stays off. Turning it on with this ceiling is the one
   combination that makes a supply-chain compromise materially dangerous.
2. `deps.mode = "allowlist"` is strongly preferred over `"open"` on any instance
   whose workspace holds anything you would not publish.

An operator running a workspace with secrets in it — `.env`, private keys,
credentials — should narrow the mount to a subdirectory rather than accept
`${NINE_WORKSPACE}` wholesale. The `guest` path makes that a one-line change.

---

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

The allowlist is matched against resolved addresses, and **there is no bare
`"*"`** — an operator who wants an unrestricted egress tool should write a native
plugin, where that intent is explicit and reviewed.

---

## 9. Lifecycle, visibility, and catalog pressure

### 9.1 A new tool is visible next turn

This needs no new machinery, because the semantics already exist. From
`internal/plugin/userplugins.go`:

> Newly-started plugins are picked up by subsequently-built agent loops (the
> builder reads `Running()` at build time); turns already in flight keep the tool
> set they started with.

`buildToolList` (`internal/runtime/builder.go:820`) reads the tool set when a loop
is built. A sandboxed tool registered in the store is picked up the same way, so:
**write it this turn, use it next turn.** In-flight turns keep the tool set they
started with, which is exactly right — a tool set that mutated mid-turn would
make a turn unreplayable, and `event-journal.md` depends on replay.

It is also precisely the pull-not-push discipline of `event-journal.md`:
new capability *enriches a later turn's context* rather than interrupting a live
one.

### 9.2 Catalog pressure is the sleeper problem

An agent that can write tools will write tools. Every one competes for the
context budget in `selectTools` (`internal/context/builder.go:232`), and a
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
through the existing HITL approval gate (`docs/hitl.md`, R-HITL.5). The gate is
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
larger addition and is worth the cost precisely because it removes a much bigger
one: without it, bundling means Node in the image, and Node in the image means a
JavaScript runtime and a package manager sitting outside the sandbox — which is
the exact thing this design exists to avoid. It is also only pulled in for
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
  build.sh           # clone at tag → wasi-sdk → emit qjs.wasm
  harness.js         # the run(argsJSON) → JSON wrapper (§4)
  stdlib/            # the curated `nine:*` modules (§4.2), pinned + vendored
```

`build.sh` **must not link `js_init_module_std` or `js_init_module_os`** (§4.1).
That is the single most consequential line in it, and the reason a stock `qjs`
build is not a substitute. A test asserts the absence of both from the compiled
module's exports, so a future bump cannot quietly reintroduce them.

Three properties this has to hold:

- **Not in the default build.** `make quickjs-wasm` is a separate target,
  invoked only on a version bump. The blob is committed, so an ordinary
  `make build` needs no wasi-sdk, no clang, no clone — and critically **the
  runtime image gains no toolchain**, which `docs/self-modification.md` insists
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

## 11. Build order

Each stage is independently useful and independently shippable.

| Stage | Scope | Proves |
|---|---|---|
| **1** | `internal/toolvm`: wazero host, raw `.wasm` only, **no capabilities**, dispatcher registration, developer tools from `user_dir` | the host, the ABI, the loader, the collision rules |
| **2** | `js` kind — trimmed QuickJS blob (§4.1), harness, host-side module allowlist, `nine:*` stdlib. Spike against a prebuilt module, ship the vendored pinned one (§10.1) | an author writes JS, not Rust — with a closed import surface |
| **3** | Capability model: `fs` pre-opens, `env` allowlist, grant resolution, load-time failure on ungranted declarations | §6–§7 end to end, developer tier complete |
| **4** | `net.http` host function with the full §8 checklist + its own adversarial tests (SSRF, rebinding, redirect laundering) | the hard capability |
| **5** | `tool_write`, `js_eval` (§5.3), the `tools` table, the agent ceiling + per-tool declarations, cap + eviction, conditional HITL gate, audit | the generated tier |
| **6** | External deps (§4.4): esbuild bundling, allowlist policy, integrity + lockfile, cache, the `net.http` interlock | libraries, safely |

**Stage 3 is the real milestone** — at that point a developer can add a permanent,
sandboxed, capability-scoped tool by dropping two files, and the entire
generated-tool tier is still switched off. That is a complete feature on its own,
and shipping it before stage 5 means the capability model is proven by a human
author before it is load-bearing for a machine one.

**Stage 6 depends on stage 5's containment being real**, not merely designed. Its
whole safety argument is "a malicious package can only do what the tool was
granted" — which is worth exactly as much as the capability model underneath it.
It should not be built until stage 3's grant resolution and stage 4's `net.http`
checks have adversarial tests passing.

Recommended production posture once all six land:

| Setting | Value |
|---|---|
| Developer tools | named grants, per tool, reviewed |
| `[tools.agent.capabilities]` | workspace `fs.read` — narrowed to a subdirectory if the workspace holds secrets (§7.1) |
| `require_approval` | `"on_capability"` |
| `deps.mode` | `"allowlist"` with `frozen = true` |
| `allow_network_deps` | **off** |

`deps.mode = "open"` belongs on a development instance — one where the agent is
exploring — not one serving a production workload. The intended path is to
iterate open, then freeze the lockfile and copy it forward.

---

## 12. Documents this changes

Reconciled via `/sync-nine` when the code lands, not before:

| Document | Change |
|---|---|
| `docs/self-modification.md` | Amend "Generate / build / hot-swap plugins — Removed". Native plugins stay removed; add generated *sandboxed tools* as permitted, with the code/capabilities split of §2 as the rationale. |
| `spec/contracts/plugin.md` (R-PLUG.7) | Narrow to native plugins. "Nine cannot grant itself capabilities" stays **unchanged** — it remains true and is now load-bearing for two subsystems. |
| `spec/contracts/toolvm.md` | **New.** `R-TVM.*`: ABI, capability set, grant resolution, load sequence, collision rules, resource bounds. |
| `spec/contracts/dispatcher.md` (R-DISP.3/4) | Tool taxonomy gains a third branch: plugin / core-intercepted / **sandboxed**. |
| `docs/configuration.md` | `[tools]`, `[tool.<name>]`, `[tools.agent]`, `[tools.agent.deps]`. |
| `docs/usage.md` (deps) | `nine tools deps`, lockfile inspection, freeze/thaw. |
| `docs/versioning.md` | `toolvm.ABIVersion` alongside `plugin.ProtocolVersion`. |
| `docs/glossary.md` | *sandboxed tool*, *generated tool*, *capability grant*, *`nine:*` stdlib* — the overloading warning in CLAUDE.md applies with force here. |
| `docs/writing-sandboxed-tools.md` | **New.** Authoring guide: manifest format, the ABI, bundling deps with esbuild (§4.2), the `nine:*` set, and what QuickJS does *not* provide. |
| `docs/usage.md` | `nine tools list/show/reload`, `nine tool validate`. |
| `docs/hitl.md` | `require_approval = "on_capability"` as a new gate trigger (§9.4). |
| `spec/contracts/dispatcher.md` (R-DISP.3) | `tool_write` and `js_eval` join the core-intercepted tool list. |
| `docs/README.md` | Index entry. |
| `Makefile` / CI | `make quickjs-wasm` (bump-only, §10.1) and the `qjs.wasm.sha256` verification step. |
| `docs/installation.md` | Note that the committed blob means no wasi-sdk for an ordinary build. |
