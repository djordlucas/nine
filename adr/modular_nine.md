# Modular Nine — built-ins as Go, MCP for subprocess tools

- **Status:** Proposed (architecture analysis). Nothing here is built.
- **Date:** 2026-11-01.
- **Reviewed:** 2026-11-06 — fact-checked against the tree; §1 tool count and names, §2 progress-rail wording, §3 database ownership, §5 supervision wording corrected; test plan added as §10.
- **Decisions:** 2026-11-06 — §9 items 1–4 answered by the requester: all tiers in scope, both directions, the sandbox fronted by MCP, prefix every server's tools.
- **Revised:** 2026-11-08 — §9 items 8–11: built-in tools stay direct Go calls in the dispatcher and MCP is the only wire protocol across process boundaries (architecture C taken to completion); the toolvm is removed with its shipped tools translated to Go (model-authored tools refactored separately); first-party tool names do not change (reverses item 4); phases P0/P2/P3 dropped.
- **Scope:** the daemon's tool-serving path — the plugin contract (`spec/contracts/plugin.md`), the `mcp` bridge (`internal/builtins/mcp.go`, `mcp_stdio.go`, `mcp_http.go`), the toolvm host front (`internal/toolvm/host.go`), the core-intercepted handlers (`internal/agent/register_*.go`), and the dispatcher (`spec/contracts/dispatcher.md`). The wire protocol (`list_tools`/`plugin_call`, R-PROTO.5), the config shape (`[[mcp.server]]`, `[plugins]`, `[tools]`), and the tool catalogs themselves are touched only where named below.
- **Depends on:** `internal/plugin` (manager), `internal/builtins`, `internal/toolvm`, `internal/agent`.
- **Follows:** R-PLUG.15 (an MCP server is a plugin, `spec/contracts/plugin.md`), `adr/plugin-http-transport-design.md`, `adr/deferred-tool-schemas.md` (toolkit exposition), `adr/tool-facilities.md` §7 (tool→tool dispatch and MCP reach rejected).

**Recommendation: MCP is the only wire protocol that crosses a process
boundary; built-in tools stay direct Go calls.** The dispatcher invokes
built-in tools as in-process function calls into `memory.Store` et al. — no
envelope, no transport, exactly as today — and reaches every tool in another
process through one MCP client (`initialize` → `tools/list` → `tools/call`):
`shell` and user plugins as MCP subprocesses (unix-socket HTTP / stdio), and
external `[[mcp.server]]` spawned directly by the manager. The custom plugin
envelope (R-PLUG.1) is retired and the `mcp` bridge builtin dissolves into the
manager, which becomes that single client. This is architecture C in §4 taken
to completion (revised 2026-11-08; the in-proc servers of the earlier
architecture B are dropped). Because built-ins never enter the MCP roster,
their names do not change, so role allowlists, eval cases, and prompts keep
naming tools literally and untouched.

---

## 1. Today: four tool tiers, three wire shapes

| Tier | Where it runs | Protocol today | Tools |
|------|---------------|----------------|-------|
| Core-intercepted | in-process dispatcher handlers (`internal/agent/register_*.go`) | none — direct function calls into `memory.Store` et al. | 46 defs in `InterceptedDefs`: `memory_*`, `skill_*`, `goal_*`, `workflow_*`, `job_*`, `queued_*`, `process_*`, `run_agent(s)`, `sql_query`, `doc_*`, `tool_list/search`, `notify_user`, `file_search_text`, `list_files`, `gap_report`, `capability_request`; `ask_human` and the generated tier (`tool_write/read/delete`, `js_eval`) register outside `InterceptedDefs` |
| Built-in plugins | subprocess `nine plugin serve <name>` (`internal/builtins`) | custom envelope R-PLUG.1: `plugin.describe`/`plugin.call`/`plugin.job_*` over HTTP on a per-plugin unix socket | `shell`, plus one `mcp` bridge instance per `[[mcp.server]]` |
| Sandboxed tools | in-process toolvm host, wazero/QuickJS (`internal/toolvm`) | internal guest ABI (host functions), not a wire protocol | shipped (`read_file` … `time`, `internal/toolvm/shipped/`), developer (`[tools].user_dir`), generated (`tool_write` rows) |
| User plugins | subprocess executable from `[plugins].user_dir` | same R-PLUG.1 envelope | operator-defined |
| External MCP servers | subprocess (stdio) or hosted (streamable HTTP) | MCP — bridged into R-PLUG.1 by the `mcp` builtin | `<server>__<tool>` (`mcp:<server>` roster row) |

Two facts frame the change. R-PLUG.15 already made an MCP server *look like* a
plugin: one bridge process per `[[mcp.server]]` translating `tools/call` into
`plugin.call`. This proposal inverts that — plugins look like MCP servers, and
the manager holds the one MCP client. Second, `go.mod` carries no MCP SDK; the
client in `internal/builtins/mcp_stdio.go`/`mcp_http.go` is hand-rolled JSON-RPC,
so the server side proposed here is hand-rolled symmetric, adding no dependency.

---

## 2. What survives standardization

MCP covers the discovery-and-call core natively; Nine's extra machinery is
client-side or host-side and moves with it, not into the wire.

| Nine machinery | In MCP? | Where it lives after |
|---|---|---|
| `tools/list` + `tools/call`, `inputSchema` | yes, natively | the one client in the manager/dispatcher |
| Content arrays (`text`/`image`/`audio`/`resource`/`resource_link`) | yes | dispatcher receives structured content; binary parts spill to files instead of the bridge's text flattening (R-PLUG.15b) |
| stdio and streamable HTTP transports | yes | two transports behind one client interface |
| Protocol version negotiation (`initialize.protocolVersion`) | yes | replaces `plugin.ProtocolVersion` for MCP-speaking servers (the bridge already negotiates 2024-11-05 / 2025-03-26) |
| `notifications/progress` with progressToken | yes | first-party servers ride it for job progress and the planned `nine:progress` rail (`adr/tool-facilities.md`, proposed) |
| `notifications/tools/list_changed` | yes | `tool_write`/`tool_delete`/`nine tools reload` notify mid-session instead of relying on `SyncSandboxed` between turns |
| `title` / `annotations` | yes | maps to `ToolDef.DisplayName`; invariant I8 (display names never reach the LLM) stays client-side filtering |
| Async jobs (R-PLUG.12: `job_id`, `job_status`, `job_cancel`, sweeper) | no | first-party: a `_meta` extension on the `tools/call` result, consumed by the dispatcher's existing `JobStarter`; third-party servers: absent, exactly as today (the bridge already reports `async_jobs: false`) |
| `max_concurrent` (R-PLUG.8) | no | a transport property: unix-socket HTTP and in-proc are concurrent, stdio is serial — one client-side write lock per stdio server, as the bridge declares today |
| `x-nine-ref` payload-by-reference (R-DISP.7) | partially (`resources/read`) | stays dispatcher-side: expand refs before `tools/call`; optionally expose the spill store as MCP resources later |
| Approval gates (HITL), role allowlists (R-ROLE.4), output cap/spill (R-DISP.2), post-call hooks (R-DISP.5) | no | dispatcher-side, in front of the client; R-DISP.1's order is unchanged |
| Capability grants (R-TVM.5/6/7) and the sandbox | no | enforced inside the toolvm host process; orthogonal to the wire protocol (§3) |

---

## 3. The hard constraint: one SQLite writer, one sandbox host

Invariant 3 (docs/architecture.md §16) makes `memory.Store` the only `*sql.DB`
owner of the store, and the daemon additionally owns the `[processes] database`
file (`sqldb.Open`, `cmd/nine/daemon.go:432`), which `spec/contracts/processes.md:253`
requires to sit apart from the store. Either way, every database handle belongs
to the daemon process. The core-intercepted tools read and write that store, so
they can be served only by an **in-process** MCP server (a loopback transport
inside the daemon) — never a subprocess, which could hold neither handle and
would need an RPC channel back to the daemon.

The same argument pins the toolvm host in-process: its host functions (workspace
confinement and SSRF gates in `ssrf.go`/`nethttp.go`, `nine:state`, `nine:sql`,
the progress rail, journal triggers) are bound to daemon internals. Moving the
host to a subprocess would require an RPC channel for every host function — a
second protocol, which is the opposite of the goal. MCP standardizes the
*transport*; the sandbox is *containment* and stays put.

`shell` needs real `exec`, so it stays a subprocess — served as MCP over the
unix-socket HTTP transport instead of the custom envelope.

---

## 4. Architectures compared

| | A — every family a stdio subprocess | B — one MCP client; in-proc servers for core + toolvm, subprocess MCP for shell and user plugins (recommended 2026-11-06, dropped 2026-11-08) | C — built-ins stay direct Go calls; every subprocess tool over MCP (**recommended**) | D — keep both protocols forever |
|---|---|---|---|---|
| One protocol on the tool path | yes | yes | no | no |
| Crash isolation | maximum | per tier: subprocess where it matters (`shell`, third-party servers), in-proc where it is already the case | as today | as today |
| Boot cost | high — a handshake per server (measured ~72s for an `npx` server) | low — in-proc servers answer instantly; one subprocess for `shell` | low | low |
| Hot path (`memory_get` …) | pays a process + JSON-RPC round trip per call | in-proc loopback, `json.RawMessage` in and out | in-process as today | in-process as today |
| toolvm host functions | need an RPC channel back to the daemon | in-process, unchanged | in-process, unchanged | in-process, unchanged |
| Single SQLite writer (invariant 3) | broken for core tools | preserved | preserved | preserved |
| Migration risk | big-bang: names, roster, fixtures churn | phased; names stable | smallest diff | none, but two clients and two version schemes are maintained forever |

A is rejected because §3 makes it unachievable for the core tier and the toolvm
host, and costly on the hottest path. D is rejected because two clients with an
"except MCP" clause in each contract is exactly what R-PLUG.15 removed. C is
the recommendation: §3 shows the in-proc MCP fronts of B add a loopback layer
without adding isolation — the same process owns the database handles either
way — so built-ins remain plain Go calls and MCP is reserved for what crosses
a process boundary. B was the recommendation on 2026-11-06; it is kept above
as the record.

---

## 5. Recommended architecture

```text
agent.Loop ── Dispatcher ── direct Go calls ──► built-in tools (register_* handlers,
                │                                   store-backed, in-process as today)
                └─ one MCP client (internal/mcp) ─┬─ unix-socket HTTP MCP    nine mcp serve shell
                                                  ├─ stdio/HTTP subprocess   user plugins, MCP-only
                                                  └─ stdio/streamable HTTP  [[mcp.server]] externals
```

- **Built-ins are plain function calls.** The core-intercepted tools keep their
  `register_*` handlers and call `memory.Store` et al. directly — no envelope,
  no transport, no marshalling on the hot path. §3 is the reason: the daemon
  owns every database handle, so this tier is in-process either way. The
  dispatcher's ref expansion, approval gate, hooks, and output cap stay in
  front of the dispatch with R-DISP.1's order unchanged, and invariant 12 (one
  tool name, one backend) is enforced at registration across both paths.
- **The manager becomes the MCP client manager for subprocess servers only.**
  Spawn → `initialize` → `tools/list` → register, per server, with its own
  connection, read loop, and write lock. The bridge's code
  (`internal/builtins/mcp*.go`) moves into `internal/mcp` as the shared
  client and the `mcp` builtin is deleted. External servers become directly
  supervised children of the daemon (today they are grandchildren behind the
  bridge, with `exitWhenServerDies` chaining); a wedged stdio read blocks only
  that server's goroutine until the existing 5-minute call timeout tears the
  connection down.
- **`nine mcp serve <name>`** — a new hidden subcommand beside `nine plugin
  serve` — speaks MCP streamable HTTP on the per-instance unix socket, reusing
  the manager's socket, sanitized env, cache dir, and `[plugins].disabled`
  machinery (R-PLUG.3/10/11/13/13a/14 all survive; only the envelope changes).
  First user: `shell`.
- **toolvm: removed (§9.10).** The shipped sandboxed tools
  (`internal/toolvm/shipped/`) are translated to Go built-ins wired directly in
  the dispatcher; `[tools].user_dir` developer tools become user plugins.
  Model-authored tools (`tool_write`/`js_eval`, generated rows) are refactored
  separately and are out of scope here.
- **User plugins:** the sidecar-manifest gate (R-PLUG.9) is unchanged; the
  executable must speak MCP (stdio, or HTTP on the socket provided). During
  migration the manager dual-stacks — MCP handshake first, legacy envelope as
  a deprecated fallback — then the envelope is removed and `nine plugin
  validate` accepts only MCP.
- **Naming (decided, §9.9):** built-ins keep their bare names (`memory_get`,
  `read_file`); only servers on the MCP roster carry `<server>__<tool>`, which
  is today's convention for external MCP servers. No first-party tool is
  renamed, so role allowlists, eval cases, skills, prompts, `[context]
  resident_tools`, and the toolkit table are untouched.
- **Wire protocol:** `list_tools`/`plugin_call` (R-PROTO.5) are unchanged — they
  address the dispatcher, which splits: direct calls for built-ins, the MCP
  client for everything in another process.

---

## 6. The other direction: Nine as an MCP server

The same in-proc server adapters can be mounted for external clients: a
`nine mcp export` stdio command, or an `/mcp` endpoint on the API server,
adapting the daemon's `list_tools`/`plugin_call` (R-PROTO.5). One adapter code
serves both directions — in-proc front for the dispatcher, mounted surface for
outside clients. Sampling and elicitation (server→client requests) stay
disabled: server→agent-LLM reach is the tool→tool dispatch that
`adr/tool-facilities.md` §7 rejects, and an exported surface must authenticate
and vet the same way the API does, because Nine does not vet a server's arguments
(R-PLUG.15b).

---

## 7. What the model sees

Schemas, descriptions, exposition (ranked/deferred/hybrid,
`adr/deferred-tool-schemas.md`), ref expansion, spill, and approval gates are
unchanged — all dispatcher-side. Tool **names** do not change (§9.9): built-ins
keep their bare names and remote tools keep `<server>__<tool>`, as today.
Optional later: `structuredContent` for `shell`'s `{stdout, stderr, exit_code}`
instead of a JSON-encoded string in the text part.

---

## 8. Migration phases

P0, P2 and P3 are dropped (revised 2026-11-08, §9.11): no in-proc MCP servers are
built, so there is no loopback transport, no toolvm front and no core front.

| Phase | Work | Gate |
|---|---|---|
| P1 | `internal/mcp`: hand-rolled MCP client (moved from `internal/builtins`) + symmetric server over real transports, no new dependency; `nine mcp serve shell`; manager dual-stack (MCP first, legacy fallback); `shell` served over MCP | unit tests against `internal/builtins/testmcpserver`; `go test ./...`; evals green; `nine plugins` roster unchanged |
| P4 | user plugins speak MCP; legacy envelope removed; `nine plugin validate` MCP-only; `spec/contracts/plugin.md` rewritten as "plugins are MCP servers". toolvm removed; shipped tools translated to Go built-ins (§9.10) | `userplugins` + plugin contract suites; toolvm suites deleted, built-in tool coverage moves to `register_*_test.go` |
| P5 | external `[[mcp.server]]` spawned directly by the manager; bridge builtin deleted; roster and `[plugins].disabled` semantics unchanged | `mcp_test.go`, `mcp_http_test.go`, evals incl. `deferred-mcp-discovery` |
| P6 | export direction (in scope, §9.2): `nine mcp export` / API `/mcp` over the same adapters (§6) | OpenAPI drift checks, API tests |

---

## 9. Decisions and remaining open questions

Answered by the requester on 2026-11-06:

1. **Scope — all tiers, core included.** The core-intercepted tools are served
   by the in-proc `core` server (P3); a subprocess is impossible because the
   daemon owns every database handle (§3). (Superseded by item 8, 2026-11-08:
   they stay direct Go calls.)
2. **Direction — both.** The dispatcher consumes only MCP, and Nine also
   exposes its own tools to external MCP clients (`nine mcp export` / API
   `/mcp`, §6); P6 is in scope, not optional.
3. **Toolvm — the sandbox is fronted by MCP.** The host stays in-process behind
   the `tools` front; capability grants (R-TVM.5/6/7) are unchanged.
   (Superseded by item 10, 2026-11-08: the toolvm is removed outright.)
4. **Naming — prefix everything.** Uniform `<server>__<tool>` for every
   server, first-party included (`shell__shell`, `tools__read_file`,
   `core__memory_get`). This reverses §5's earlier bare-names leaning: every
   first-party tool is renamed, and the ripple (roles, evals, skills, prompts,
   `resident_tools`, toolkit table) is part of the migration, not a separate
   decision. See §7 and §10.
   (Reversed by item 9, 2026-11-08: no first-party tool is renamed.)

Answered by the requester on 2026-11-08:

8. **Architecture — built-ins stay direct Go calls.** No in-proc MCP front for
   core or toolvm: the daemon owns every database handle (§3), so those tiers
   are in-process either way, and a loopback MCP layer adds complexity without
   isolation. MCP is the only wire protocol across process boundaries —
   architecture C taken to completion (§5 rewritten).
9. **Naming — no renames.** Built-ins keep bare names (`memory_get`,
   `read_file`); only MCP-roster servers carry `<server>__<tool>`, today's
   convention. Reverses item 4, so the §7/§10 ripple disappears and the
   behaviour-parity gate has no sanctioned differences.
10. **toolvm — removed.** Shipped sandboxed tools become Go built-ins wired
    directly in the dispatcher; `[tools].user_dir` tools become user plugins.
    Model-authored tools (`tool_write`/`js_eval`/generated rows) are refactored
    separately, out of scope here; the generated-tools eval track is reworked
    in the same PR that lands the removal.
11. **Phasing — P1/P4/P5/P6 only.** P0/P2/P3 dropped (§8); phased dual-stack
    remains. Answers open question 7.

Still open:

5. **SDK** — hand-roll the MCP server symmetric to the existing client
   (recommended; `go.mod` vendors everything and carries no MCP SDK today), or
   vendor `mcp-go`?
6. **First-party subprocess transport** — unix-socket HTTP (reuses R-PLUG.8
   concurrency machinery; recommended) or stdio (simpler, serial, fine for
   `shell` alone)?

---

## 10. Test plan

Phases follow §8. Harness rules: daemon tests use `startDaemon`/`dial`/`seqProvider`
(`internal/runtime/daemon_test.go`) with sockets in `/tmp` (macOS caps socket
paths at 104 bytes); a plugin process that dies before listening is a real
failure, not a timeout (30s readiness budget); the evals runner deadlocks
intermittently and is excluded from `make ci`, so it is run explicitly per
phase; Playwright end-to-end only under `NINE_PLAYWRIGHT_TEST=1`; the pre-merge
gate is `make ci` (plus `make scan` for the image). At the requester's direction
(2026-11-06) the evals are a behaviour-parity gate at every phase, not a
one-off check — see "Behaviour parity" below.

Revised 2026-11-08: the P0, P2 and P3 rows below are dropped with those
phases; P1 covers the client and server over real transports (no in-proc
transport).

| Phase | Unit | Integration | Gate |
|---|---|---|---|
| P0 `internal/mcp` | JSON-RPC framing on the in-proc and unix-socket HTTP transports; request-id correlation; `isError`→error mapping; content-array flattening parity with `flattenMCPContent` (port `mcp_content_test.go`); protocol-version matrix (2024-11-05 stdio, 2025-03-26 HTTP); `NINE_MCP_HANDSHAKE_TIMEOUT` parse and fallback | against `internal/builtins/testmcpserver` over stdio and HTTP; ctx cancel tears down a blocked stdio read | `go test ./internal/mcp/` |
| P1 `nine mcp serve shell`, dual-stack | shell over MCP: destructive-command guard (`shell_security_test`), `NINE_WORKSPACE` cwd, per-call timeout; legacy-envelope plugin still loads with a deprecation warning; `nine plugin validate` accepts both | manager dual-stack (MCP first, legacy fallback); `[plugins].disabled` blocks both start paths; roster unchanged; tool-name collision skips the whole plugin (R-PLUG.9); `list_tools`/`plugin_call` wire tests unchanged | `go test ./internal/plugin/ ./internal/builtins/`; evals runner run explicitly |
| P2 in-proc `tools` | every shipped tool callable through the client; grants still enforced (`capability_test`, `ssrf_test`); bounds (`timeout_test`, `workbudget_test`); `x-nine-ref` expanded before `tools/call` (`refs_test`); spill and cap unchanged (`spill_test`) | `tool_write`/`tool_delete`/`nine tools reload` emit `tools/list_changed`; a written tool is callable from the next turn (`sync_sandboxed_test` plus a new notification test) | toolvm suites; evals incl. `deferred-mcp-discovery` |
| P3 in-proc `core` | all 46 `InterceptedDefs` reachable via the client; approval gate fires pre-call with the original args (`approval_test`); `RestrictTo` enforced (`restrict_test`); hooks fire (`hooks_test`); unknown-tool + did-you-mean (`dispatcher_test`) | benchmark `memory_get` over the loopback vs the direct handler — validates §5's "json.RawMessage only" cost claim | dispatcher + `register_*_test.go` suites |
| P4 user plugins MCP-only | manifest gate: no manifest → binary never executed; malformed manifest or non-MCP binary skipped while others load (fail-soft); `Probe`/`validate` | `nine plugins reload` starts/stops only user plugins; in-flight turns keep their tool set | `userplugins_test.go`, `transport_test.go`, `startup_test.go` |
| P5 manager spawns `[[mcp.server]]` directly | stdio spawn + handshake; `<server>__<tool>` prefixing unchanged (`mcp_test.go`); server death marks the plugin down and the next call errors (replaces the `exitWhenServerDies` chain); handshake-timeout test via the env override; listen-first ordering preserved | hosted streamable HTTP (`mcp_http_test.go`); N servers start concurrently and one dead server does not block boot; `disabled = ["mcp:github"]` | `mcp_*_test.go`; `mcp_playwright_test.go` only under `NINE_PLAYWRIGHT_TEST=1`; evals |
| P6 (optional) export | `nine mcp export` stdio: `tools/list` equals the daemon's `list_tools`; `tools/call` proxies `plugin_call`; sampling/elicitation refused | API `/mcp` behind the same authentication as REST | API tests; `make ci` OpenAPI drift + lint |

### Behaviour parity

Every phase runs the eval tracks of `docs/evals.md` and compares **per case**
against a baseline captured on `main` before P0 starts:

- `make eval-replay` — deterministic and infra-free (`TestCasesValidate` +
  `TestReplayFixtures` in `./tests/evals/`; already `make ci`'s eval gate) —
  must be identical at every phase: same cases pass, same fail, nothing new
  skipped.
- `make eval-live` — the live model matrix (`NINE_EVAL_MODELS`, needs
  `make build`; `TestLiveMatrix` in `./tests/evals/`) — run at every phase that
  renames or reroutes tools; per-case outcomes must match the baseline. The
  runner starts a real in-process daemon per test and deadlocks intermittently,
  which is why it is outside `make ci`: rerun once on a deadlock before treating
  a failure as real.
- There is no sanctioned difference (renames reversed, §9.9): any per-case
  difference — a new failure, a new pass, a new skip — is a behaviour change
  and blocks the phase until explained. (The 2026-11-06 version allowed a
  rename through per-phase old→new mapping tables.)

Cross-cutting, at every phase:

- **Catalog stability** — snapshot the full catalog (names + schemas) before and
  after each phase; nothing may change (§9.9). Role allowlists, eval cases,
  skills, prompts, `[context] resident_tools`, and the toolkit table
  (`internal/runtime/exposition.go`) are untouched.
- **Conformance** — each new or rewritten rule gets a row in
  `spec/conformance.md` (R-PLUG.1 as "plugins are MCP servers", the `_meta`
  jobs extension, the in-proc transport).
- **Boot order** — MCP servers start concurrently and a dead server never
  blocks boot; regression-test both.
- **Gate** — `go build ./...`, `go test ./...`, the behaviour-parity check
  above (recorded pass counts also live in `adr/deferred-tool-schemas.md`),
  then `make ci` before merge.

---

## Limits

| Limit | Detail |
|-------|--------|
| Nothing here is built | This is an analysis and a recommendation; no phase of §8 has started. |
| MCP has no jobs or concurrency hint | First-party servers carry jobs in a `_meta` extension and concurrency stays a transport property; third-party servers keep today's `async_jobs: false`, serial-stdio behavior. |
| stdio stays serial | One client-side write lock per stdio server; the 3-minute handshake budget and 5-minute call timeout move with the client into the manager. |
| Third-party server trust is unchanged | Declaring an MCP server is trusting it (R-PLUG.15b); standardization does not vet a server's arguments. |
| Export direction adds a trust surface | An external `/mcp` endpoint needs the API's authentication, and server→client sampling/elicitation stay disabled (adr/tool-facilities.md §7). |
| First-party tool names do not change (decided, §9.9) | Built-ins keep bare names; only MCP-roster servers carry `<server>__<tool>`, today's convention (reverses §9.4). Role allowlists, eval cases, skills, prompts, `resident_tools`, and the toolkit table are untouched. |
| Tool→tool dispatch stays rejected | With the toolvm removed (§9.10) there is no guest ABI; no dispatch verb is added. |
| Built-ins are in-process by design | Core tools call the daemon's store directly (§3); process isolation applies only to subprocess tools (`shell`, plugins, external servers). |
| Model-authored tools are out of scope | `tool_write`/`js_eval`/generated tools are refactored separately (§9.10); the generated-tools eval track is reworked in the same PR that lands the removal. |
