# Glossary

A reference of concepts, components, and terms used throughout Nine's
architecture and documentation. Entries are grouped by topic; each links to
the doc with the full explanation.

---

## Core Processes & Components

**Daemon** — The long-running background process that holds all state: plugin
registry, LLM queue, active sessions, and the handle to the SQLite store.
The CLI is a
thin client that connects to it over a Unix socket and auto-starts it if not
running (`EnsureDaemon`). See [Daemon Architecture](daemon.md).

**CLI** — The `nine` binary used both as a one-shot client (`nine
"<message>"`) and as the interactive TUI (`nine` with no args). See
[CLI Usage](usage.md).

**AgentWorker** — Per-session goroutine that wraps one `agent.Loop`,
serializes turns through a buffered-1 inbox channel, prepends pending
notifications, runs the loop, checks for stalls, and checkpoints state after
every turn. Used for interactive conversations, self-reflection, goal pursue
sessions, and sub-agents alike. See [AgentWorker Architecture](runner.md).

**Agent Loop (`internal/agent/loop.go`)** — The ReAct (Reason → Act →
Observe) implementation. Holds `history` and `scratchpad`, repeatedly builds
a context, submits it to the LLM queue, and dispatches any tool calls until
the model returns a final answer with no tool calls. See
[Agent Loop](agent-loop.md).

**Supervisor Agent** — A special agent that monitors all other sessions. It
activates on `gap_report` calls or stall detection. It has higher LLM queue
priority than background work but lower than active conversations. See
[Architecture § 14 Autonomy & oversight](architecture.md#14-autonomy--oversight-components).

**Tool Dispatcher (`internal/agent/dispatcher.go`)** — Routes each tool call
to its registered handler: plugin tools via `plugin.call`, core-intercepted
tools in-process. Fires post-call hooks on success and caps every result at
~2048 tokens, spilling larger output to the file store
([tool-output-spill.md](tool-output.md)). Handlers are registered at
loop-build time via `RegisterPlugin` and the `Register*` functions.
See [Agent Loop § Dispatcher](agent-loop.md#dispatcher).

**Spill (`spill/<agent-id>/…`)** — A tool result too large for the output cap,
written whole to the memory file store and replaced in the model's context by a
head+tail preview naming the path. The namespace is daemon-owned: `file_store`
cannot write to it and nothing in it is embedded, so untrusted tool output can
never be pull-surfaced into a later turn as fact.

**Reference argument (`x-nine-ref`)** — An input property a tool declares as
carrying a file-store *path* rather than a value. The daemon substitutes the
stored content before the call, so a large payload moves between tools without
passing through the model's context.

**Plugin Manager (`internal/plugin/manager.go`)** — Spawns plugin
subprocesses, calls `plugin.describe`, and registers their tools with the
dispatcher. Plugins are fixed at build time — started at boot and stopped on
shutdown (`Start`/`TryStart`/`Stop`/`StopAll`); there is no runtime hot-swap. A
crashed subprocess is isolated from the daemon. See [Plugins](plugins.md).

**LLM Queue (`internal/llm/queue.go`)** — Prioritized queue in front of the
LLM provider. Enforces `max_concurrent` in-flight requests. Priority order:
1 = supervisor, 2 = active conversations, 3 = background sessions and sub-agents. See
[Architecture § 9 The LLM queue](architecture.md#9-the-llm-queue).

**Context Builder (`ninectx.Builder`, `internal/context/builder.go`)** —
Assembles one `llm.Request` per loop iteration from system prompt, tool
definitions, message history, and scratchpad, trimming lower-priority content
to fit `context_budget`. See [Context Builder](context-builder.md).

**Checkpoint** — Serialized `history` + `scratchpad` for one agent, persisted
to the `conversations` table (`CheckpointStore`) after every turn via `SaveCheckpoint`/
`LoadCheckpoint`. Enables `nine attach <id>` and survival of daemon
restarts. See [Agent Loop § Checkpointing](agent-loop.md#checkpointing).

---

## Work Units

**Conversation** — An interactive thread tied to a terminal session. Status
`active` (terminal connected) or `archived` (disconnected, resumable via
`nine attach`).

**Work unit** — Nine has two fundamental units: a **session** (an agent loop —
conversation, goal-pursue, reflection, or sub-agent) and a **tool call** (the
atom, issued within a turn). There is **no "Task" entity**: finite work is
either a sub-agent (transient) or a workflow step (durable), and
`task_timeout_seconds` is just the sub-agent timeout. See
[overview.md §3](../spec/overview.md).

**Goal** — A persistent, open-ended intention with no defined end condition
(e.g. "monitor this repo for security issues"). A goal carries a status
(`active`/`paused`/`done`/`archived`) and an optional parent. Sub-goals are
ordinary goals naming their parent, so the parent link *is* the hierarchy —
there is no separate list to maintain. LLM tools: `goal_create`, `goal_get`,
`goal_list`, `goal_update_status` (role-gated like `run_agent`).
See [Architecture § 14 Goals](architecture.md#goals--state-write-paths-status).

**Sub-agent (`run_agent` / `run_agents`)** — Core-intercepted tools that spawn
a child agent to execute one task (`run_agent`) or several in parallel
(`run_agents`, default 120s timeout). Both accept an optional `role` naming the
child's worker role (default `executor`; see [Roles](roles.md)). Available to
delegating roles only, with the delegation depth guard as recursion backstop.
See `internal/agent/register_subagents.go`.

**Workflow** — A **ledger of delegated work** the LLM keeps: a named, persistent
record of the multi-step plan it created before delegating to sub-agents.
Passive — it has no session, no scheduler, and no driver, so it advances only
when a model calls a `workflow_*` tool inside a turn. That is what separates it
from a **Goal**, which owns a session and wakes itself; the axis is autonomy, not
ordering (`spec/overview.md` §3.1). Has a `name`, ordered `steps`
(`pending`/`running`/`done`/`failed`/`skipped`), and an overall `status`
(`active`/`done`/`failed`/`cancelled`). Auto-closes when all steps reach a
terminal state. LLM tools: `workflow_create`, `workflow_update`,
`workflow_get`, `workflow_list`, `workflow_retry_step` (depth-capped like
`run_agent`). Operator commands: `nine workflow stop|fail`. See
[Workflows](workflows.md).

**Session Plan** — Persistent state machine of **routines** attached to every
`AgentWorker` (ordinary conversation, the self-reflection session, or a
goal's pursue session). Stored as one row per agent ID in `session_plans`.
Drives autonomous between-turn behavior via `OnTurnEnd` (every turn) and
`OnIdle` (per-routine idle scheduler). See [Session Plans & Routines](session-plans.md).

**Routine / RoutineHandler** — The extension point for session plans. Each
`kind` (`active`, `idle-reflection`, `pursue`) implements `Init`,
`OnTurnEnd`, and `OnIdle`. `active` is a no-op trivial routine every
conversation gets; `idle-reflection` and `pursue` are idle-capable and get
their own resumable background sessions.

**Self-reflection session (`idle-reflection` routine)** — A single fixed
session (agent ID `self-reflection`) that wakes every 2 minutes and asks the
model to update `self/capabilities` and `self/learned` via `memory_set`. Each
turn is recorded by the journal under its own `agent_id`, read back with
`nine reflections [agent-id]` (or `/reflections`). It is a routine kind, not a
session kind: any session can carry it as a routine. See [Session Plans § idle-reflection](session-plans.md#idle-reflection--self-reflection-session).

**Pursue session (`pursue` routine)** — Background session spawned 1:1 for
every top-level goal (`agentID == goalID`), waking every 5 minutes to
`goal_get`, act on the goal, record what it finds as sub-goals, and call
`goal_update_status`. Capped by `daemon.max_goal_sessions` (default 10);
`goal_create` reports `pursue_session: "spawned"` or `"limit_reached"`. See
[Session Plans § pursue](session-plans.md#pursue--background-goal-pursuit).

---

## Context, Memory & Self-Model

**Context Budget (`context_budget` / `num_ctx`)** — Per-turn token limit. The
context builder allocates it across priorities: (1) system prompt core —
never trimmed, (2) tool definitions — relevance-filtered, (2.5) self-model —
capped ~600 tokens, (3) message history — oldest trimmed first, (4)
scratchpad — oldest trimmed first, (5) system extras — dropped if tight. See
[Context Builder](context-builder.md).

**Scratchpad** — Ephemeral list of `ScratchpadEntry{Thought, ToolName,
ToolArgs, Observation}` accumulated during the current turn's tool-call loop.
Each entry expands to an assistant message (thought + tool call) and a user
message (tool result) for the LLM. Cleared on every final answer; persisted
in checkpoints so it survives disconnects/restarts.

**Tool Relevance Filtering** — At each turn the advertised tool set is capped at
top-N (`ToolTopN`, default 20), plus always-include tools (memory tools,
core-intercepted tools like `gap_report` and `run_agent`) which bypass the cap.
Non-always tools are ordered by cosine similarity between the query and each
tool's description embedding. The `AgentBuilder` embeds each tool description once
(cached by name; descriptions are static after boot) and sets it on the
`ToolWithVector`, so ranking is live with any embedder; with `provider = "none"`
vectors are nil and selection falls back to insertion order under the cap.

**Embedder** — Provider-agnostic interface (`Embed(ctx, text) ([]float32,
error)`) used for skill ranking, semantic memory/file search, and the
related-session indexer. Configured independently of the chat LLM via
`[embeddings]`. Providers: `keyword` (built-in, default, no network), `ollama`,
or `none` (disables ranking). See
[Configuration § Embeddings](configuration.md#embeddings).

**Vector store / namespaces** — Embeddings are stored in the in-process
`internal/memory.Store`'s `vectors` table (packed float32 blobs) under
namespaced keys — e.g. `skills` (skill descriptions), `docs` (one vector per
section of the bundled documentation), `session-index` (one vector per completed
turn, for the related-session indexer), and per-agent memory namespaces.
Nearest-neighbour queries rank by cosine similarity, computed in process over a
namespace scan.

**Self-model (`SystemSelf`)** — A context block built by
`internal/selfmodel.Assembler` from the `self/identity`, `self/capabilities`,
and `self/learned` KV keys, injected into every turn at priority 2.5 (capped
~600 tokens). Seeded by `BootstrapSelfKV`; `self/learned` and
`self/capabilities` are refreshed by the idle-reflection routine.

**`internal/memory.Store`** — The single, in-process **SQLite** interface for all of
Nine's persistent state — *not* a plugin subprocess. Opens a file
(`[memory].path`, default `~/.nine/nine.db`), creating it if absent, fails fast if
it is unusable, and is the sole owner of the database handles (the "single
gateway" invariant — a one-connection writer pool plus a read-only pool, since
SQLite serializes writes). Agent-facing K/V (`memory_get/set/delete/list`), file storage
(`file_store/fetch/list`, `file_search_text`), and (core-intercepted) vector ops
(`memory_embed`/`memory_query`/`file_search_semantic`) are exposed as tools.
Operational tables (`conversations`, `goals`, `notifications`,
`user_notifications`, `workflows`, `session_plans`,
`human_requests`, `interactive_sessions`, `session_events`, `event_cursors`,
`related_sessions`) are accessed only by the daemon, never exposed as agent tools.
(There is no `tasks` or `plugin_registry` table.)
See [Architecture § 12 Memory & persistence](architecture.md#12-memory--persistence).

**Notification** — Result of a completed/errored background session, goal, or
sub-agent. **Pull**: `nine goals`/`/goals` (and `nine workflows`) show live status.
**Push**: pending rows in the `notifications` table are prepended to the next
active-conversation turn (`prependNotifications`). Separately, the
`user_notifications` table is a human-facing feed background agents post to
(`notify_user`), read via `nine notifications`.

---

## Event Journal & Subscriptions

**Event journal (`session_events`)** — Append-only, per-session execution log:
one row per step (`turn_start`, `llm_request`/`llm_response` with the exact
assembled prompt, `tool_start`/`tool_end`, `context_update`, `turn_end`,
`supervisor`), ordered by a `seq` BIGSERIAL and grouped by `span_id`. It is the
source of truth for observation, replay, and subscriptions. Design:
[event log](event-journal.md).

**EventSink (`runtime.NewSQLEventSink`)** — The async, batched writer that
persists journal events off the turn's critical path. Its flush also wakes
journal subscribers (`daemon.NotifySubscribers`).

**Span (`span_id` / `parent_span_id`)** — Causal grouping within a turn: the turn
root, each LLM call, and each tool call get spans, so a trajectory forms a tree.

**`nine trace` / `nine replay`** — `trace` renders a session's journal directly
(works with the daemon down). `replay` (`internal/replay`) deterministically
re-executes a recorded session on a real loop wired to a *recorded*
provider/dispatcher — no live LLM or tool calls.

**Retention scrub (`SessionEventsScrub`)** — Boot-time pruning that bounds journal
growth: keep the last N turns per agent and/or drop events older than a max age
(`[daemon] event_retention_turns` / `event_retention_days`).

**Subscription (`internal/subscribe`)** — A durable-cursor reader over the journal:
a `Handler` (id = cursor key, opt-in event `Types`, idempotent `Handle`) driven
forward from its persisted position (`event_cursors`), woken in-process and
catching up after restart. At-least-once delivery; a poison event is logged and
skipped, never wedging the cursor.

**Subscriber (`internal/subscribers`)** — A programmatic, **out-of-band** handler
that reacts to journal events to enrich *derived* stores — never a generative LLM
call, never a write into the active session (enrich, don't interject). Design:
[reactive events](event-journal.md).

**Related-session indexer / `related_sessions`** — The first subscriber: on each
`turn_end` it embeds the answer, links topically-similar prior sessions into the
`related_sessions` table (vector-ranked, threshold-gated), and indexes the turn. A
later user turn *pulls* the most relevant link into context under the token budget
(pull, not push). On by default when an embedder is configured
(`[daemon] related_sessions_index`).

---

## Plugins & Tools

**Plugin** — A standalone binary that answers two methods — `plugin.describe`
(returns tool definitions) and `plugin.call` (executes a tool, returns a result)
— over **HTTP on a per-plugin Unix socket** (`NINE_PLUGIN_SOCKET`, `POST /rpc`;
see [HTTP transport](plugins-http-transport.md)). That is the only plugin
transport: an external **MCP** server is reached through the `mcp` bridge plugin,
which speaks stdio JSON-RPC to the server and the ordinary plugin contract to
Nine. A plugin may also implement two optional
job methods (`plugin.job_status` / `plugin.job_cancel`, protocol v2) for
long-running work. Plugins are compiled into the image at build time; there is no
runtime generation. See [Plugins](plugins.md) and
[Architecture § 10 Plugin subsystem](architecture.md#10-plugin-subsystem).

**Core-intercepted tools** — Tools that appear in the agent's tool list but
are handled directly by the Tool Dispatcher, with no plugin subprocess:
`gap_report`, `memory_embed`, `memory_query`, `file_search_semantic`,
`run_agent`, `run_agents`, the `workflow_*` tools, and the `goal_*` tools.
Registered by `AgentBuilder.registerCoreTools` and `registerSubAgentTools`
when each loop is built.

**Sandboxed tool** — A wasm module the daemon executes **in-process**, in a
wazero sandbox, with an explicitly conferred capability set. A *second backend
behind the same Tool Dispatcher* as plugins — registered, advertised, and
role-filtered identically — but neither a subprocess (unlike a **plugin**) nor
built in (unlike a **core-intercepted tool**). Installed by an operator as two
files in `[tools].user_dir`: a `.js` or `.wasm` entrypoint and a `.toml`
manifest. Off unless `[tools] enabled` is set. See
[Sandboxed tools](writing-sandboxed-tools.md) and
[contract](../spec/contracts/toolvm.md).

**Tool kind (`js` / `wasm`)** — How a sandboxed tool's module is obtained. A
`wasm` tool is the developer's own module, built from Rust, TinyGo, Zig, or C. A
`js` tool's module is the pre-supplied QuickJS-NG interpreter, with the author's
JavaScript as its input — nothing is compiled at install time. Not two trust
tiers: same ABI, same capability model, same everything downstream.

**Capability declaration vs. capability grant** — The distinction the whole
sandboxed-tool design rests on, and the one most easily blurred. A tool's
manifest **declares** what it needs (developer, in the repo); `[tool.<name>]` in
`nine.toml` **grants** what it gets (operator, on the host). Only the grant is
effective — nothing reads the declaration at call time, so *a manifest that lies
gains nothing*. The two must name the same capabilities or the tool does not
load. Contrast **plugin settings**, which are configuration, not capability.

**Generated tool** — A sandboxed tool authored by Nine itself, via the
core-intercepted `tool_write` (`sandboxed-tools.md` §5.2, R-TVM.14). A row in
the store's `tools` table holding the tool's `js` source and its capability
**declaration** — never a grant. It runs through the exact same sandbox, ABI, and
bounds as a **developer tool**; the differences are that the *agent* wrote the code
and that the operator confers a **capability ceiling** rather than a per-tool grant.
Off unless `[tools.agent] enabled`. *Nine cannot grant itself capabilities*
(R-PLUG.7) holds unchanged: `tool_write` writes code, never a grant.

**Capability ceiling (`[tools.agent.capabilities]`)** — The **maximum** any
generated tool may be granted, and the operator's only lever over a tier where the
agent writes the code. Not a default: a generated tool receives a capability only
if it **declares** it, so a tool that declares nothing runs with nothing however
permissive the ceiling is — and a tool cannot declare its way past it. Re-resolved
on every load, so narrowing the ceiling disables a tool that no longer fits rather
than leaving it running. Distinct from a **capability grant**, which is per named
developer tool.

**`js_eval`** — Runs one JavaScript snippet under the generated-tool rules and
persists **nothing** — no name, no row, no catalog entry (`sandboxed-tools.md`
§5.3). Not a softer trust tier than `tool_write`, only a less persistent one; it
exists so iterating on an idea does not accrete single-use tools into the catalog.
Switched on separately by `[tools.agent] eval`.

**`nine:*` stdlib** — A small, pinned, vendored set of pure-JavaScript modules a
generated tool may import with no config and no network — `nine:csv`, `nine:date`,
`nine:diff` (`sandboxed-tools.md` §4.2, R-TVM.15). Embedded in the binary and
served host-side; authored in-house rather than pulled from npm, so each is known
to run under the trimmed interpreter and carries no transitive surface.

**External dependencies / lockfile (`[tools.agent.deps]`)** — Off by default. When
an operator enables it, a generated tool may `import` a named npm package; Nine
resolves it **once, in the daemon, at `tool_write` time**, verifies its sha512,
runs no install scripts, and bundles it into the tool's source with esbuild — so a
called tool has no imports but `nine:*` and no network (R-TVM.15). The **lockfile**
is the exact third-party code a tool carries (name, version, integrity, requester),
printed by `nine tools show` / `nine tools deps`. The **interlock**: `deps` +
`net.http` on one tool is refused unless `allow_network_deps` — a networked package
turns the sandbox into an exfiltration path.

**Post-call hook** — A callback registered with `AddHook(toolName, fn)` that
fires after a successful call to a specific tool. (Skill description embedding
is now done inline by `skill_write`/`skill_modify` in `RegisterSkillTools`, not
via a hook.)

**Plugin lifecycle** — Built into the image (the Go built-ins into the `nine`
binary itself) → started at daemon boot (`TryStartBuiltin` spawns `nine plugin
serve <name>`, one `mcp` bridge instance per `[[mcp.server]]`, `TryStart` spawns
a user plugin's binary; then `plugin.describe`, register tools)
→ in use via `plugin.call` → SIGTERM on shutdown. A crashed
subprocess is isolated from the daemon; restart from the existing binary is the
plugin manager's responsibility. See [Plugins § Plugin Lifecycle](plugins.md#plugin-lifecycle).

**Default plugins** — Shipped with Nine and auto-loaded at startup: `shell`
(`shell`), `files` (`read_file`, `write_file`), `http` (`http_get`,
`http_post`, `web_search`, `web_page_read`), and `time` (`time`).
Memory/file/vector and skill tools are core-intercepted, not a subprocess
plugin. See [Plugins](plugins.md).

**MCP server** — A capability Nine does not build, declared as an
`[[mcp.server]]` and reached through an `mcp` bridge plugin — one process per
server, named `mcp:<name>`, tools prefixed `<name>__<tool>`. Either spawned over
stdio (`command`) or hosted over streamable HTTP (`url`). See
[Plugins § MCP servers](plugins.md#mcp-servers).

**Browser automation** — Not a plugin. Nine drives a browser by declaring
[Playwright's MCP server](https://github.com/microsoft/playwright-mcp) as an
`[[mcp.server]]`; its tools arrive as `playwright__browser_navigate` and so on.
Nine enforces no URL policy on it. This replaced a built-in Playwright plugin
that did block private/loopback URLs — see [Browser Automation](browser.md) for
the migration and the security consequences.

**`plugin.Serve`** — Helper in `internal/plugin` (`serve.go`) that implements the
JSON-RPC server loop for a plugin, so a plugin's `main` only passes its
`ToolDefinition`s and a `name → ToolHandler` map. See
[Plugins § Adding a Plugin](plugins.md#adding-a-plugin).

**`NINE_BIN`** — Environment variable passed to every plugin subprocess: the
plugin binary directory. Individual plugins may receive extra env vars at
startup: the built-in defaults (e.g. `NINE_WORKSPACE` for `files`), an operator's
`[plugin.<name>.settings]` (passed through verbatim), and the cache-dir vars
below. See [Plugin capabilities](plugin-capabilities.md).

**Plugin cache directory** — A per-plugin scratch directory the manager creates
and hands over as `NINE_PLUGIN_CACHE_DIR` (with `NINE_PLUGIN_CACHE_PERSISTENT`).
Ephemeral by default (`<root>/<name>.<rand>/`, wiped when the plugin exits);
`[plugin.<name>].persist_cache = true` keeps `<root>/<name>/` across restarts.
Opaque scratch — Nine never reads it. See [Plugin capabilities § 4](plugin-capabilities.md).

**Job (plugin job)** — Detached work a plugin tool starts and outlives the call:
`plugin.call` returns a `job_id` and an ack instead of a result, the daemon
records it in the `plugin_jobs` registry keyed to the owning conversation under a
stable **handle** (`job_<hex>`), and a sweeper polls `plugin.job_status` until it
finishes, then notifies the owner so a later turn learns of it. The model drives
it with `job_wait` / `job_check` / `job_list` / `job_cancel`. Distinct from a
*task* (the per-turn unit) and a *goal*. Native plugins only. See
[Plugin capabilities § 5](plugin-capabilities.md).

---

## Skills

**Skill** — A named markdown how-to note (`name`, `description`, `tags`, body)
stored in the `skills` table. Never preloaded; the description is
embedded into the `skills` vector namespace and the self-model surfaces relevant
names (context priority 5, dropped first under budget pressure), which the agent
then reads in full via `skill_read`. See [Skills](skills.md).

**Built-in vs. agent skills** — Built-in skills are seeded from the binary
(repo `skills/*.md`, embedded at build) on every boot and are **immutable** at
runtime. Agent skills are authored by Nine via `skill_write`/`skill_modify`,
persist in the store, and are mutable. `skill_write`/`skill_modify` refuse to
touch a built-in skill.

**Skill vs. Memory** — Skills are semantically-retrieved procedures that shape
*how* the agent approaches a class of task. Memory (KV store) is
exact-key-lookup, per-session/dynamic *data* the agent fetches explicitly.

**Self-documentation (`doc_search` / `doc_read`)** — The `docs/` and `spec/`
trees embedded in the binary, indexed at boot into the `docs` vector namespace
as one vector per `##` section and retrieved on demand. The index stores
addresses (`skills.md#tools`), never text: a read slices the section back
out of the embedded filesystem, so what Nine cites always matches its own
version. Never preloaded — the standing context cost is two tool definitions.
See [Self-Documentation](self-documentation.md).

**Default skills** — The repo's `skills/*.md` (e.g. `task-management`,
`git-workflow`, `go-development`), embedded into the binary and seeded into the
`skills` table on every boot (`runtime.SeedSkills`).

**Self-improvement loop** — When a task completes successfully, the
supervisor evaluates whether the approach is general enough to write up as a
new agent skill via `skill_write`, so future similar tasks can reuse it.

---

## Self-Improvement & Capability Gaps

**Self-Improvement** — Nine improves itself only by writing its own skills
(`skill_write` / `skill_modify`) — data, not code. Built-in skills are immutable.
It does not generate plugins, change `nine.toml`, or rebuild its own source at
runtime. See [Self-Improvement & Boundaries](self-modification.md).

**`gap_report`** — Core-intercepted tool an agent calls when no available
tool fits its task. Posts a gap event to the supervisor's internal event
queue (distinct from the user-facing notification queue).

**Capability Gap Detection** — When no tool fits, the agent first tries
`skill_search` to recall a documented approach. An unresolved gap is reported
via `gap_report` (or surfaced by stall detection), which notifies the
supervisor; capabilities Nine genuinely lacks are surfaced to the user rather
than self-generated.

**Stall detection** — The AgentWorker counts consecutive turns where
`agent.Loop.LastRunToolCount() == 0`. At `StallConfig.Limit` (default 3),
`OnStall` fires (notifies the supervisor) and the counter resets. Disabled if
`Limit == 0`. See [AgentWorker § Stall Detection](runner.md#stall-detection).

---

## Configuration & Operations

**`nine.toml`** — TOML config file, located via `$NINE_CONFIG` → `./nine.toml`
→ `/nine.toml` (Docker bind-mount) → `~/.nine/nine.toml`. Sections:
`[llm]`, `[daemon]`, `[plugins]`, `[skills]`, `[memory]`, `[embeddings]`,
`[ui]`, `[workspace]`. See [Configuration](configuration.md).

**LLM provider** — `[llm].provider`: `ollama`, the only chat backend
`BuildProvider` wires. Nine runs on local models, so any other value is reported
as unknown at boot and Ollama is used anyway. (`internal/llm/openai` is an empty
placeholder — OpenAI is available for *embeddings* only, via
`[embeddings].provider`.) The `Provider` interface is a single method,
`Complete(ctx, Request) (Response, error)`, with streaming via the request's
`OnChunk` callback.

**`max_concurrent`** — Cap on in-flight LLM requests in the priority queue;
set to `1` for local Ollama models to avoid contention.

**`max_goal_sessions` (`daemon.max_goal_sessions`)** — Cap on concurrently
running `pursue` sessions, default 10 (`DefaultMaxGoalSessions`).

**Token counting** — Approximated as 4 characters ≈ 1 token everywhere in the
context builder (no tokenizer dependency).

**Volume layout (`/data/`)** — Holds `nine.db` (the SQLite database, plus its
`-wal`/`-shm` sidecars) and `workspace/` (the sandboxed tools' workspace). All primary
state is therefore on one volume. The `nine` binary (with built-in skills
embedded) and plugins are immutable image content under `/opt/nine`.

**`path` (`[memory].path` / `NINE_DB_PATH`)** — The SQLite database file the
daemon opens, creating it and its parent directory if absent. Defaults to
`/data/nine.db` when the container's `/data` volume is present, else
`~/.nine/nine.db`. The daemon fails fast if the file cannot be opened.

---

## CLI & TUI

**`nine <message>`** — One-shot client call; auto-starts the daemon if
needed and continues the same conversation thread across calls.

**`nine attach <agent-id>`** — Reconnect to a specific conversation/task/goal
session by its agent ID (restores from checkpoint if not already running).

**`nine status` / `/status`** — Daemon uptime, active agent count, loaded
plugins (and tool counts).

**`nine goals` / `nine reflections` / `nine workflows`** —
List goals (with their sub-goals), idle-reflection history,
and active/recent workflows respectively. TUI equivalents: `/goals`,
`/reflections`, `/workflows`. (There is no `tasks` verb.)

**`nine workflow stop <id>`** — Live cancellation of an ongoing workflow
(daemon must be running): marks it `cancelled`, pending steps `skipped`,
running steps `failed` (`stopped`). In-flight sub-agents finish but their
results are discarded.

**`nine workflow fail <id> | --all`** — Post-mortem cleanup; marks
workflow(s) `failed`. Works even if the daemon is down (runs the memory
plugin as a one-shot process).

**Startup scrub (`internal.workflow.scrub`)** — On every daemon start, marks
all `running` workflow steps as `failed` (`interrupted`) and auto-closes
workflows whose steps are now all terminal; workflows with remaining
`pending` steps stay `active` for the LLM to resume.

**TUI slash commands** — `/help`, `/sessions`, `/status`, `/config`,
`/context [id]`, `/plan-mode <mode>`, `/goals`, `/workflows`,
`/tools [filter]`, `/skills [name]`, `/memory [key]`, `/new`,
`/think <message>`, `/clear`. Handled locally — no LLM tokens consumed
(`/think` is the exception: it sends a real turn). Typing `/` opens a picker
that filters the list as you type. See
[CLI Usage § TUI Slash Commands](usage.md#tui-slash-commands).

---

## Protocol

**`internal/protocol`** — Package holding the daemon/client wire types
(`Msg`, `ProgressEvent`, `StatusInfo`) and the client (`EnsureDaemon`, typed
request methods), kept separate from the runtime so client code doesn't pull
in the whole daemon. Messages are newline-delimited JSON over the Unix
socket. See [Daemon Architecture § Protocol](daemon.md#protocol).

**Progress events** — Streamed during a turn: `tool_start`/`tool_end` (tool
call lifecycle), `context_update` (token usage vs. budget), `response_chunk`
(streamed text). Final `response` + `done` sent when the turn completes.
