# Design note — Extracting the tool VM

**Status:** **Proposed** · **Depends on:** nothing · **Related:** `spec/contracts/toolvm.md`,
`adr/agent-boundary.md`, `adr/rich-js-tools.md`, `adr/capability-grants.md` ·
**Amends:** R-TVM.1 and R-TVM.15 (names only, with a compatibility window — see §5)

`internal/toolvm` is the in-process wasm host behind every sandboxed tool: wazero, the
QuickJS blob, the guest ABI, capability grants and ceilings, the `net.http` gate and its
SSRF guard, durable state, resumable jobs. It contains nothing agent-specific. This note
proposes making it a library that can be embedded without Nine — a host for running
untrusted, capability-scoped JavaScript or wasm tools.

**Recommendation:** remove its one Nine import, give the guest ABI neutral names with a
compatibility window, rename the generated-tier API, and move it to `nine/pkg/toolvm`.
Split it into its own module or repository only when a consumer other than Nine exists.
Estimated at about a day of work, in five PRs that each leave Nine green (§7). §8 lists
what is undecided.

---

## 1. Current coupling

The package already has a library-shaped boundary.

- **One nine import.** `internal/toolvm` imports `nine/internal/llm` and nothing else from
  Nine, and only for `Tool.ToLLMDef()` (`internal/toolvm/host.go:954`). `internal/toolvm/deps`
  imports nothing from Nine.
- **Small external surface.** wazero, BurntSushi/toml (the manifest), esbuild (`deps/bundle.go`).
- **Seams already point outward.** `Config` takes callbacks and interfaces — `StateStore`,
  `TouchGenerated` — and its comments say so: "the daemon wires it to the store; this
  package has none." The zero `Config` is a disabled host. Grants arrive as a
  `map[string]Grant`; `nine.toml` parsing lives in `internal/config`, not here.
- **Self-contained assets.** `qjs.wasm`, `harness.bc`, `stdlib/*.js` and `shipped/*.js` are
  `go:embed`ed from inside the package, with `quickjs/build.sh` to reproduce the blob.

Size: about 10.9k lines of Go including tests, plus about 1.2 MB of embedded QuickJS.

Consumers inside Nine: `internal/runtime` (22 files), `internal/agent` (6),
`internal/cli` (1), `tests/evals/runner` (1). The surface they use is `Host`, `Open`,
`Config`, `Tool`, `Output`, `Grant`/`Declaration`/`Mount`/`Ceiling`, the `Cap*` constants,
`Continuation`/`JobContext`, the state and HTTP audit context helpers, `Generated`/`AgentConfig`,
and `LoadManifest`. That surface is the library's public API on day one.

## 2. Reasons for and against

- **A reusable piece on its own.** "Run an LLM-authored tool with no ambient authority and an
  explicit, auditable grant" is useful to any Go agent framework, MCP server or workflow
  engine, not only Nine.
- **Forces the boundary to stay honest.** Today nothing stops a future change from reaching
  into `internal/runtime` from `toolvm`. A separate module makes that a compile error.
- **Pairs with `adr/agent-boundary.md`.** That note splits runtime from agent; this one
  splits the tool sandbox from the runtime.

Against: Nine is the only consumer today. Every cross-cutting change
(a new capability, an ABI bump) becomes two changes and a tag. §6 is about not paying that
before there is a reason to.

## 3. Library and Nine responsibilities

| Part | Goes to the library | Stays in Nine |
|------|:---:|:---:|
| Host, ABI, loader, manifest (`host.go`, `abi.go`, `load.go`, `manifest.go`) | ✓ | |
| Capabilities, grants, ceilings (`capability.go`) | ✓ | |
| `net.http` gate, SSRF guard (`nethttp.go`, `ssrf.go`) | ✓ | |
| Durable state interface and quotas (`state.go`) | ✓ | the SQLite `StateStore` |
| Resumable-job envelope (`Continuation`, `JobContext`) | ✓ | the job driver, `StandingRunner` |
| QuickJS blob, harness, `build.sh` | ✓ | |
| `stdlib/` JS modules | ✓ | |
| `deps/` (npm resolution and bundling) | ✓, as a subpackage | the `[tools.agent.deps]` policy config |
| Generated-tier API (`Generated`, `AgentConfig`, `LoadGenerated`) | ✓, renamed — §4.3 | `tool_write`, the store rows, the approval gate |
| Shipped tools (`shipped/*.js`, `shipped.go`) | open — §8.2 | |
| `ToLLMDef()` | | ✓, as a helper beside its caller |
| `nine.toml` → `Grant` translation | | ✓ (`internal/config`) |

The rule: the library owns *how a tool runs and what it may reach*. Nine owns *where tools
come from, who approves them, and where their state lives*.

## 4. Changes needed before the move

### 4.1 Drop the `llm` import

Remove `Tool.ToLLMDef()`. Add the equivalent function where it is called — it maps three
fields (`Name`, `Description`, `InputSchema`) onto `llm.ToolDef`. After this, the package
imports nothing from Nine.

### 4.2 Neutral names in the ABI

The guest-visible names carry the product name. This is the only change a guest can see:

| Where | Today | Proposed |
|-------|-------|----------|
| Host import module (`abi.go:74`) | `nine` | `toolvm` |
| Guest exports (`abi.go:61-62`) | `nine_alloc`, `nine_run` | `toolvm_alloc`, `toolvm_run` |
| Stdlib specifiers (`stdlib/`, `harness.js`) | `nine:fs`, `nine:csv`, … | `toolvm:fs`, `toolvm:csv`, … |

Final names are §8.1. Compatibility is §5.

### 4.3 Product vocabulary in the API

`Generated`, `AgentConfig`, `LoadGenerated` and `Tool.Generated` model Nine's
"the agent writes its own tools" tier (R-TVM.14). The mechanism is general — an untrusted
tier capped by a ceiling rather than granted per tool — and the names should say that:
for example `Untrusted` / `CeilingConfig` / `LoadUntrusted`. Same for comments that cite
`nine tools show`, `NINE_WORKSPACE` and `[tool.<name>]`; they become statements about the
`Config` field instead of the config file.

`Tool.Shipped` and `Tool.Generated` collapse naturally into one `Tool.Tier` enum with
values the embedder defines or the library fixes (§8.3).

### 4.4 Spec and docs

The normative parts of `spec/contracts/toolvm.md` that describe the runtime — R-TVM.1 to .6,
.8, .9, .12, .15, .18, .19 — move to the library as its contract. The parts that describe
Nine — R-TVM.7 (grants in `nine.toml`), .10/.11 (loading and reporting), .14 (the generated
tier's lifecycle and approval), .20 (standing tools) — stay, and cite the library's
contract by version. `spec/conformance.md` gains one row: the pinned library version.

## 5. Compatibility

Existing guests break under §4.2 if the old names simply disappear: every developer tool
built as raw `wasm` imports from `nine` and exports `nine_run`, and every JS tool and
generated tool in the store imports `nine:*`.

Proposal:

- **Accept both for one major version.** The host registers the host module under both
  names; the loader looks for `toolvm_run` then `nine_run`; the module resolver maps
  `nine:x` to `toolvm:x`. All three are a few lines each.
- **Report it.** A tool that resolves through an old name loads, with a `deprecated` note
  in its `Status`, so `nine tools` shows what needs updating.
- **Rewrite generated tools in place.** They are rows Nine owns; a migration can rewrite
  their import specifiers rather than waiting for them to be regenerated.
- **Bump `ABIVersion` to 2** when the old names are dropped, not when the new ones are added.

## 6. Packaging options

In order of commitment. Each is a superset of the one before.

1. **Public package inside Nine** — `nine/pkg/toolvm`. Do §4 and move the directory. Anyone
   can import it at once; they pull in Nine's whole `go.mod` to do it.
2. **Separate module, same repo** — `nine/toolvm/go.mod`, tagged `toolvm/vX.Y.Z`. Its own
   small dependency set; changes that touch both still land in one PR; Nine depends on it
   through a `replace` during development and a tag at release.
3. **Own repository** — `github.com/djordlucas/toolvm` (name open, §8.1). The clearest story
   for an outside user, and the full two-repo cost: two PRs and a tag per cross-cutting
   change, a re-vendor in Nine, and `sync-nine` / `sync-evals` taught about it.

**Recommendation:** do option 1 now — it is where the real work (§4, §5) happens — and move
to 2 or 3 when a consumer other than Nine exists. Fix the ABI names (§4.2) before anything
is published under a public path, because after that a rename is a migration for someone
else.

## 7. Plan

1. Drop `ToLLMDef` (§4.1). No behavior change.
2. Neutral ABI names with both accepted (§4.2, §5). Shipped tools and the stdlib switch to
   the new names; tests cover both.
3. Rename the generated-tier API (§4.3). Mechanical; callers in `internal/runtime`.
4. Decide shipped tools (§8.2) and move them if they are leaving.
5. Move to `pkg/toolvm` and rewrite imports.
6. Split the spec (§4.4).
7. Later, on demand: option 2 or 3.

Steps 1 to 5 are each one PR that leaves Nine green.

## 8. Open questions

1. **The name.** `toolvm` is the package name, and generic. It is also what ends up in the
   import module and the stdlib specifiers, so choosing it is choosing the ABI.
2. **Shipped tools.** `read_file`, `edit_file`, `delete_file`, `trash_list` and the rest
   encode Nine's workspace-and-trash model (`adr/file-namespaces.md`). In the library as an
   optional `tools/fs` subpackage, or kept in Nine? The HTTP ones (`http_get`, `http_post`,
   `web_page_read`) are more generic than the file ones.
3. **Tiers.** Does the library know about tiers (developer, untrusted, first-party) as a
   fixed enum, or does it expose one mechanism — a grant, optionally capped — and let the
   embedder name its tiers?
4. **The resumable-job envelope.** `Continuation` is in the library, the driver is not. Is a
   minimal reference driver worth shipping so an embedder can run a resumable tool without
   writing one?
5. **`deps`.** npm resolution brings esbuild and network access at write time. A subpackage
   keeps it out of a minimal embed — is that enough, or should it be its own module?
6. **The QuickJS blob.** Built by `quickjs/build.sh` and checked in with a sha256. Does that
   stay a checked-in artifact, or does the library publish it as a release asset?

## Limits

| Limit | Detail |
|-------|--------|
| Nothing built | Proposed only; no code has moved. |
| No outside consumer yet | The case for options 2 and 3 (§6) rests on a consumer that does not exist; this note recommends not paying for them until one does. |
| API stability not designed | Moving to `pkg/` makes today's surface public as-is. No review of which exported names should stay exported has been done. |
| Plugins out of scope | Native plugins (`spec/contracts/plugin.md`) are a separate backend and are not part of this extraction. |
