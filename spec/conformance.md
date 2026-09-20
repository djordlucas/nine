# Nine — conformance checklist

This is the acceptance checklist: every numbered requirement from a **`Built`**
contract in [`contracts/`](contracts/), mapped to an **observable** test. An
implementation "is Nine" when it passes every row below.

Two contracts are **not** `Built` and so have no rows here:
[`agent-policy.md`](contracts/agent-policy.md) is `Planned` and
[`api.md`](contracts/api.md) is `Design`. A requirement with no implementation has no
observable check. That is the only permitted reason for a requirement to be absent — if a
`Built` contract's requirement has no row, the omission is a defect in this file, not a
statement about the requirement.

How to use this file:

- Each row names a requirement ID, the property, and a concrete check you can run or
  observe. The check is the conformance test; the contract is the normative text.
- IDs are stable (`R-<area>.<n>`). Do not renumber; append.
- The **invariants** (I1–I11) and **non-goals** (N1–N5) from
  [`overview.md`](overview.md) are cross-cutting; § 0 below maps each to the requirement
  rows that enforce it, so they can be checked as a set.

---

## 0. Invariants and non-goals → where they are enforced

| ID | Property | Enforced / checked by |
|----|----------|------------------------|
| I1 | One session, one serial worker, serialized turns | R-WORK.1; R-LOOP.1 |
| I2 | LLM reachable only through the queue | R-LLM.3, R-LLM.4; R-LOOP.8 |
| I3 | One database gateway; services depend on narrow repos | R-MEM.1; R-ORCH.5 |
| I4 | Operational tables are daemon-private (not tools) | R-MEM.4; R-HITL.7 |
| I5 | Every turn ends with a checkpoint | R-LOOP.7; R-WORK.2; R-MEM.5 |
| I6 | Sub-agent recursion is depth-capped | R-DISP.6; R-ORCH.3; R-ROLE.6 |
| I7 | Background autonomy is resumable | R-PLAN.3, R-PLAN.5 |
| I8 | Display names never reach the LLM | R-PROTO.6 |
| I9 | Plugins are isolation boundaries | R-PLUG.4 |
| I10 | Self-improvement is data, not code | R-SKILL.5; R-ROLE.7 |
| I11 | Journal is append-only; reactions are out-of-band | R-EVT.1; R-SUB.3 |
| N1 | No runtime plugin generation/compile/hot-swap | R-PLUG.7; R-SKILL.5 |
| N2 | No runtime config rewrite | R-CFG.3; R-SKILL.5 |
| N3 | No self-rebuild from source | R-SKILL.5 |
| N4 | No tool access to daemon-private state | R-MEM.4 |
| N5 | No hosted LLM API required to function | R-LLM.1; R-LLM.7 |

---

## 1. Foundations (phase 0)

### Config — [`config.md`](contracts/config.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-CFG.1 | Resolution order | Set `$NINE_CONFIG`; it wins over `./nine.toml`, `/nine.toml`, `~/.nine/nine.toml`. With none present, defaults load. |
| R-CFG.2 | Sections and fields | Every documented section/field parses; unknown keys do not crash; omitted fields take the spec defaults. |
| R-CFG.3 | Operator-set, applied at start (N2) | Config is read at boot only; no code path rewrites `nine.toml`; changing it has no effect until restart. |
| R-CFG.4 | Volume / filesystem layout | DB, skills, and plugin binaries resolve under the configured paths. |
| R-CFG.5 | Logging & environment | Log level/destination honor config/env; documented env vars take effect. |

### Memory store — [`memory-store.md`](contracts/memory-store.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-MEM.1 | Single gateway (I3) | One store object holds the only DB handle; no other component opens the DB. |
| R-MEM.2 | Exact schema | Fresh DB creates exactly the specified tables; re-open is idempotent. |
| R-MEM.3 | Agent-facing methods | K/V, file, and vector methods round-trip; these are the only store methods exposed as tools. |
| R-MEM.4 | Daemon-private methods (I4, N4) | `conversations`/`goals`/`workflows`/`notifications`/`session_plans`/HITL tables are reachable only via internal methods, never as tools. |
| R-MEM.5 | Checkpoints | `{history, scratchpad}` saves and reloads exactly; reload reconstructs session state. |
| R-MEM.6 | Vectors and namespaces | Put/query under `tools:`/`skills:` namespaces returns ranked matches scoped to the namespace. |
| R-MEM.7 | JSON convenience variants | JSON set/get variants marshal/unmarshal symmetrically. |
| R-MEM.8 | Memory surfacing (KV pull-surfacing) | `memory_set` also mirrors the value into the `memories` vector pool and `memory_delete` removes it, both best-effort — an embed failure never fails the KV write; once per turn the builder injects up to N over a similarity floor as advisory enrichment that yields under budget pressure. |
| R-MEM.9 | The `spill/` namespace is daemon-owned | `file_store` refuses a path under `spill/` while it stays agent-readable; nothing under `spill/` enters the vector pool; spills are swept on a retention window. |
| R-MEM.10 | Schema versioning and migration | `user_version` tracks the schema generation and equals the step count; each step and its bump commit atomically (a failing step leaves the version untouched); a fresh database is stamped without running steps; a database newer than the binary is refused, not rewritten. |

### Sandboxed tools — [`toolvm.md`](contracts/toolvm.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-TVM.1 | ABI | A tool module exports exactly `nine_alloc` and the call entrypoint at `ABIVersion` 1; a module missing either is refused at load. |
| R-TVM.2 | Two module kinds | A `wasm` tool is the developer's own module; a `js` tool runs the pre-supplied QuickJS-NG interpreter over its source. Nothing is compiled at install time. |
| R-TVM.3 | Compile once, instantiate per call | The module is compiled once and a fresh instance is created per call and closed when it returns; no state survives between calls of the same tool. |
| R-TVM.4 | Resource bounds | Each call is bounded by wall clock (default 5s, per-tool overridable), memory pages (default 16 MiB), and is torn down when the context is done. |
| R-TVM.5 | Default-empty capabilities | With no grant, a tool has no filesystem pre-open and no host functions: a denied capability is **structurally absent**, not refused at call time — there is no function to call. |
| R-TVM.6 | Declaration ≠ grant | A manifest declares a need; only `nine.toml` grants. A capability declared but not granted is absent, **and one granted but not declared is refused** — the check runs both ways. |
| R-TVM.7 | Grant table shape | Grants live in a singular `[tool.<name>]` table beside the plural `[tools]` subsystem table, mirroring R-PLUG.10. |
| R-TVM.8 | Imports resolve in the host | Module resolution happens host-side against a closed allowlist **before instantiation**; the guest never receives a resolver, so `import` cannot reach disk or network. |
| R-TVM.9 | Trimmed interpreter surface | The QuickJS `std` and `os` modules are not linked: no filesystem API, no `os.exec`, no `std.urlGet`, and no `std.evalScript`/`loadScript` eval hooks are reachable from a tool. The granted surface `nine:fs` exposes includes a ranged read, an append, a rename and a non-recursive remove, so a file larger than a call's memory cap is reachable and a write can be replaced atomically. |
| R-TVM.10 | Loading | Developer tools load from `[tools].user_dir` in the same sidecar-manifest layout as user plugins (R-PLUG.9); a module without its manifest does not load. |
| R-TVM.11 | Visibility and reload | A newly loaded tool appears to **subsequently built** loops only; a turn in flight keeps the tool set it started with, so the turn stays replayable. |
| R-TVM.12 | `net.http` | Network access is a host function the host enforces: the SSRF checklist applies to every request. `allow_hosts` may be a bare `*`, which grants any host and no additional address — loopback, link-local, private and multicast stay refused at dial time regardless. |
| R-TVM.13 | Fully built | Every feature the design specifies is implemented — the `nine:*` stdlib, external dependencies, and the deps/`net.http` interlock — with nothing stubbed or refused by name. |
| R-TVM.14 | Generated tools | A tool Nine wrote via `tool_write` is a row in `tools` carrying source, schema, and a capability **declaration with no grant**; it runs through the same host, ABI, and instance model, and is capped by the generated ceiling. |
| R-TVM.15 | Dependencies and the `nine:*` stdlib | Both `nine:*` and external npm imports resolve **before** the call, never by the guest and never at call time. |
| R-TVM.16 | The shipped tier | First-party tools compiled into the binary run through the same host, ABI, and bounds; they are granted what they declare, that grant is visible in the roster, a declaration the host cannot enforce is refused, and they load before the other tiers so neither can take their names. An fs tool's workspace is created if absent, and its `path` argument accepts the workspace's host path as well as its guest mount. Removing or replacing a workspace file keeps the previous contents under `.nine/trash/`, which is agent-unwritable, excluded from the operator's version control, restorable, and bounded by both age (taken from the entry, not the file's mtime) and total size. A change is renderable as a diff before it happens (`preview`), inside the approval prompt, and afterwards against the trashed version; the prompt reuses the tool's own preview, survives a preview failure, and is bounded without rendering an oversized change as empty. |
| R-TVM.17 | `fs.write` includes directory creation | A tool granted `fs.write` can create a directory and its missing parents, recursively and idempotently; creation is confined to the mount by the pre-open, and a tool without `fs.write` is refused. |

### Wire protocol — [`wire-protocol.md`](contracts/wire-protocol.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-PROTO.1 | Envelope | Every `Msg` type marshals/unmarshals over newline-delimited JSON framing. |
| R-PROTO.2 | Client → daemon messages | Each request type (incl. `new_conversation` `Interactive`) is parsed and routed by type. |
| R-PROTO.3 | Daemon → client messages | `tool_start`/`tool_end`/`context_update`/`response_chunk`/`response`/`done` stream in order. |
| R-PROTO.4 | Read payloads | Read-only queries (status, lists) return their documented payload shapes. |
| R-PROTO.5 | `plugin_call` bypasses the LLM | A `plugin_call` runs the tool directly with no completion request. |
| R-PROTO.6 | Display names never reach the LLM (I8) | The model receives canonical tool names; display names appear only in client-facing events. |
| R-PROTO.7 | `EnsureDaemon` / auto-start | With a dead socket, a client auto-starts the daemon (re-exec) and connects. |
| R-PROTO.8 | Connection & concurrency | Each connection handled concurrently and independently; concurrent clients are served in parallel. |
| R-PROTO.9 | Every client message type is dispatched | Each member of the documented client-sendable set reaches a handler; none falls to the dispatcher's `unknown message type` default. |
| R-PROTO.10 | A reply echoes its request's type | Every reply-reading client method rejects a non-`error` reply of the wrong type instead of returning a zero value; a wrong-typed reply produces an error naming both types. |
| R-PROTO.11 | Required fields checked on ingress | A client message missing a field its type requires is rejected before routing, with the type and missing field named; a type with no requirements passes. |
| R-PROTO.12 | Requests decode to per-message types | The dispatcher routes on a decoded request type, not the envelope; no handler reads a field belonging to another message; decoding validates and rejects unroutable types, so the router has no unknown branch. |

---

## 2. LLM, embeddings, plugins (phases 1–3)

### LLM provider & queue — [`llm-provider.md`](contracts/llm-provider.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-LLM.1 | Provider interface | A single `Complete(ctx, Request) (Response, error)` implemented by the real adapter (Ollama); streaming via `Request.OnChunk`. |
| R-LLM.2 | Tool calls and final answers | A completion yields either tool calls or a final answer, parsed to the common shape. |
| R-LLM.3 | The priority queue (I2) | All provider calls go through the queue; `max_concurrent` bounds in-flight calls. |
| R-LLM.4 | Priorities | With `max_concurrent=1` and one in flight, queued requests at priority 3/2/1 are served 1→2→3. |
| R-LLM.5 | Cancellation & timeout | A cancelled/expired context aborts the call and frees its queue slot. |
| R-LLM.6 | Token counting | Budgeting uses the builder's 4-chars≈1-token estimate; the provider interface carries no token-counting method. |
| R-LLM.7 | Ollama adapter (required) | `/api/chat` streaming accumulates to `Text` + `OnChunk`; tool calls surface; `done_reason`→`StopReason` mapped; `num_ctx` applied; `prompt_eval_count`/`eval_count` from the final chunk surface as `Usage`; HTTP/in-stream errors surfaced — covered by a hermetic `httptest` unit test. |
| R-LLM.8 | Reported usage | `Response.Usage` carries provider-reported input/output token counts; zero means *not reported* and is never synthesized; usage is reported, never enforced. |

### Embedder — [`embedder.md`](contracts/embedder.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-EMB.1 | Interface | `Embed(ctx, text) ([]float32, error)` implemented. |
| R-EMB.2 | Providers | `keyword` (default), `ollama`, `none` all constructible from `[embeddings]`; any other value falls through to `keyword`. |
| R-EMB.3 | Two embedding code paths | Indexing-time and query-time embedding both flow through the same embedder. |
| R-EMB.4 | Namespaces | Embeddings store/query under `skills`/`session-index`/agent namespaces (no `tools:`; see R-MEM.6). |
| R-EMB.5 | Determinism & degradation | `keyword` embeds a string identically twice; with `none`, ranking is bypassed (all candidates returned). |

### Plugin manager — [`plugin.md`](contracts/plugin.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-PLUG.1 | Plugin contract | `plugin.describe` and `plugin.call` over HTTP (`POST /rpc` on `NINE_PLUGIN_SOCKET`) behave as specified. One transport: MCP arrives through the bridge plugin (R-PLUG.15), not a second path. |
| R-PLUG.2 | `plugin.Serve` ergonomics | A plugin passing `[]ToolDefinition` + a handler map serves the full contract via the shared loop. |
| R-PLUG.3 | Manager lifecycle | Spawn (env `NINE_PLUGIN_SOCKET` + `NINE_BIN` + extras), await socket, describe, track `*Plugin`, forward calls over an HTTP client. |
| R-PLUG.4 | Crash isolation (I9) | Killing a plugin subprocess mid-run is reported without crashing the daemon. |
| R-PLUG.5 | Default plugins | `files`, `shell`, `http`, `time` start at boot (skills/memory are core-intercepted). |
| R-PLUG.13 | Built-ins in the `nine` binary | `files`/`shell`/`http`/`time` start as `nine plugin serve <name>` child processes — no per-plugin binary on disk — and still get their own process, sanitized env, cache dir, and `max_concurrent`. Nine ships nothing into `[plugins].bin`. |
| R-PLUG.13a | Plugin child is single-purpose | A plugin child reads no config file (even with one in its cwd or `$HOME`), opens no `nine.log`, and builds no CLI; `plugin serve` without `NINE_PLUGIN_SOCKET` exits non-zero naming the variable. |
| R-PLUG.15b | MCP content flattening | An image part is saved to the plugin cache dir and its path named in the tool result — never dropped; a tool named `../escaped` cannot write outside that dir. |
| R-PLUG.15a | MCP transports and startup | A `command` server (stdio) and a `url` server (streamable HTTP, JSON or SSE reply) both load; a server that takes far longer than the socket-ready budget to start still loads, because the bridge listens before handshaking. |
| R-PLUG.15 | MCP servers are plugins | Each `[[mcp.server]]` runs as its own `mcp:<name>` plugin instance with prefixed tools (`github__create_issue`); it crashes, disables, and reports independently, and the core holds no MCP-specific transport or spawn path. |
| R-PLUG.14 | `[plugins].disabled` | A named plugin never spawns — built-in, own-binary, or user alike — and no socket or cache dir is created for it; the daemon boots normally and reports it in `plugins_list` with `disabled: true` (`nine plugins` shows `off`). |
| R-PLUG.6 | Browser automation is not a plugin | No browser ships as a default plugin; a browser MCP server declared as `[[mcp.server]]` supplies `<name>__browser_*` tools, and no URL policy is enforced on it. `web_search`/`web_page_read` remain available and are not described as a fallback to an absent browser. |
| R-PLUG.7 | No runtime plugin mutation (N1) | No path generates, compiles, or hot-swaps a plugin; recovery is restart-from-binary only. |
| R-PLUG.14 | Startup readiness: dead vs slow | A plugin that exits before listening is reported as exited (with status) immediately, not as a socket timeout; the readiness budget bounds only a live-but-silent process; the process is reaped exactly once and the result is readable by both startup and stop. |
| R-PLUG.8 | Per-plugin concurrency | `describe`'s `max_concurrent` maps onto the transport's `MaxConnsPerHost`; 0/omitted is unbounded, and a declared cap bounds in-flight calls to that plugin client-side. |
| R-PLUG.9 | User plugins | Executables under `[plugins].user_dir` load only with a sidecar `<name>.toml` declaring name and entrypoint; no manifest means no load, and built-ins are untouched. |
| R-PLUG.10 | Operator settings pass-through | `[plugin.<name>.settings]` keys reach the process verbatim as environment variables at spawn; a key outside `[A-Za-z_][A-Za-z0-9_]*` is rejected. |
| R-PLUG.11 | Per-plugin cache directory | Each plugin process gets `NINE_PLUGIN_CACHE_DIR` (existing, mode 0700) plus `NINE_PLUGIN_CACHE_PERSISTENT`; Nine never reads the directory, and an ephemeral one is removed with the process. |
| R-PLUG.12 | Long-running jobs (opt-in) | A tool may return `job_id` + ack only when the plugin advertised `async_jobs`; a `job_id` from a plugin that did not is rejected. |

---

## 3. Context, loop, dispatcher (phases 4–5)

### Context builder — [`context-builder.md`](contracts/context-builder.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-CTX.1 | Token model | Sizing uses 4 chars ≈ 1 token, no tokenizer dependency. |
| R-CTX.2 | Priority allocation | With a tiny budget, P1 system core survives and P5 extras drop below the 200-token floor. |
| R-CTX.3 | Tool relevance filtering | With many tools, only top-N by cosine similarity plus always-include appear. |
| R-CTX.4 | Assembly output & usage | The assembled request matches the provider's expected shape. |
| R-CTX.5 | Memory as compression | Behavioral guidance present in the system prompt (advisory steer, not a hard gate). |

### Agent loop — [`agent-loop.md`](contracts/agent-loop.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-LOOP.1 | Turn structure | `Run(ctx, text)` embeds the query once, assembles context, submits via the queue. The system core carries the current time and, when the session has one, a `Session ID: <id>` line naming that loop's own session. |
| R-LOOP.2 | Scratchpad is working memory | Observations accumulate in the scratchpad across inner round-trips. |
| R-LOOP.3 | History | A final answer (no tool calls) clears the scratchpad and folds the exchange into history. |
| R-LOOP.4 | Tool-call retry | An always-erroring tool is retried up to `maxToolRetries=2` (3 attempts total). |
| R-LOOP.5 | Empty-answer retry + fallback | A response with no text and no tool calls is retried up to `maxEmptyAnswerRetries=2` (3 attempts total); only then does the fallback surface accumulated tool errors rather than returning blank. |
| R-LOOP.6 | Progress callbacks | Tool start/end and context updates fire through the progress callback. |
| R-LOOP.7 | Checkpoint unit & stall signal (I5) | The turn exposes `{history, scratchpad}` for checkpointing and a no-tool-turn signal. |
| R-LOOP.8 | Priority is the owner's | The loop submits at its owner's priority (supervisor/conversation/background). |

### Dispatcher — [`dispatcher.md`](contracts/dispatcher.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-DISP.1 | Dispatch path | A name→handler map routes each tool call to its handler. |
| R-DISP.2 | Output cap and spill | Tool output is capped at 2048 tokens. Over-cap output is stored whole and replaced by a banner+head+tail preview naming the path; with no spill sink (or on sink failure) it is truncated instead, and the call still succeeds. |
| R-DISP.3 | Tool taxonomy | Plugin vs core-intercepted tools are classified as specified. |
| R-DISP.4 | Wiring model | `Dispatcher.New()` is empty; `Register*` add handlers at startup (only a role's allowed tools); an unregistered tool dispatches as `unknown tool`. |
| R-DISP.5 | Post-call hooks | A registered post-call hook fires after the matching tool returns. |
| R-DISP.6 | Role-aware registration (I6) | Registration follows the role's Delegates/SpawnsGoals/allowlist gates; delegation tools are absent once depthGuard is exhausted. |
| R-DISP.7 | Reference arguments | A property marked `x-nine-ref` is resolved from the file store before dispatch; unmarked properties are never expanded; an unresolvable path fails the call; approval gates and hooks see the unexpanded arguments. |

---

## 4. Worker, attach, daemon (phases 6–7)

### Agent worker — [`agent-worker.md`](contracts/agent-worker.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-WORK.1 | One serial worker, serialized (I1) | A single-slot inbox prevents concurrent turns on a session; an idle and a user turn never overlap. |
| R-WORK.2 | Turn pipeline | `processTurn` runs prepend-notifications → progress → `loop.Run` → stall check → checkpoint → respond. |
| R-WORK.3 | Notifications | Pending notifications are prepended to the next turn. |
| R-WORK.4 | Stall detection | Five consecutive no-tool turns fire `OnStall` once, then reset. |
| R-WORK.5 | Progress streaming | Turn progress streams to the attached client live. |
| R-WORK.6 | Replay buffer | Ring cap 200; ≤ 50 recent events replayed on attach. |
| R-WORK.7 | Clean shutdown | Worker stops without dropping the in-flight checkpoint. |
| R-WORK.8 | Durable journal | With an `EventSink`, a turn records `turn_start`/`llm_request`/`llm_response`/`tool_*`/`turn_end` to `session_events`, distinct from the in-memory progress/replay buffers. |

### End-to-end (phases 6–7 gates)

| Check | Observable |
|-------|------------|
| Cold-start conversation | `nine "hello"` auto-starts the daemon and prints a reply. |
| Thread continuity | A second `nine "..."` continues the same thread (history retained). |
| Streaming | Tool calls stream to the client during a turn. |
| Status | `nine status` lists uptime, active agents, loaded plugins. |
| Attach mid-turn | Disconnect mid-turn, `nine attach <id>` redraws recent activity and delivers the final answer. |
| Attach after restart | After a daemon restart, `attach` rebuilds the session from its checkpoint. |

---

## 5. Autonomy (phases 8–10)

### Session plans — [`session-plans.md`](contracts/session-plans.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-PLAN.1 | Data model | `session_plans` rows persist routine kind + state. |
| R-PLAN.2 | RoutineHandler interface | `Init`/`OnTurnEnd`/`OnIdle` invoked at the right points. |
| R-PLAN.3 | Lazy vs eager persistence (I7) | `[active]` plans are lazy; idle-capable plans are eager-persisted. |
| R-PLAN.4 | Idle scheduling | `armIdleTimer` picks the soonest interval; on fire, `handleIdle` runs the due `OnIdle`, and any returned text runs as a turn. |
| R-PLAN.5 | Resume on restart (I7) | Idle-capable plans restart at boot; ordinary conversations attach on demand. |
| R-PLAN.6 | `active` routine | All-no-op routine behaves trivially. |
| R-PLAN.7 | `idle-reflection` routine | Attachable to any session; updates self KV. The turn is recorded by the journal like any other — there is no dedicated reflections store. |
| R-PLAN.8 | Self-model (`SystemSelf`) | `self/identity`+`self/capabilities`+`self/learned` injected as P2.5, capped ~600 tokens. |
| R-PLAN.9 | `pursue` routine | 5-min interval; reads the goal and its derived children, acts, syncs status, pauses goal on stall. |

### Goals — [`orchestration.md`](contracts/orchestration.md) (§ goals)

| ID | Property | Observable check |
|----|----------|------------------|
| R-ORCH.10 | What a goal is | Goal data model + store methods as specified. |
| R-ORCH.11 | Tools (depth < 2) | `goal_create`/`goal_get`/`goal_list`/`goal_update_status`, depth-capped. |
| R-ORCH.12 | Pursue spawning (depth 0 only) | Top-level `goal_create` spawns one pursue session; duplicate is a no-op; at `max_goal_sessions` (10) it reports `limit_reached`. `nine goals` lists goals. |

---

## 6. Delegation & workflows (phases 11–12)

### Sub-agents — [`orchestration.md`](contracts/orchestration.md) (§ sub-agents)

| ID | Property | Observable check |
|----|----------|------------------|
| R-ORCH.1 | Tools | `run_agent` and `run_agents` registered for delegating roles; both accept an optional `role` field. |
| R-ORCH.2 | Execution model | A child runs a fresh loop in its leaf role at depthGuard-1 synchronously (`RunSubAgentSync`); lifecycle streams as `sub_agent_start`/`sub_agent_end`. |
| R-ORCH.3 | Delegation termination (I6) | An `executor` child can `run_agent`; its grandchild cannot (tool absent, guard exhausted). `run_agents` group timeout = 1800s; stragglers cancelled and marked `timed_out`. |

### Workflows — [`orchestration.md`](contracts/orchestration.md) (§ workflows)

| ID | Property | Observable check |
|----|----------|------------------|
| R-ORCH.4 | What a workflow is | Workflow + steps data model and status vocabulary. |
| R-ORCH.5 | Package boundary (I3) | `workflow.Service` depends only on a narrow `Repository`; the store implements it. |
| R-ORCH.6 | Tools (depth < 2) | `workflow_create`/`update`/`get`/`list`/`retry_step`, depth-capped. |
| R-ORCH.7 | Auto-close | A workflow auto-closes `done` when its last step completes. |
| R-ORCH.8 | Operator commands | `workflow_stop` (live cancel: pending→skipped, running→failed) and `workflow_fail` (post-mortem, works daemon-down). |
| R-ORCH.9 | Startup scrub | After an unclean restart, orphaned `running` steps become `failed (interrupted)`; now-terminal workflows auto-close; those with `pending` stay `active`. |
| R-ORCH.13 | One representation of the goal tree | `goal_get`'s `subtree` is derived from children's `parent_id`; no column stores it and no tool writes it. |

---

## 7. Oversight (phase 13) — [`supervisor.md`](contracts/supervisor.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-SUP.1 | Durable, journal-backed bus | `Post` synchronously appends to `session_events` (never dropped); the supervisor consumes via a resumable cursor and survives restart. |
| R-SUP.2 | Events | The four event kinds carry at least the `AgentID`/plugin name. |
| R-SUP.3 | Reactions | Stalls/gaps are surfaced (never self-generate plugins/config); plugin crash → restart-from-binary. |
| R-SUP.4 | Priority | Supervisor LLM calls submit at priority 1. |
| R-SUP.5 | `gap_report` tool | A `gap_report` call posts exactly one `EventGapReported`. |
| (gate) | Stall→event | Five consecutive no-tool turns post exactly one stall event. |

---

## 8. Human-in-the-loop (phase 14) — [`hitl.md`](contracts/hitl.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-HITL.1 | Interactive-session gating | `ask_human` registers only for interactive sessions (never for sub-agents); the flag persists across restart; background/sub-agent paths pass `false`. Approval gates instead follow the owning session into its sub-agents (R-HITL.5). |
| R-HITL.2 | `ask_human` tool | Schema matches; available only in interactive sessions, all depths within them. |
| R-HITL.3 | `ask_human` behavior | A call blocks the turn until `human_input_answer`; on answer it resumes; on timeout it returns an error (not cancellation); one pending per **asking agent** (parallel sub-agent gates get distinct rows). |
| R-HITL.4 | Restart recovery | A pending request survives restart and is re-emitted; an answer given while down returns immediately; stale pendings expire at boot. |
| R-HITL.5 | Approval gates | With `require_approval=["shell"]`, a `shell` call prompts; non-`y` fails it and is **not** retried; a sub-agent of an interactive session prompts on that session's stream with an `origin`; `gate_sub_agents=false` or a non-interactive root is never prompted. |
| R-HITL.6 | Protocol messages | `human_input_required` streams on the **owning** session's progress channel (with `origin` when a sub-agent asks); `human_input_answer` is a top-level message that starts no turn. |
| R-HITL.7 | Persistence | `human_requests`/`interactive_sessions` tables + methods; HITL tables are never tools (I4). |
| R-HITL.8 | TUI rendering | A pending question renders a panel with a `?` badge; submit sends an answer, not a turn; concurrent questions queue and render oldest-first with a waiting count, and an `origin` is shown. |

---

## 9. Skills & boundary (phase 15) — [`skills.md`](contracts/skills.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-SKILL.1 | File / record format | Markdown with `name`/`description`/`tags` frontmatter in the skills dir. |
| R-SKILL.2 | Three sources, two immutable | Built-in and user skills are read-only to the agent (`skill_write`/`skill_modify` refuse both); agent-authored ones accept updates. A user skill seeds from `[skills].user_dir` as `source=user`; one whose name matches a built-in is skipped, not applied. An invalid user file is skipped with a logged reason and the daemon still boots. Deleting a user file prunes only that skill on the next boot; an absent/unconfigured dir prunes nothing. |
| R-SKILL.3 | Tools | `skill_list`/`skill_read`/`skill_write`/`skill_modify`/`skill_search` behave as specified. |
| R-SKILL.4 | Indexing hook | After `skill_write`/`skill_modify`, the description embeds into `skills:` and is discoverable via `skill_search` next turn without restart. |
| R-SKILL.5 | Self-improvement boundary (N1–N3, I10) | No tool exists that mutates `nine.toml`, builds a plugin, or rebuilds the binary. |

### Self-documentation — [`self-documentation.md`](contracts/self-documentation.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-DOC.1 | Addressing | Every section address resolves to exactly one section; repeated headings disambiguate; an unanchored address returns the whole document, and the leading section carries a title-derived anchor of its own. Names accepted by `nine docs <topic>` resolve identically; hidden documents are not indexed. |
| R-DOC.2 | Chunking | Split at `##`; headings inside code fences do not split; an oversized section splits again at `###` without losing its lead-in. Indexed text includes the document title and section heading. |
| R-DOC.3 | Index holds addresses | The `docs` namespace stores one vector per section keyed by address; no section body is written to the store. |
| R-DOC.4 | Fingerprinted indexing | An unchanged corpus + embedder re-embeds nothing on reboot; changing either forces a full namespace rebuild; a pass with any embed failure records no fingerprint and retries next boot; a nil embedder skips without error. |
| R-DOC.5 | Tools | Every `doc_search` address is readable by `doc_read`; unresolvable addresses are skipped, not returned; a `bundle` filter still fills `top_k`. `doc_read` accepts addresses, topic names, and bundle paths, errors with the topic list on an unknown ref, and returns Markdown rather than JSON. Both tools work with no embedder configured (lexical ranking); hybrid ranking degrades to the surviving retriever rather than failing; a labelled retrieval suite guards the floor. |
| R-DOC.6 | Docs are the source of truth | A documented/observed mismatch is reported as a defect rather than answered from the implementation. |

### Roles — [`roles.md`](contracts/roles.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-ROLE.1 | Frontmatter format | A skill with a `role:` block is also a role; without one it is a plain skill; a malformed block degrades to a plain skill without failing boot. |
| R-ROLE.2 | Wildcard tools | `tools: "*"` or an omitted tools key grants all available tools; an explicit list is a strict allowlist. |
| R-ROLE.3 | Body is the persona | The role body is the loop's system core; an empty body falls back to the daemon prompt (the orchestrator runs on the daemon prompt). |
| R-ROLE.4 | Two-boundary enforcement | A disallowed tool is absent from the advertised list AND dispatches as `unknown tool`. |
| R-ROLE.5 | Roles only narrow | An allowlist naming an unavailable tool spawns a leaf without it, no error; `gap_report` survives every allowlist. |
| R-ROLE.6 | Depth is a guardrail | depthGuard 0 removes delegation tools from a `Delegates:true` role. |
| R-ROLE.7 | Agent roles are restrictive; operator roles are trusted | An **agent-authored** role with `persists:true` yields a non-persisting leaf (structural flags ignored). A **user** role (`source=user`, from `[skills].user_dir`) with `persists:true`/`delegates:true` keeps them, like a built-in. Neither may widen tools beyond the daemon's surface. |
| R-ROLE.8 | Delegation role field | `run_agent`/`run_agents` accept an optional `role`; omitted/unknown resolves to the default leaf, never an error. The advertised schema's role enum is rendered from the live registry, so a store-backed role skill (user or agent) appears in it — not only the built-ins — and its order is stable across boots. The system-prompt steering names the same live set, reaches every delegating worker (including a role with its own body, which never sees the daemon prompt), and is absent for roles that cannot delegate. |
| R-ROLE.9 | Resolution & fallback | Built-ins resolve from the embedded registry; user and agent role skills resolve from the store at delegation time; children always spawn as leaves. |
| R-ROLE.10 | Leaf persona | A spawned leaf's system core is its role body (executor's sub-agent stance by default), never the orchestrator's daemon prompt. |

---

## 10. Operator surface (phase 16) — [`wire-protocol.md`](contracts/wire-protocol.md) (client)

| Check | Observable |
|-------|------------|
| List verbs | `goals`/`workflows` render the matching read-only daemon query; `reflections` reads the journal directly. |
| `workflow stop\|fail [--all]` | Operator workflow commands act as in R-ORCH.8. |
| Slash commands | `/help`, `/status`, `/config`, `/context [id]`, `/plan-mode <mode>`, `/sessions`, `/tools [filter]`, `/skills [name]`, `/memory [key]`, `/goals`, `/workflows`, `/new`, `/clear` run with no LLM call. |
| Command picker | Typing `/` at the start of the TUI input opens a picker listing every command with its description; typing filters it by name prefix, and it closes when the leading `/` is deleted, when an argument is started, when nothing matches, or on Esc. Suppressed while an `ask_human` question is on screen. |
| Live TUI | The TUI shows tool calls live; a pending `ask_human` renders with a `?` badge. Also: `nine trace`/`nine replay`/`nine send` (see §12) and `nine notifications`. |

---

## 11. Startup wiring & resume (phase 17)

| Check | Observable |
|-------|------------|
| Boot order | A single `runDaemon` builds the graph in the specified order (config → SQLite store (fail-fast) → plugins → checkpoint/notif → embedder → supervisor+`Attach` → self-model → bootstrap self KV → reflection → pursue → workflow+journal scrub → HITL → builder → daemon → event sink → configure (+ subscribers) → inject spawn/progress fns → start supervisor → reconcile standing agents → `ResumeSessions` → accept loop). |
| Cold boot | Seeds self KV and the reflection session; scrubs stale workflows and journal events; seeds config-declared standing agents; resumes idle-capable sessions from a prior run. |
| Restart survival | Killing and restarting the daemon brings back the reflection session and all active goal pursue sessions; ordinary conversations return on `attach`. |

---

## 12. Event journal & subscriptions (phase 18) — [`event-journal.md`](contracts/event-journal.md), [`subscriptions.md`](contracts/subscriptions.md)

| ID | Property | Observable check |
|----|----------|------------------|
| R-EVT.1 | Append-only, ordered, typed | `session_events` rows are never mutated; reads for one agent are `seq`-ordered; the journal is not an agent tool. |
| R-EVT.2 | Async batched producer | A completed turn's exact model I/O and tool trajectory are reconstructable from the journal; journal writes never stall a turn. |
| R-EVT.3 | Trace & replay | `nine trace` renders the journal with the daemon down; `nine replay` reproduces a recorded turn's answer with **no** live LLM/tool calls. |
| R-EVT.4 | Retention | A boot scrub bounds the journal by `event_retention_turns`/`event_retention_days`, leaving a replayable prefix. |
| R-EVT.5 | Read methods | `SessionEventsAppend`/`ByAgent`/`After`/`Scrub` and `LatestTurnResult` exist; ByAgent returns the full trajectory in `seq` order, After scans forward from a cursor, Scrub bounds by turns and age. |
| R-SUB.1 | Subscription primitive | A `Handler` resumes from its durable cursor (`event_cursors`), receives filtered events in `seq` order at-least-once; a poison event is skipped, not wedged. |
| R-SUB.2 | Wake + catch-up | An in-process wake delivers promptly; a subscriber that was down catches up from its cursor on restart. |
| R-SUB.3 | Out-of-band discipline (I11) | A subscriber makes no generative LLM call and never mutates an active session; it writes only derived stores. |
| R-SUB.4 | Related-session indexer | On `turn_end` (with an embedder), topically-similar prior sessions are linked in `related_sessions` (threshold-gated, deduped); no generative call; a no-op without an embedder. |
| R-SUB.5 | Pull surfacing | A later on-topic turn surfaces one recorded link as `SystemEnrichment` under relevance + budget; an off-topic turn surfaces nothing. |
| R-SUB.6 | Supervisor as a subscriber | Supervisor lifecycle events are posted as journal `supervisor` events and consumed through a cursor-backed subscription that survives restart. |
| R-SUB.7 | Deferred by decision | No generative-LLM reaction and no autonomous session injection exists; reactions stay programmatic and out-of-band. |

---

## Done

When every row passes, the implementation conforms to this spec. Phase gates in
[`build-order.md`](build-order.md) are the incremental checkpoints; this file is the whole.
