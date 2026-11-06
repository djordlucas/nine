# Modular Nine — all tooling over MCP

- **Status:** Proposed (architecture analysis). Nothing here is built.
- **Date:** 2026-11-01.
- **Reviewed:** 2026-11-06 — fact-checked against the tree; §1 tool count and names, §2 progress-rail wording, §3 database ownership, §5 supervision wording corrected; test plan added as §10.
- **Scope:** the daemon's tool-serving path — the plugin contract (`spec/contracts/plugin.md`), the `mcp` bridge (`internal/builtins/mcp.go`, `mcp_stdio.go`, `mcp_http.go`), the toolvm host front (`internal/toolvm/host.go`), the core-intercepted handlers (`internal/agent/register_*.go`), and the dispatcher (`spec/contracts/dispatcher.md`). The wire protocol (`list_tools`/`plugin_call`, R-PROTO.5), the config shape (`[[mcp.server]]`, `[plugins]`, `[tools]`), and the tool catalogs themselves are touched only where named below.
- **Depends on:** `internal/plugin` (manager), `internal/builtins`, `internal/toolvm`, `internal/agent`.
- **Follows:** R-PLUG.15 (an MCP server is a plugin, `spec/contracts/plugin.md`), `adr/plugin-http-transport-design.md`, `adr/deferred-tool-schemas.md` (toolkit exposition), `adr/tool-facilities.md` §7 (tool→tool dispatch and MCP reach rejected).

**Recommendation: make MCP the only tool protocol on the daemon's tool path.**
The dispatcher reaches every tool through one MCP client (`initialize` →
`tools/list` → `tools/call`), and every first-party tool is served by an MCP
server built into the same `nine` binary — in-process for the store-backed core
tools and the sandboxed-tool host, a subprocess over the existing unix-socket
HTTP transport for `shell`. The custom plugin envelope (R-PLUG.1) is retired and
the `mcp` bridge builtin dissolves into the manager, which becomes that single
client. Architecture B in §4 is the recommendation; §8 phases it so tool names
— which role allowlists, eval cases, and prompts name literally — never change.

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

| | A — every family a stdio subprocess | B — one MCP client; in-proc servers for core + toolvm, subprocess MCP for shell and user plugins (**recommended**) | C — swap the envelope only (MCP over unix-socket HTTP for subprocesses; core/toolvm untouched) | D — keep both protocols forever |
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
not "all tooling over MCP" but is retained as phase P1 of §8, since it is the
smallest first step.

---

## 5. Recommended architecture (B)

```text
agent.Loop ── Dispatcher ── one MCP client (internal/mcp)
                                   ├─ in-proc  nine:core    InterceptedDefs + register_* handlers, store-backed
                                   ├─ in-proc  nine:tools   toolvm.Host front: Tools()→tools/list, CallOutput→tools/call
                                   ├─ unix-socket HTTP MCP   nine mcp serve shell   (subprocess, same binary)
                                   ├─ stdio subprocess       [[mcp.server]] command = …   (spawned by the manager directly)
                                   └─ streamable HTTP        [[mcp.server]] url = …
```

- **The manager becomes the MCP client manager.** Spawn → `initialize` →
  `tools/list` → register, per server, with its own connection, read loop, and
  write lock. The bridge's code (`internal/builtins/mcp*.go`) moves into
  `internal/mcp` as the shared client and the `mcp` builtin is deleted. The
  bridge's process-per-server disappears for external servers: the server
  process becomes a directly supervised child of the daemon (today it is a
  grandchild behind the bridge, with `exitWhenServerDies` chaining its death to
  the bridge's exit), so crash isolation of the *server* is unchanged, and a
  wedged stdio read blocks only that server's goroutine until the existing
  5-minute call timeout tears the connection down (the failure mode R-PLUG.15's
  process boundary guarded against).
- **`nine mcp serve <name>`** — a new hidden subcommand beside `nine plugin
  serve` — speaks MCP streamable HTTP on the per-instance unix socket, reusing
  the manager's socket, sanitized env, cache dir, and `[plugins].disabled`
  machinery (R-PLUG.3/10/11/13/13a/14 all survive; only the envelope changes).
  First user: `shell`.
- **toolvm:** `Host` gains a `ServeMCP` adapter (`Tools()` → `tools/list`,
  `CallOutput` → `tools/call`). Capability resolution, bounds, and audit
  (R-TVM.4/5/6/7/12) are untouched — transport only. `tool_write`/`tool_delete`
  and `nine tools reload` emit `tools/list_changed`.
- **Core tools:** an in-proc server over the existing `register_*` handlers.
  The dispatcher's ref expansion, approval gate, hooks, and output cap stay in
  front of the client; R-DISP.1's order is unchanged. The loopback transport
  passes `json.RawMessage` without a socket, so the hot path pays JSON
  marshalling only.
- **User plugins:** the sidecar-manifest gate (R-PLUG.9) is unchanged; the
  executable must speak MCP (stdio, or HTTP on the socket provided). During
  migration the manager dual-stacks — MCP handshake first, legacy envelope as
  a deprecated fallback — then the envelope is removed and `nine plugin
  validate` accepts only MCP.
- **Naming:** first-party servers keep bare tool names (the R-PLUG.9 collision
  rules already reserve them); third-party `[[mcp.server]]` tools keep the
  `<server>__<tool>` prefix. The deferred index's toolkits become uniform:
  `mcp/<server>` for external servers, named toolkits for first-party ones.
  (Open question 4.)
- **Wire protocol:** `list_tools`/`plugin_call` (R-PROTO.5) are unchanged — they
  address the dispatcher, which now fans out over MCP.

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

Nothing changes: same tool names, schemas, and descriptions; exposition
(ranked/deferred/hybrid, `adr/deferred-tool-schemas.md`), ref expansion, spill,
and approval gates are all dispatcher-side and untouched. Optional later:
`structuredContent` for `shell`'s `{stdout, stderr, exit_code}` instead of a
JSON-encoded string in the text part.

---

## 8. Migration phases

| Phase | Work | Gate |
|---|---|---|
| P0 | `internal/mcp`: hand-rolled MCP client (moved from `internal/builtins`) + symmetric server + in-proc transport; no new dependency | unit tests against `internal/builtins/testmcpserver` |
| P1 | `nine mcp serve shell`; manager dual-stack (MCP first, legacy fallback); `shell` served over MCP (architecture C as the first step) | `go test ./...`; evals green; `nine plugins` roster unchanged |
| P2 | in-proc `nine:tools` front over `toolvm.Host`; sandboxed tools registered through the client; `tools/list_changed` on write/delete/reload | toolvm + `sync_sandboxed_test.go` suites |
| P3 | in-proc `nine:core` server; core tools registered through the client; refs/approval/spill stay in front | dispatcher + `register_*_test.go` suites |
| P4 | user plugins speak MCP; legacy envelope removed; `nine plugin validate` MCP-only; `spec/contracts/plugin.md` rewritten as "plugins are MCP servers" | `userplugins` + plugin contract suites |
| P5 | external `[[mcp.server]]` spawned directly by the manager; bridge builtin deleted; roster and `[plugins].disabled` semantics unchanged | `mcp_test.go`, `mcp_http_test.go`, evals incl. `deferred-mcp-discovery` |
| P6 (optional) | export direction: `nine mcp export` / API `/mcp` over the same adapters (§6) | OpenAPI drift checks, API tests |

---

## 9. Open questions

1. **Scope of "all tooling"** — are the core-intercepted tools in scope (served
   by the in-proc `nine:core` server), or only the provider tiers (`shell`,
   sandboxed, user plugins, MCP)? Asked of the requester.
2. **Direction** — consume-only (architecture B), or also expose Nine's own
   tools to external MCP clients (§6)? Asked of the requester.
3. **Toolvm fate** — front the existing host with MCP (recommended; keeps the
   capability model), or retire the sandbox and run JS tools as their own MCP
   server processes (loses R-TVM.5/6/7 enforcement)? Asked of the requester.
4. **Naming** — keep `<server>__<tool>` for every server uniformly, or bare
   names for first-party servers with prefixes only for third-party? Asked of
   the requester; §5 recommends the latter because names are load-bearing.
5. **SDK** — hand-roll the MCP server symmetric to the existing client
   (recommended; `go.mod` vendors everything and carries no MCP SDK today), or
   vendor `mcp-go`?
6. **First-party subprocess transport** — unix-socket HTTP (reuses R-PLUG.8
   concurrency machinery; recommended) or stdio (simpler, serial, fine for
   `shell` alone)?
7. **Phasing** — phased dual-stack (§8, recommended) or big-bang? Role
   allowlists, eval cases, and prompts name tools literally, which argues for
   phased.

---

## 10. Test plan

Phases follow §8. Harness rules: daemon tests use `startDaemon`/`dial`/`seqProvider`
(`internal/runtime/daemon_test.go`) with sockets in `/tmp` (macOS caps socket
paths at 104 bytes); a plugin process that dies before listening is a real
failure, not a timeout (30s readiness budget); the evals runner deadlocks
intermittently and is excluded from `make ci`, so it is run explicitly per
phase; Playwright end-to-end only under `NINE_PLAYWRIGHT_TEST=1`; the pre-merge
gate is `make ci` (plus `make scan` for the image).

| Phase | Unit | Integration | Gate |
|---|---|---|---|
| P0 `internal/mcp` | JSON-RPC framing on the in-proc and unix-socket HTTP transports; request-id correlation; `isError`→error mapping; content-array flattening parity with `flattenMCPContent` (port `mcp_content_test.go`); protocol-version matrix (2024-11-05 stdio, 2025-03-26 HTTP); `NINE_MCP_HANDSHAKE_TIMEOUT` parse and fallback | against `internal/builtins/testmcpserver` over stdio and HTTP; ctx cancel tears down a blocked stdio read | `go test ./internal/mcp/` |
| P1 `nine mcp serve shell`, dual-stack | shell over MCP: destructive-command guard (`shell_security_test`), `NINE_WORKSPACE` cwd, per-call timeout; legacy-envelope plugin still loads with a deprecation warning; `nine plugin validate` accepts both | manager dual-stack (MCP first, legacy fallback); `[plugins].disabled` blocks both start paths; roster unchanged; tool-name collision skips the whole plugin (R-PLUG.9); `list_tools`/`plugin_call` wire tests unchanged | `go test ./internal/plugin/ ./internal/builtins/`; evals runner run explicitly |
| P2 in-proc `nine:tools` | every shipped tool callable through the client; grants still enforced (`capability_test`, `ssrf_test`); bounds (`timeout_test`, `workbudget_test`); `x-nine-ref` expanded before `tools/call` (`refs_test`); spill and cap unchanged (`spill_test`) | `tool_write`/`tool_delete`/`nine tools reload` emit `tools/list_changed`; a written tool is callable from the next turn (`sync_sandboxed_test` plus a new notification test) | toolvm suites; evals incl. `deferred-mcp-discovery` |
| P3 in-proc `nine:core` | all 46 `InterceptedDefs` reachable via the client; approval gate fires pre-call with the original args (`approval_test`); `RestrictTo` enforced (`restrict_test`); hooks fire (`hooks_test`); unknown-tool + did-you-mean (`dispatcher_test`) | benchmark `memory_get` over the loopback vs the direct handler — validates §5's "json.RawMessage only" cost claim | dispatcher + `register_*_test.go` suites |
| P4 user plugins MCP-only | manifest gate: no manifest → binary never executed; malformed manifest or non-MCP binary skipped while others load (fail-soft); `Probe`/`validate` | `nine plugins reload` starts/stops only user plugins; in-flight turns keep their tool set | `userplugins_test.go`, `transport_test.go`, `startup_test.go` |
| P5 manager spawns `[[mcp.server]]` directly | stdio spawn + handshake; `<server>__<tool>` prefixing unchanged (`mcp_test.go`); server death marks the plugin down and the next call errors (replaces the `exitWhenServerDies` chain); handshake-timeout test via the env override; listen-first ordering preserved | hosted streamable HTTP (`mcp_http_test.go`); N servers start concurrently and one dead server does not block boot; `disabled = ["mcp:github"]` | `mcp_*_test.go`; `mcp_playwright_test.go` only under `NINE_PLAYWRIGHT_TEST=1`; evals |
| P6 (optional) export | `nine mcp export` stdio: `tools/list` equals the daemon's `list_tools`; `tools/call` proxies `plugin_call`; sampling/elicitation refused | API `/mcp` behind the same authentication as REST | API tests; `make ci` OpenAPI drift + lint |

Cross-cutting, at every phase:

- **Name stability** — snapshot the full catalog (names + schemas) before and
  after; it must be byte-identical, which is what keeps role allowlists, eval
  cases, skills, and prompts working (§9 q4).
- **Conformance** — each new or rewritten rule gets a row in
  `spec/conformance.md` (R-PLUG.1 as "plugins are MCP servers", the `_meta`
  jobs extension, the in-proc transport).
- **Boot order** — MCP servers start concurrently and a dead server never
  blocks boot; regression-test both.
- **Gate** — `go build ./...`, `go test ./...`, an explicit `tests/evals/runner`
  run compared against the baselines in `adr/deferred-tool-schemas.md`, then
  `make ci` before merge.

---

## Limits

| Limit | Detail |
|-------|--------|
| Nothing here is built | This is an analysis and a recommendation; no phase of §8 has started. |
| MCP has no jobs or concurrency hint | First-party servers carry jobs in a `_meta` extension and concurrency stays a transport property; third-party servers keep today's `async_jobs: false`, serial-stdio behavior. |
| stdio stays serial | One client-side write lock per stdio server; the 3-minute handshake budget and 5-minute call timeout move with the client into the manager. |
| Third-party server trust is unchanged | Declaring an MCP server is trusting it (R-PLUG.15b); standardization does not vet a server's arguments. |
| Export direction adds a trust surface | An external `/mcp` endpoint needs the API's authentication, and server→client sampling/elicitation stay disabled (adr/tool-facilities.md §7). |
| Names must not change during migration | Role allowlists, eval cases (`deferred-mcp-discovery` and the `mcp/fixture` toolkit), skills, and prompts reference tool names literally; renaming is a separate, larger decision (open question 4). |
| Tool→tool dispatch stays rejected | An in-proc MCP front for toolvm does not give sandboxed tools MCP reach; the guest ABI has no dispatch verb and none is added. |
| In-proc servers lose process isolation for core/toolvm | They never had it — both tiers are already in-process; §3 only forbids moving them out. |
