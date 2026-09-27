# Capability grants — defaults on, and a request path

- **Status:** **Implemented** (2026-09-27). Both parts shipped on
  `feat/tools-default-on-and-grants`. This note records why the defaults moved and
  why the ceiling left `nine.toml`, because both reverse a position the codebase
  argued for in writing.
- **Date:** 2026-09-27.
- **Scope:** `[tools] enabled`, `[tools.agent] enabled`, `[tools.agent.capabilities]`,
  `[workspace].root`, and the `capability_requests` / `capability_grants` tables.
- **Motivation:** `adr/critic.md` findings `C4` and `C5`.

---

## 1. What changed

| Before | After |
|---|---|
| `[tools] enabled` defaulted false; both shipped configs set it true | Defaults true. A `*bool`, so declining it is distinguishable from saying nothing |
| `[tools.agent]` absent from both shipped configs | Defaults true, gated by `[tools] enabled` above it (`ToolsConfig.GeneratedEnabled`) |
| `[workspace].root` had no default | `Config.WorkspaceRoot()`, mirroring `DatabasePath()` |
| The ceiling was empty unless an operator wrote one | Defaults to `[workspace].root`, read and write, derived in code |
| The ceiling lived only in `nine.toml`, read at boot | Lives in the store; `nine.toml` is reconciled into it at every boot |
| A ceiling refusal told the model to use `gap_report`, which is never advertised | `capability_request`, advertised, records a request an operator decides |
| A grant needed a file edit and a restart | An approval installs the ceiling on the running daemon |

## 2. Why the defaults moved

The sandboxed tier was the only real boundary in Nine and it guarded the tier
nobody ran. `docs/sandboxed-tools.md §1` called off-by-default "the additive
property the whole design rests on", and that property was worth what it cost only
while the tier was optional. It stopped being optional when the shipped file tools
moved into it: `[tools] enabled = false` now means an agent that cannot read or
write a file, which is not a posture anyone chooses deliberately.

The argument for the generated tier is the one `docker/nine.toml` already made for
the host tier, and it is a relative one. `shell` is on by default and runs commands
as the daemon's process user with its full filesystem and network reach. A generated
tool runs in wasm, one instance per call, with no network, no environment, and
nothing mounted but the workspace `shell` already runs in. Defaulting the sandboxed
tiers on **reduces the share of the agent's reach that sits outside a boundary**. It
does not widen the total.

What makes that safe to default is that the switch was never the control. The ceiling
is: a tool is granted only what it declares and only what the operator conferred, and
`resolveCeiling` refuses anything else. A tier that is on with a narrow ceiling is not
a tier that is unbounded. Every switch that widens reach beyond the workspace stayed
off — `deps`, `allow_network_deps`, `allow_long_running`, `allow_standing`, and
`net.http` in the ceiling.

### The workspace root was load-bearing and undefaulted

`resolveShipped` skips any shipped tool declaring `fs` when no workspace is
configured. Flipping the host on without defaulting `[workspace].root` would have
produced a daemon whose `read_file`, `write_file`, `edit_file` and six others all
skip — a working host with no working file tools. This was found by reading
`shipped.go`, not by a test: nothing covered the combination, because the
combination was previously unreachable.

### The ceiling is derived, not written

An fs mount needs an absolute host path and nothing in `internal/config` expands
variables in config values, so the workspace cannot be named in TOML portably. The
`${NINE_WORKSPACE}` example in four documents and in `nine.toml` was not merely
unhelpful — `validateToolEntry` would have refused it. Deriving the mount in
`agentConfig` is what `SetShippedWorkspace` already did for the shipped tier, and it
reuses the same guest path so one file has one name whichever tier reaches it.

### A guard was re-aimed, not removed

`TestPublishedConfigIsConservative` asserted by regex that `docker/nine.toml`
contained no `[tools.agent]` section. It existed to prevent this change. It was
rewritten rather than deleted because its premise — a published default reaches
people who never read it — is still right, and because a textual assertion can no
longer verify the posture: with the tiers defaulting on, an absent key means
*enabled*, so the old form would have passed a config that turned everything on. It
now loads the file through `config.Load` and asserts the resolved posture.

## 3. Why the ceiling left `nine.toml`

`C5` was a dead end with three segments, and only the last needed a new home for the
ceiling:

1. The refusal named `gap_report`, which `buildToolList` never advertises. Fixed by
   adding `capability_request` to `coreToolNames`.
2. Nothing persisted a request. Fixed by `capability_requests`.
3. A grant required a file edit and a restart. This is the one that moved the ceiling.

An approval has to take effect somewhere. Writing `nine.toml` was never an option —
R-CFG.3 forbids a runtime config-rewrite tool, and for good reason. So the grant is
recorded in the store, and the store became the source of truth for what is in force.

### Provenance, not a sentinel

The first design was "seed the store from the file on first boot", the self-model's
pattern. It was rejected: `docs/personalities.md` records that pattern's wart as a
Limit — "the self-model is seeded, never re-synced; editing the file after first boot
changes nothing." For a persona that is a papercut. For a capability ceiling it is a
security bug, because an operator who narrows the file expects the narrowing to bite,
and a seed-once design cannot express a removal at all.

What shipped instead is provenance plus wholesale reconciliation. Each row carries a
`source`; a boot deletes every `default` and `config` row and rewrites them, and
leaves `approved` alone. That gives the file both roles — first-boot seed and change
feed — with no sentinel, no stored snapshot and no three-way merge, and it makes
removal work: a grant the file stops declaring stops applying.

This is the doctrine `standing_tools` already states for its own split
(`internal/memory/standing_tools.go`): configuration owns the definition, the runtime
owns the state.

### What the invariant actually was

`config.go`'s axiom is *the agent writes the code, the operator writes the grants,
never the same actor*. That holds unchanged: approval is an operator action, and
`CapabilityRequestCreate` is the only capability method a tool can reach — it inserts
a `pending` row and cannot confer anything.

What changed is the weaker claim that grants *live in `nine.toml`*, stated at
`internal/memory/db.go` and `docs/sandboxed-tools.md`. It becomes: the file declares
the baseline, the store holds what is in force. Both statements were amended in
`docs/` and `spec/`.

A grant arriving through approval is validated by the same code the file is —
`config.ValidateCapabilityGrant`, an exported wrapper over `validateToolEntry` rather
than a restatement of its rules — so the store cannot hold a ceiling `nine.toml`
could not express. Validation runs at *request* time, not approval time, so the
refusal reaches the model, which can rewrite the request, instead of the operator,
who cannot.

## 4. One decision path, three front ends

`runtime.CapabilityService` holds the decision. The CLI (`nine grants`), the TUI
(`/grants`) and the API (`/capabilities`) all reach it over the wire protocol, and
none of them touches the store directly — only the process running the agents can
install a ceiling on its live tool host, and three implementations of "widen a
capability" is three places to get it wrong.

`/grants` makes the TUI mutating for the second time, after `/plan-mode`. The
alternative was worse: the agent posts a request to `nine notifications`, which the
TUI shows, and the operator then has to leave the session to act on it.

## 5. A test that could not fail

`TestEveryClientMsgTypeIsDispatched` did not catch a wire message with no handler.
The field-less verbs share one `QueryReq` and are routed by a second switch on
`Kind`; a verb missing a case there never reaches the outer default branch, so it
produces no `unknown message type` — it produces *nothing*, and that test reads a
silent connection as "routed" and skips. A `grants_list` with no inner case reached a
built binary and hung the client until its own deadline.

`TestEveryQueryKindIsAnswered` asserts the stronger property: every query kind
answers something. It was confirmed to fail with the case removed, which the test it
supplements could not do.

## Limits

| Limit | Detail |
|-------|--------|
| No per-tool grants in the generated tier | The ceiling is per instance. A generated tool gets what it declares of the ceiling, so two tools declaring `fs.read` get the same mounts. Per-tool generated grants would need a grant keyed by tool name, which the schema allows and nothing writes. |
| Two writers of one capability union | A `config` grant and an `approved` grant of the same capability accumulate: mounts and env keys append, and `net.http` hosts and methods merge. The wider one effectively wins. `nine grants` shows both so the effective reach is legible rather than inferred. |
| A revoked grant does not stop a running call | Revocation re-projects the catalog, so the tool stops loading. A call already in flight in the wasm host completes. |
| Approval is not audited beyond the row | `capability_grants.request_id` records which request a grant came from, and the journal records the request. Who approved it is not recorded, because the daemon has no notion of operator identity. |
| `gap_report` is still unadvertised | This note fixed the ceiling path by adding a tool beside it, not by fixing `gap_report`, whose event still reaches a supervisor arm that does nothing (`adr/critic.md` `C2`). |
| Eval coverage is unchanged | `adr/critic.md` `C1` records that the approval gate, HITL and safety have no eval cases. This change enlarges that untested surface; the cases remain separate work. |
