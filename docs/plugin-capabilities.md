# Plugin capabilities

A plugin gets three capabilities from the daemon, all carried on the existing
daemon-to-plugin transport:

| Capability | Shape | Section |
|------------|-------|---------|
| Pass-through settings | `[plugin.<name>.settings]` in `nine.toml` becomes environment variables at spawn. Nine declares no schema, so an operator can configure a third-party plugin without rebuilding Nine. | §3 |
| Cache directory | A per-plugin scratch dir created by the manager and handed over as `NINE_PLUGIN_CACHE_DIR`. Wiped when the plugin exits unless `persist_cache = true`. | §4 |
| Long-running jobs | A tool call may return a job id instead of a result. The daemon records the job, polls the plugin for status, and surfaces completion on a later turn. The model gets `job_wait`, `job_check` and `job_list`. | §5 |

The plugin contract carries two methods for this (`plugin.job_status`,
`plugin.job_cancel`), one describe flag, one call-result field, and two
environment variables. All are optional; a plugin that uses none is unaffected.

The normative contract is `spec/contracts/plugin.md` (R-PLUG.10/11/12) and the
authoring guide is [plugins.md](plugins.md). This document explains the
mechanism and the reasoning behind it.

---

## 1. Design constraints

All three capabilities are additive and ride the existing daemon-to-plugin
transport. No new socket, no reverse channel, no capability tokens.

Two deviations from the original design, both behaviour-preserving:

- The sweeper polls on an age-based backoff computed in SQL (`JobsDueForPoll`)
  rather than per-job timers.
- `job_wait` blocks on a sweeper-signalled `JobWaiters` channel with a poll
  fallback, because the eval harness wires no signaller.

---

## 2. No reverse channel

Rev 1 proposed a `host.*` RPC surface on a second Unix socket that plugins would
dial to read and write Nine's memory, authenticated by a per-process capability
token. **That is removed in full** — read *and* write, socket, token, grant model,
and the `memory_scope` config.

**The transport stays one-directional: the daemon dials the plugin, never the
reverse.** Nothing about how Nine talks to plugins changes.

The one thing that channel was genuinely needed for was a plugin saying *"the
long job is done."* Without it, that signal becomes **daemon-pulled**: the plugin
hands back a job id and keeps the answer until it is asked for it, and the daemon
asks — over `plugin.job_status`, on the socket it already owns. §5 is built
around that inversion. It is a strictly better fit for the rest of the system:
`event-journal.md` already argues that a background result should *enrich
a later turn's context*, not interrupt a live one, and a pull loop is exactly
that discipline expressed in the transport.

Second-order consequences of the removal:

- Plugin-private durable state — the rev-1 "case C" — is now served by the
  **cache dir** (§4) rather than a memory namespace. This is the better home:
  plain files, no schema, no risk of a plugin's private data ever being reachable
  by the context builder.
- `tool-output.md` mentions a deferred `host.memory.set` as a possible
  socket-throughput optimization. That option no longer exists; a plugin with a
  large result returns it and the daemon spills it, which is the shipped path.

---

## 3. Per-plugin settings from `nine.toml`

### The constraint

Operators install plugins Nine has never heard of. Today the only per-plugin
environment comes from a hard-coded switch in `factory`:

```go
func (cfg *Config) PluginEnvs(name string) []string {
	switch name {
	case "files", "shell":   // NINE_WORKSPACE
	}
}
```

A user plugin gets **nothing** — a weather plugin needing an API key has no way
to receive one short of recompiling Nine. Any design that requires Nine to
declare a plugin's config keys fails the same way. So the settings surface must
be **schema-less on Nine's side**: a bag of keys Nine copies through without
interpreting.

### Config shape

A singular `[plugin.<name>]` table per plugin, sibling to the plural `[plugins]`
subsystem table (the same singular/plural split `[[agent]]` already uses):

```toml
[plugin.weather]
persist_cache = false                    # §4

[plugin.weather.settings]                # verbatim env vars, uncapped, untyped
WEATHER_API_KEY = "sk-…"
UNITS           = "metric"
TIMEOUT_MS      = 5000

[plugin.files.settings]
NINE_WORKSPACE = "/srv/data"             # overrides the built-in default
```

- `<name>` is the plugin name as `nine plugins` reports it: the name for
  built-ins (`files`, `shell`, `http`, `time`), `mcp:<server>` for an MCP
  server, the manifest `name` for user plugins.
- In Go: `Plugin map[string]PluginEntry \`toml:"plugin"\`` on `Config`, with
  `PluginEntry{ PersistCache bool; Settings map[string]any }`. The map keys are
  whatever the operator wrote — no field-by-field decoding, so no recompile.

### Key and value rules

- **Keys are used verbatim as environment-variable names**, so an operator can
  match exactly what a plugin's README documents. A key that is not
  `[A-Za-z_][A-Za-z0-9_]*` is a **config error** at load, not a silent skip.
- **Values are TOML scalars**, stringified: strings as-is, integers/floats via
  `strconv`, booleans as `"true"` / `"false"`. A table or array value is a config
  error — env vars are strings, and inventing an encoding for structured values
  invites two plugins to disagree about it.
- **Merge order** (later wins): inherited OS environment → manager env
  (`NINE_BIN`) → Nine-owned vars (`NINE_PLUGIN_SOCKET`, the §4 cache vars) →
  built-in defaults from `PluginEnvs` → operator `settings`. This is expressed as
  **append order**: the manager builds `cmd.Env` as `os.Environ()` followed by each
  later group, and `os/exec` resolves a duplicate key to its **last** occurrence,
  so later groups win by construction — there is no explicit dedup to get wrong.
  Operator settings beat built-in defaults, which is what lets an operator
  override a `NINE_`-prefixed default such as `NINE_WORKSPACE` through
  `settings`.
- **Reserved keys.** The three vars Nine computes freshly per spawn are off-limits
  to `settings`, because overriding one breaks the transport or the cache contract
  rather than merely changing a default. The reserved set is exactly
  `NINE_PLUGIN_SOCKET`, `NINE_PLUGIN_CACHE_DIR`, and `NINE_PLUGIN_CACHE_PERSISTENT`
  — **not** the whole `NINE_` prefix, so `NINE_WORKSPACE` and other `PluginEnvs`
  defaults stay overridable. A `settings` key naming one of the three is a **config
  error** at load, not a silent drop.
- **Secrets.** Values routinely hold API keys. They are never logged (the
  manager logs key *names* at debug, never values), and `nine plugins` shows
  names only. Same-uid processes can read another process's environment; that is
  the accepted bound, and it is documented rather than papered over.

### Consequences for the existing code

- `PluginEnvs(name)` shrinks to *defaults only* and is then applied to **every**
  plugin, including user plugins, which today receive no environment at all.
- The call sites in `daemon` collapse: every `TryStart(name)` passes
  `cfg.PluginEnvs(name)...`, and `LoadUserPlugins` grows a way to reach config
  (pass a `func(name string) []string` into the manager rather than threading
  `*config.Config` into `plugin`, which must stay config-agnostic).
- **Settings are read at spawn.** Editing `nine.toml` does not reach a running
  plugin; a user plugin picks it up on `nine plugins reload`, a built-in on
  daemon restart. A `plugin.configure` RPC for hot reload is possible later and
  is deliberately not in scope.

---

## 4. The plugin cache directory

### What it is

A directory per plugin process that the plugin may use for anything — partial
downloads, extracted archives, a SQLite file, a cursor, an expensive cached
lookup. Nine creates it, hands over the path, and deletes it. Nine **never reads
it**: it is opaque scratch, not a store. It is not the memory file store, it is
never embedded, and nothing in it can reach the model's context except by the
plugin returning it from a tool call.

### Config

```toml
[plugins]
cache_dir = "~/.cache/nine/plugins"   # root for all plugin cache dirs

[plugin.scanner]
persist_cache = true                   # default false
```

Default root (**decided**): the OS user cache dir (`os.UserCacheDir()` →
`~/.cache/nine/plugins` on Linux, `~/Library/Caches/nine/plugins` on macOS),
overridable by `NINE_PLUGINS_CACHE_DIR` for the container layout — note the
**plural**: `NINE_PLUGINS_CACHE_DIR` overrides the shared *root* for every plugin,
and is a different variable from the singular `NINE_PLUGIN_CACHE_DIR` that each
plugin receives pointing at *its own* dir under that root (see *Environment*
below). The root must be a durable location,
not `/tmp`, because persistent caches live under the same root. The macOS
`sun_path` limit does not apply — this is a directory, not a socket.

### Layout and lifecycle

| Mode | Path | Created | Removed |
|---|---|---|---|
| **Ephemeral** (default) | `<cache_dir>/<name>.<rand8>/` | by the manager before spawn, mode `0700` | on `Manager.Stop` / `StopAll`, and by a **boot sweep** of leftover `<name>.<rand>` dirs |
| **Persistent** (`persist_cache = true`) | `<cache_dir>/<name>/` | same | **never by Nine** — the operator owns it |

The random suffix on ephemeral dirs does two jobs: it keeps two Nine instances
(or a reload overlapping a shutdown) from sharing a scratch dir, and it makes the
boot sweep safe — anything matching `<name>.<hex>` is by definition disposable, so
a dir orphaned by a hard-killed daemon is reclaimed on the next boot rather than
leaking forever. It mirrors `allocSocketPath`'s existing `crypto/rand` pattern.

**The boot sweep is the primary reclaim path, not a backstop.** `daemon`
installs no signal handler and never calls `StopAll` today, so on any exit other
than a terminal-delivered SIGINT the plugin children are orphaned and `Stop` — and
with it the dir removal — never runs. Phase 6 adds graceful shutdown, which makes
removal-on-stop the common path; until then, and after any crash, the sweep is
what enforces "wiped on exit". One consequence to accept either way: an orphaned
plugin from a previous daemon keeps writing into a dir the new daemon has already
swept. That is harmless — the writes go to an unlinked directory nothing will read
— and it is a symptom of the orphan, not of the cache design.

### Environment

- `NINE_PLUGIN_CACHE_DIR` — absolute path, guaranteed to exist and be writable.
- `NINE_PLUGIN_CACHE_PERSISTENT` — `0` or `1`, so a plugin can tell whether it is
  worth writing a cursor at all (and skip the work of reading one back that will
  never be there).

`Probe` (used by `nine plugin validate` and user-plugin vetting) always allocates
an **ephemeral** dir regardless of `persist_cache`, and removes it with the
process — validating a plugin must never create or touch persistent state.

### Why not memory

The rev-1 design put plugin-private state in a memory namespace, which needed a
grant model, a scope prefix, and a promise that the context builder would never
surface that namespace. A directory needs none of that: it is outside the store
entirely, so there is no namespace to accidentally expose and no schema to agree
on. Files are also simply the right primitive for a half-downloaded tarball.

---

## 5. Long-running work

### The problem

`plugin.call` is synchronous: the daemon issues an HTTP request and waits. A
20-minute download therefore holds the turn open to the daemon's
`task_timeout_seconds` (default 1800), keeps the model idling on a tool result,
and — for a plugin advertising `max_concurrent = 1`, like an MCP bridge —
occupies the plugin's only connection so nothing else can reach it. There is no way to
say "started, ask me later."

### The shape: return a handle, let the daemon poll

1. A tool handler starts the work on its own goroutine and returns **immediately**
   with a `job_id` and a one-line acknowledgement instead of a result.
2. The daemon records the job in the database, keyed to the conversation that started
   it, and returns the model a short observation naming a **job handle**.
3. A daemon-side poller asks `plugin.job_status` on a backoff until the job
   reaches a terminal state.
4. On completion the output is capped-or-spilled through the existing spill sink
   and a notification is posted to the owning conversation, so the **next turn**
   is told even if nothing ever waited.

The model chooses its own posture: block with `job_wait`, or move on and check
later with `job_check`. Both are supported, and the registry row is what makes
"later" work across turns and restarts.

### Contract additions (plugin side)

| Addition | Where | Meaning |
|---|---|---|
| `async_jobs: true` | `plugin.describe` result | The plugin can run detached work and answer status queries. The daemon **rejects a `job_id` from a plugin that did not advertise it** — fail-closed against version skew. |
| `job_id: "…"` | `plugin.call` result | The call did not produce a result. `output` carries a one-line ack (`"started download of ubuntu-24.04.iso"`) that the model sees verbatim. |
| `plugin.job_status` | new method | `{"job_id":"…"}` → `{"state":"queued\|running\|done\|failed\|cancelled","progress":"…","output":"…","error":"…"}`. `cancelled` is terminal and distinct from `failed`. `progress` is an optional free-text one-liner (`"41% · 1.2 GB/2.9 GB"`) — see below. |
| `plugin.job_cancel` | new method | `{"job_id":"…"}` → `{"cancelled":true}`. Best-effort; the daemon calls it on explicit cancel and on shutdown. Once a cancel takes effect the job is terminal and `job_status` reports `cancelled` (not `failed`); the daemon mirrors that state into the registry row. |

Unknown job ids answer `failed` with a clear error rather than an RPC error, so a
daemon that restarted and lost track cannot wedge on a missing key.

**`progress` is one free-text string, not a structured `{percent, detail}`.** Its
only consumer in v1 is the model, via `job_check` / `job_list` and the context
line — and a model reads `"41% · 1.2 GB/2.9 GB"` exactly as well as it reads a
parsed struct, while a struct forces every plugin to express its progress as a
percentage even when it has no total (a log tail, a scan of an unknown number of
hosts). A plugin that knows a percentage simply puts it in the string. If the TUI
later wants a real progress bar, an optional `percent` field is additive and can
land then, with the text staying authoritative for display.

**MCP servers get none of this.** `mcpClient.call` translates `plugin.call` into
`tools/call` and collapses the reply to `CallResult{Output: text}`, and MCP has no
status or cancel notion to map `job_status` / `job_cancel` onto. Jobs are
native-plugin-only. Settings (§3) and the cache dir (§4) *do* apply to MCP —
`StartMCP` already takes `extraEnv` and appends it to the process environment, so
both are one line each.

### SDK ergonomics

Plugin authors must not hand-roll goroutine bookkeeping. The SDK gains a job
registry and a second handler shape, wired through `Serve`:

```go
jobs := plugin.NewJobs()

plugin.Serve(tools, handlers,
	plugin.WithJobHandlers(map[string]plugin.JobHandler{
		"download_file": func(ctx context.Context, args json.RawMessage) (plugin.Job, error) {
			var req struct{ URL string `json:"url"` }
			// … validate synchronously; a bad argument is a normal error, not a job …
			return plugin.Job{
				Ack: "started download of " + req.URL,
				Run: func(ctx context.Context) (string, error) { return download(ctx, req.URL) },
			}, nil
		},
	}),
	plugin.WithJobs(jobs),
)
```

`Serve` sets `async_jobs`, allocates the id, answers `job_status` / `job_cancel`
from the registry, and — the detail most likely to be got wrong by hand — runs
`Job.Run` under a **context detached from the HTTP request**. The request context
dies when the reply is written; a job inheriting it would be cancelled the
instant it started. Cancellation comes only from `plugin.job_cancel` or process
exit.

Validation stays synchronous: a malformed argument returns an ordinary error from
the handler, so the model gets it immediately instead of via a job that fails a
second later.

### Many agents, one plugin process

A plugin is a **singleton per daemon**: every session and sub-agent shares the one
process. Concurrent calls are already the normal case (`http.Serve`, one goroutine
per request), and jobs inherit that — two agents starting the same async tool get
two ids and two goroutines. Three consequences have to be handled deliberately.

**Jobs must not bypass `max_concurrent`.** The daemon enforces that cap as
`MaxConnsPerHost` — it bounds *in-flight HTTP requests*, not work. A job start
returns immediately, so the connection frees at once and the cap stops meaning
anything: three agents could have three jobs running inside a plugin that
advertises `max_concurrent: 1` precisely because it owns one shared resource.
So **`NewJobs` honours the plugin's declared cap itself**, running jobs through a
worker pool of that size (unbounded when the plugin declared none). Jobs beyond
it sit in a new `queued` state, reported as such by `job_status` so the model can
tell "waiting its turn" from "running but silent". This is the one place the SDK
must not simply mirror the daemon's cap — it has to re-enforce it, because the
daemon structurally cannot.

Note the flip side: for the *synchronous* calls a `max_concurrent = 1` plugin
serves today, jobs are a straight improvement. A 20-minute call currently occupies
the plugin's only connection and locks every other agent out of it; the same work
as a job frees the connection immediately.

**Each job gets its own directory.** One process means one cache dir (§4) shared
by every agent's jobs, so two concurrent downloads writing `download.tmp` would
clobber each other. The SDK hands each job `<cache_dir>/jobs/<job_id>/`, created
on first use — collision-free by construction, and no burden on the author to
remember to namespace.

**The plugin-side registry evicts.** Terminal jobs are kept for a TTL (1h
proposed) and then dropped along with their job dir, so a long-lived plugin does
not accumulate finished jobs forever. The daemon polls at worst every 30s, so
collection happens well inside that window; a job evicted before collection reads
back as an unknown id, which already answers `failed` with a clear reason.

What stays the plugin author's problem is unchanged from today: a handler touching
shared mutable state must be safe for concurrent use, or the plugin must advertise
a finite `max_concurrent`. Jobs add no new requirement there — they just make the
existing one actually hold.

### Daemon side: the registry

A new `jobs` table in `memory` (like everything else
persistent):

> **Now shared with the tool backend.** The table is `jobs` rather than
> `plugin_jobs`, and a `backend` column says whether a row is a plugin's detached
> goroutine or a resumable sandboxed tool the daemon calls itself
> (`spec/contracts/toolvm.md` R-TVM.19). Everything below still describes the
> plugin backend; the model-facing surface is identical for both.

| Column | Purpose |
|---|---|
| `handle` | the daemon-side id the model sees (`job_7f3a`) — stable and unique across plugins |
| `plugin`, `tool`, `plugin_job_id` | where to poll |
| `owner_id` | who to notify, and whose `job_list` it appears in. One column, not two: the notification path already treats the agent id and the conversation id as the same value (`runtime/store.go` passes `agentID` into `NotificationCreate`'s `conversationID`, and `AgentWorker` reads its own `w.id` back). Every job has one — a tool call only ever happens inside a turn. |
| `state` | `queued` \| `running` \| `done` \| `failed` \| `cancelled` \| `lost` |
| `ack`, `progress` | what to show without fetching the result |
| `output`, `spill_path`, `error` | the terminal result; large output goes to the file store, not the row |
| `created_at`, `updated_at`, `finished_at` | age, timeout enforcement, cleanup |

The model never sees the plugin-side id; the handle indirection keeps ids unique
across plugins and stable if a plugin reuses its own numbering after a restart.

A single daemon-level sweeper (a component alongside the existing subscribers,
not a goroutine per job) polls every job in `running`: every 2s for the first
minute, backing off to 30s (`[plugins] job_poll_seconds` caps it). On a terminal
state it runs the output through the dispatcher's cap-or-spill path, writes the
row, journals an event, and posts a `NotificationCreate` to the owning
conversation — which `AgentWorker.prependNotifications` already delivers on the
next turn.

### Model-facing tools

Core-intercepted tools in `agent` (registered like
`RegisterNotifyUser`), available whenever the job registry is wired:

| Tool | Blocking | Behaviour |
|---|---|---|
| `job_wait` | **yes** | Waits for `handle` up to `timeout_seconds` (default 60, capped by the turn's remaining budget). Returns the result if it lands, otherwise `"still running"` plus the current progress — never an error. |
| `job_check` | no | Current state, progress, and the result if terminal. |
| `job_list` | no | Outstanding jobs for this conversation: handle, tool, age, progress. |
| `job_cancel` | no | Best-effort stop via `plugin.job_cancel`. |

`job_wait` blocks on a channel the sweeper signals, not on its own poll loop, so
ten waiters cost one poller. A timed-out `job_wait` is deliberately *not* a
failure: the model is expected to shrug and carry on, which is the whole point of
supporting both postures.

**Naming.** `job_*`, not `task_*`: "task" already means the daemon's per-turn unit
(`task_timeout_seconds`) and reads like a sub-agent delegation. A *job* is
detached work inside a plugin process. `glossary.md` gains the entry.

### Remembering across turns

Three independent mechanisms, because the model may never block and may never be
asked again:

1. **The registry row** is in the database, so the fact survives the turn, the
   session, and the daemon.
2. **Completion posts a notification**, so the next turn in that conversation is
   told without having to ask.
3. **The context builder surfaces outstanding jobs** as one compact line each
   (handle · tool · age · progress), capped at a small N and omitted entirely
   when there are none. This is the piece that makes "the LLM must remember
   something is ongoing" true by construction rather than by discipline — a model
   that forgot about a job still sees it in context next turn.

**Delivery is pull-only: a finished job never wakes anything.** It waits for the
owner's next turn, which always comes:

- an **interactive conversation** delivers on the human's next message;
- a **standing/goal agent** already runs on its own cadence — `armIdleTimer` /
  `handleIdle` fire its plan's idle routines on their `interval` or `schedule` — so
  it picks the notification up on the next tick without any new wake path.

This is `event-journal.md`'s discipline applied unchanged: a background
result enriches a later turn, it does not interrupt a live one. If a case ever
demands sooner delivery the mechanism is already there (re-arm the idle timer on
completion), so this is a one-line change later, not an architectural bet.

The one gap is an owner that will genuinely never take another turn — a session
stopped or archived while its job was still running. There the daemon posts to
the **human-facing feed** (`UserNotificationCreate`, readable with
`nine notifications`) instead, and the same applies to rows marked `lost` at boot
whose owner is gone. The common instance of this is a **sub-agent** owner: a
delegated turn that started a long job and then returned to its parent is exactly
such an owner — it takes no further turn of its own, so its completions route to
the human feed by the same rule rather than to a turn that will never come. (A
future refinement could instead deliver a sub-agent's job result back to its
*parent* conversation, but that needs a parent link the registry does not carry
today, so the human feed is the v1 answer.) Otherwise the human feed stays
untouched: whether a finished
job is worth telling a human about is the agent's judgement, made with
`notify_user` on its next turn, exactly as `predefined-agents.md` §5 has it.
A plugin cannot reach the human's feed on its own — that would be the pushy
reverse channel §2 removed, arriving by another door.

### Failure, restart, limits

- **A job never survives a daemon restart — but it is not reliably killed
  either.** The job lives as a goroutine inside the plugin *process*, and what
  happens to that process depends on how the daemon died:

  | Daemon exit | Plugin process | The job |
  |---|---|---|
  | Ctrl-C in a terminal | SIGINT reaches the whole process group; `serve.go` exits | dies with it |
  | `kill <pid>`, service restart, crash | **orphaned and still running** — no signal handler, no `StopAll`, no `Setpgid` | keeps running, unreachable: the new daemon spawns a fresh plugin on a fresh socket and can never ask the old one for status |

  Either way the answer is unreachable, so on boot every row still in `running` is
  marked `lost` with a reason and a notification goes to the owning conversation.
  Nine does not pretend otherwise. **Phase 6 adds the graceful shutdown that is
  missing today** — a signal handler that calls `plugin.job_cancel` for every
  running job, then `Manager.StopAll` — which turns the orphan row of that table
  into the first row and stops leaking plugin processes and their sockets. That
  gap exists in `main` right now, independent of this feature; jobs only make it
  more expensive.
- **Resume after restart** — a plugin that persists job state to a persistent
  cache dir (§4) *could* answer `job_status` for an id from a previous process.
  That is a real interaction between two of these features, and it is
  **deferred**: it needs a plugin-side durability contract that no current use
  case justifies.
- **A cancelled turn does not cancel a job.** Outliving the turn is the feature.
  Only `job_cancel` and daemon shutdown stop one.
- **Limits.** `[plugins] job_max_seconds` (default 3600) marks an over-age job
  `failed` and attempts a cancel. `max_jobs_per_conversation` (default 8) caps the
  outstanding jobs a conversation may hold — but the plugin has already started the
  goroutine by the time its `job_id` reaches the daemon, so the cap is enforced
  **on admission, not by refusal**: a `job_id` that would exceed the cap is
  immediately `plugin.job_cancel`'d, no registry row is written, and the model gets
  a clear over-limit error in place of a handle. A looping model therefore cannot
  accumulate a hundred *live* jobs, though it can briefly start-and-kill them — a
  plugin that treats cancel as a hard stop wastes no real work. (Counting the id
  before starting the work would need a pre-flight admission handshake the
  transport does not have; the post-hoc cancel is the price of the plugin owning
  when work begins.)

---

## 6. Contract, version, and spec impact

Everything here is **additive** to the plugin contract:

- `plugin.describe` result gains `async_jobs`.
- `plugin.call` result gains `job_id`.
- Two new methods: `plugin.job_status`, `plugin.job_cancel`.
- New env vars at spawn: `NINE_PLUGIN_CACHE_DIR`, `NINE_PLUGIN_CACHE_PERSISTENT`,
  plus arbitrary operator settings.

Because it is additive, a v1 plugin remains *fully functional* except that it
cannot start jobs — it ignores the new env vars and never returns a `job_id`. But
`checkProtocolVersion` demands **exact equality** today, so a naive bump to v2
would reject every existing plugin binary for no reason. **Decided: bump
`ProtocolVersion` to 2 and widen `checkProtocolVersion` from exact equality to a
supported set `{1, 2}`**, with v1 plugins treated as lacking async jobs. The
"predates versioning" (0) rejection stays as-is.

Docs and spec to update (via `/sync-nine`): `spec/contracts/plugin.md` (R-PLUG.1
gains the two methods; new rules for settings, cache dir, and jobs),
`plugins.md` (authoring: settings, cache dir, the job SDK),
`configuration.md` (`[plugin.<name>]`, `cache_dir`, job knobs),
`glossary.md` (*job*), `versioning.md` (the protocol bump), and this
note flipping to Implemented. `spec/contracts/wire-protocol.md` is **untouched** —
none of this reaches the daemon↔client protocol.

---

## 7. Build order

All seven phases shipped. They were built in this order, each independently
useful: settings pass-through, cache dir, the plugin-side job SDK, the
daemon-side job registry, the model-facing job tools, hardening and graceful
shutdown, then docs and spec.

Graceful shutdown is the part worth knowing about operationally: on SIGINT or
SIGTERM the daemon cancels every running job and then calls `Manager.StopAll`.
Without it a `kill` orphans every plugin process along with its jobs, cache dir
and socket.

---

## 8. Eval scenarios

The unit and integration tests prove the *mechanism* works — a plugin can
detach work, the daemon can poll and surface it. What they do not exercise is the
**model behaviour** the feature exists to shape: posture, memory, and escalation.
Those belong in the in-process eval harness (`evals.md`), one scenario each,
asserting on the transcript rather than on daemon state:

1. **Posture — wait vs move on.** Given a job whose ack reads as fast, the model
   `job_wait`s; given one described as long (a large download), it starts the job
   and continues with other work instead of blocking. Asserts the model *reads the
   ack and chooses* rather than always doing one thing.
2. **Timeout is not failure.** A `job_wait` that times out returns `"still
   running"` plus progress; the model carries on — reports progress, moves to the
   next step — rather than treating it as an error or retrying in a tight loop.
3. **Memory by context, not discipline.** After a turn boundary with a job still
   outstanding, the model — reminded only by the context-builder line — reports the
   job as in-flight and does **not** loop on `job_check` or claim it forgot. This
   pins the §9 decision that surfacing outstanding jobs into context makes
   remembering structural.
4. **Escalation is the agent's call.** On a finished job the model uses
   `notify_user` to tell the human when the result warrants it, and stays silent
   when it does not — it never reaches for a plugin-side push, which does not exist
   (§2).
5. **Completion reaches the next turn.** After a job the model never waited on
   completes, the following turn's context carries the notification and the model
   acts on the result without having been told to poll — the pull-only delivery
   path (§5) exercised end to end.

Each maps to an assertion made elsewhere in §5/§9; together they are the
acceptance bar for the model-facing half, distinct from the plugin/daemon unit
tests that only prove the mechanism runs. They land with phase 5 (the tools) and
phase 6 (completion delivery), and `/sync-evals` reconciles the harness after.

---

## 9. Decisions taken

- **The host API is dropped entirely** — no reverse channel, no second socket, no
  capability tokens, no plugin access to memory in either direction. The daemon
  dials the plugin and nothing dials the daemon.
- **Completion is pulled, not pushed.** The plugin holds the answer; the daemon
  asks. This follows the same discipline as `event-journal.md`.
- **Plugin config is schema-less pass-through**, because operators install plugins
  Nine has never heard of and must not have to rebuild Nine to configure them.
- **Settings keys are verbatim env-var names**, so they match what a plugin's own
  documentation says, and operator settings beat Nine's built-in defaults.
- **Plugin-private state is a directory, not a memory namespace** — no grants, no
  scope prefix, no risk of leaking into the agent's context.
- **Ephemeral is the default; persistence is opt-in per plugin**, and the random
  suffix plus boot sweep make "wiped on exit" hold even after a hard kill — which
  today is the *only* thing that makes it hold, since nothing stops plugins on
  shutdown (§5, phase 6).
- **A job is lost, never resumed, across a daemon restart** — the row is marked
  `lost` and the human is told, rather than the daemon guessing at a plugin
  process it can no longer reach.
- **Both waiting postures ship together.** Blocking (`job_wait`) and
  non-blocking (`job_check`) are two views of one persisted registry, not two
  mechanisms.
- **Outstanding jobs are surfaced into context**, so remembering an in-flight job
  is structural rather than something the model must choose to do.
- **`job`, not `task`** — `task` is already the per-turn unit and reads like a
  sub-agent delegation.
- **Job delivery is pull-only** — completion never wakes an idle session, because
  every owner already takes another turn on its own (a human's next message, or a
  standing agent's idle tick).
- **The human feed is the agent's call, not the plugin's** — the daemon posts
  there only when the owning session is gone and the notification would otherwise
  be lost.
- **MCP gets settings and a cache dir, not jobs** — the MCP adapter has nowhere to
  carry a job id, and MCP has no status/cancel to map onto.
- **`progress` is free text**, because the model is its only reader and not every
  job has a denominator.
- **The SDK re-enforces `max_concurrent` for jobs**, because the daemon's cap is
  on connections and a job start returns instantly — the daemon structurally
  cannot hold that line once work outlives its request.
- **Every job gets its own directory** under the shared cache dir, since one
  plugin process serves every agent.
- **Reserved env keys are the three transport/cache vars, not the `NINE_` prefix**
  — `NINE_PLUGIN_SOCKET`, `NINE_PLUGIN_CACHE_DIR`, `NINE_PLUGIN_CACHE_PERSISTENT`
  are rejected in `settings`; `NINE_WORKSPACE` and other defaults stay overridable.
- **`cancelled` is a terminal state distinct from `failed`**, returned by
  `plugin.job_status` after a successful `job_cancel` and mirrored into the row.
- **`max_jobs_per_conversation` is enforced on admission, not by refusal** — the
  plugin starts the work before the daemon sees the id, so an over-cap job is
  cancelled post-hoc rather than prevented.
- **A sub-agent's job completion goes to the human feed**, because a returned
  sub-agent takes no further turn of its own to receive it (parent-delivery is a
  deferred refinement).

## 10. Limits

No open questions outstanding. Q1 (`cache_dir` default) and Q2 (protocol version) are
settled in §4 and §6; Q3 (waking an idle owner) and Q4 (jobs with no live
conversation) in §5 *Remembering across turns*; Q5 (MCP) and Q6 (progress
granularity) in §5 *Contract additions*.

Two things are deliberately **deferred**, each recorded where it belongs rather
than left as a question:

1. **Resuming a job across a daemon restart** (§5) — possible only for a plugin
   that persists its own job state to a persistent cache dir, and it needs a
   plugin-side durability contract no current use case justifies.
2. **Hot-reloading settings into a running plugin** (§3) — a `plugin.configure`
   RPC would do it; today a settings change takes effect on
   `nine plugins reload` (user plugins) or daemon restart (built-ins).
