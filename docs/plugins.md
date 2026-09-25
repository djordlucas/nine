# Plugins

A plugin is a **subprocess** that provides tools. It is the right shape for a
capability that genuinely needs the daemon's own authority on the host —
running shell commands is the example, and `shell` is the one built-in plugin
Nine ships.

Most capabilities do not need that. Reading and writing files, fetching over
HTTP and reading the clock are [sandboxed tools](sandboxed-tools.md): they run
inside the wasm tool host with only the capabilities they are granted, and no
subprocess at all. A tool that needs a workspace directory is given that
directory and nothing else, which is a much narrower grant than a process
running as the daemon's user.

Built-in plugins are fixed at build time — not generated or loaded at runtime.
`shell` is Go, compiled **into the `nine` binary** rather than shipped as a
separate executable; the daemon starts it by re-executing itself as
`nine plugin serve shell`, so it still runs as its own isolated process while
there is only one artifact to build and ship.

Capabilities Nine does not implement itself arrive two further ways, both of which present as ordinary plugins: an **MCP server** declared in `[[mcp.server]]` (see [MCP servers](#mcp-servers)), and a **user plugin** dropped in `[plugins].user_dir`. Browser automation is the worked example of the first — see [Browser Automation](browser.md).

**Withholding a plugin.** `[plugins] disabled = ["shell"]` (or `NINE_PLUGINS_DISABLED=shell`) stops a plugin from ever starting — no process, no socket, no tools registered. It works by name and covers built-ins, MCP servers (as `mcp:<name>`), and your own plugins alike. This is how you run without `shell`, which executes arbitrary commands. A disabled plugin is reported by `nine plugins` as `off` rather than silently missing, so a tool that has gone absent is traceable to the decision that removed it. It is an operator setting read at boot; no agent or wire message can switch a plugin on or off.

Sharing the binary does not widen what a plugin process does. `plugin serve` is dispatched before nine's normal startup, so a plugin child loads **no config file** (the operator's `nine.toml` carries the embeddings API key and every other plugin's settings), writes **no** `nine.log`, and holds no way to start a daemon or TUI. Its output goes to stderr, which the daemon captures. Run by hand without `NINE_PLUGIN_SOCKET`, it refuses to start.

Several tools — memory, semantic search, workspace listing, and skills — are **core-intercepted**: built directly into the agent loop rather than served by a subprocess or run in the sandbox. See [Core-intercepted tools](#core-intercepted-tools) below.

---

## Built-in plugins

### `shell` — run shell commands

| Tool | Description |
|------|-------------|
| `shell` | Execute a shell command with a configurable timeout. Returns stdout, stderr, and exit code. |

**Example prompt:**
```
Run "ls -la /tmp" and tell me the five largest files.
```

**Notes:**
- Commands run in the workspace root (`workspace.root` / `NINE_WORKSPACE`), so a
  relative path names the same file for `shell` as it does for `read_file` and
  `write_file`. With no workspace configured, commands run in the daemon's own
  working directory.
- Commands run as the daemon's process user.
- Default timeout is 30 seconds.
- Nine does not sandbox commands — use Docker or a restricted user for untrusted workloads.

---

## Shipped sandboxed tools

These are **not plugins**. They are first-party [sandboxed tools](sandboxed-tools.md#54-shipped-tools--first-party-in-the-binary)
compiled into the binary and run in the wasm host with a declared, operator-visible
grant — where as plugins they held the daemon's uid. `nine tools` lists them with
the grant each one resolved.

### Workspace files

| Tool | Description |
|------|-------------|
| `read_file` | Read a workspace file, or a `spill/...` path holding truncated tool output. `lines` or `offset`/`limit` read part of a large file; `line_numbers` prefixes each line with its number. Refuses a file that is not text. |
| `write_file` | Write content to a file, creating parent directories as needed. `mode=append` adds to the end; `if_unchanged` refuses the write if the file changed since it was read. |
| `edit_file` | Replace exact text in an existing file, leaving the rest unchanged. Never loads the file into context, so it works past the context window. |
| `move_file` | Move or rename a file. Relinks rather than copying. |
| `copy_file` | Copy a file, streaming rather than going through context. |
| `delete_file` | Delete one file or one empty directory. The file moves to the trash rather than being destroyed. |
| `trash_list` | List what is recoverable from the trash, newest first. |
| `restore_file` | Restore a trashed file. Never overwrites an existing file. |
| `diff_file` | Show what changed in a file, as a unified diff against the version before the last change. |

**Paths and the workspace root.** When a workspace root is configured
(`workspace.root` / `NINE_WORKSPACE`), paths resolve against it: a relative path
or one under the `/work` alias (e.g. `/work/out.txt`) maps to the root, and
`write_file` is confined to it — a genuine absolute path outside the root is
rejected. `read_file` also honours the `/work` alias but still reads other
absolute paths as given. With no workspace root configured, paths are used
as-is.

**Example prompt:**
```
Read /work/notes.txt and then write a summary to /work/summary.txt
```

---

### HTTP and web

| Tool | Description |
|------|-------------|
| `http_get` | Perform an HTTP GET request. Returns status code, headers, and body. |
| `http_post` | Perform an HTTP POST request with a JSON or form body. |
| `web_search` | Search the web. Uses DuckDuckGo by default (no key); Brave or SerpAPI optionally. |
| `web_page_read` | Fetch a URL and extract readable text (strips HTML, scripts, nav). |

**Example prompts:**
```
Fetch https://api.github.com/repos/golang/go/releases/latest and tell me the tag name.
Search the web for "golang context best practices" and summarize the top 3 results.
```

**Backends for `web_search`:**
- Unset — DuckDuckGo, no API key required (the default)
- `SEARCH_PROVIDER=brave` + `SEARCH_API_KEY` — Brave Search API
- `SEARCH_PROVIDER=serpapi` + `SEARCH_API_KEY` — SerpAPI

`web_search` and `web_page_read` are plain HTTP and see only the raw response. When a
browser MCP server is loaded ([Browser Automation](browser.md)), prefer its navigate and
snapshot tools for anything needing JavaScript, a login, or interaction.

---

### The clock

| Tool | Description |
|------|-------------|
| `time` | Return the current date and time in **UTC**, as ISO-8601 and a readable form. |

The system prompt instructs the agent to call this before any time-sensitive
research (news, current events, prices, status) so it never reasons from a stale or
assumed date.

**It reports UTC, where the plugin it replaced reported the daemon's local zone.**
A sandboxed tool has no more access to host state than any other: the guest
carries no timezone database, and a tool cannot learn the host's zone unless the
host confers it. That is the tier's constraint showing through, and the price of
the clock no longer running with the daemon's authority.

**Example prompt:**
```
What's the current date and time?
```

---

## Core-intercepted tools

These are wired directly into the agent loop and call the store in
process. There is no subprocess and no sandbox: they reach Nine's own
state without leaving it.

### Memory and search

| Tool | Description |
|------|-------------|
| `memory_get` | Retrieve a value from the key-value store by key. |
| `memory_set` | Store a value in the key-value store. |
| `memory_delete` | Delete a key from the key-value store. |
| `memory_list` | List keys in the key-value store, optionally filtered by prefix. |
| `file_search_text` | Full-text search over stored file content, including one spilled tool result by path. |
| `list_files` | List workspace files by prefix, glob, or what changed since a timestamp. |
| `memory_embed` | Embed text and store the resulting vector under a namespace and key. |
| `memory_query` | Embed a query and return the most semantically similar stored items from a namespace. |

**Example prompts:**
```
Remember that my AWS region is us-west-2.
What AWS region did I configure?
Store the output of this script as 'last_run_log'.
Find files related to database migrations.
```

---

### Skills

Also **core-intercepted** (backed by the `skills` table in the memory store, no
subprocess). Skills are markdown how-to notes; built-in ones are immutable, and
Nine can author its own. See [Skills](skills.md).

| Tool | Description |
|------|-------------|
| `skill_list` | List all skills with names, descriptions, and tags. |
| `skill_read` | Read a skill's full content by name. |
| `skill_write` | Create or replace one of Nine's own skills (refuses built-in names). |
| `skill_modify` | Update one of Nine's own skills (refuses built-in skills). |

---

## MCP servers

An [MCP](https://modelcontextprotocol.io) server is a capability Nine does not build.
Declare one in `nine.toml` and it becomes a plugin in every respect: its own process,
its own crash isolation, its own row in `nine plugins`, its own entry in
`[plugins].disabled`.

```toml
[[mcp.server]]
name    = "github"
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-github"]
[mcp.server.env]
GITHUB_PERSONAL_ACCESS_TOKEN = "ghp_..."
```

- **Tools are prefixed with the server name** — `github__create_issue`. Two servers
  that both expose `search` would otherwise collide, and a collision silently drops
  one depending on load order.
- **It appears as `mcp:github`** in `nine plugins`, crashes on its own without
  affecting the daemon or other servers, and is switched off with
  `[plugins] disabled = ["mcp:github"]`.
- **`env` goes to that server only.** Nine's own secrets are not inherited by it, the
  same as for every other plugin.
- **Two transports:** `command` spawns a local server and speaks stdio; `url` reaches a
  hosted one over streamable HTTP (below).

**Hosted servers.** A server reached at a URL instead of spawned uses `url` and
`headers` in place of `command` and `env`:

```toml
[[mcp.server]]
name = "hosted"
url  = "https://mcp.example.com/rpc"
[mcp.server.headers]
Authorization = "Bearer ..."
```

This is MCP's **streamable HTTP** transport: each request is a POST, and the server
answers with either a JSON body or an SSE stream, its choice per request. A session id
the server issues on connect is echoed on every later request. Exactly one of `command`
or `url` is set per server; mixing `env` with `url` (or `headers` with `command`) is a
config error rather than a silently ignored setting.

Two things to know before relying on one:

- **stdio is serial.** The bridge advertises `max_concurrent: 1`, because a stdio
  response can only be matched to its request by owning the stream for the whole round
  trip. Calls to one server queue.
- **Nine does not vet a server's arguments.** Declaring an MCP server is trusting it. See
  the security section of [Browser Automation](browser.md#5-security) for what that means
  in the case where it bites hardest.

**Browser automation** is the worked example: [Browser Automation](browser.md) walks
through Playwright's MCP server end to end.

---

## Adding a plugin

Plugins are part of the source repo and are compiled into the image at build time.
Nine cannot generate, build, or load a plugin at runtime — adding a capability means
adding a plugin to the tree and rebuilding.

A plugin you supply is a standalone binary (see [User Plugins](#user-plugins) below); Nine's own Go plugins skip the separate binary by registering in `builtins`. Either way the code is the same shape — `plugin.Serve` with a tool list and a handler map. Here is a minimal standalone example:

```go
package main

import (
    "context"
    "encoding/json"

    "nine/internal/plugin"
)

func main() {
    plugin.Serve(
        []plugin.ToolDefinition{
            {
                Name:        "greet",
                DisplayName: "Greet",
                Description: "Return a greeting for a given name.",
                InputSchema: plugin.Schema(`{
                    "type": "object",
                    "required": ["name"],
                    "properties": {"name": {"type": "string", "description": "Name to greet"}}
                }`),
            },
        },
        map[string]plugin.ToolHandler{
            "greet": func(_ context.Context, args json.RawMessage) (string, error) {
                var p struct{ Name string `json:"name"` }
                if err := json.Unmarshal(args, &p); err != nil {
                    return "", plugin.InvalidArgs("%v", err)
                }
                return "Hello, " + p.Name + "!", nil
            },
        },
    )
}
```

`plugin.Serve` (in `nine/internal/plugin`) handles the JSON-RPC loop: pass your
`[]plugin.ToolDefinition` (schemas as raw JSON via `plugin.Schema`) and a
`name → plugin.ToolHandler` map, and implement only the business logic. Return
`plugin.InvalidArgs(...)` for bad input.

To register the new plugin: add it under `plugins/<name>/`, add `<name>` to the
`PLUGINS` list in the `Makefile` and the build loop in the `Dockerfile`, start it
in `daemon` (`pluginManager.TryStart("<name>", …)`), and rebuild. Default
plugins are sub-packages of the core module and use the vendored dependencies.

### Taking large input by reference

A tool that consumes a large payload should not make the model paste it. Mark the
property with `"x-nine-ref": true` and the daemon will treat the argument as a
**file-store path**, substituting the stored content before your handler runs:

```go
InputSchema: plugin.Schema(`{
    "type": "object",
    "required": ["content"],
    "properties": {"content": {
        "type": "string",
        "x-nine-ref": true,
        "description": "File-store path whose content to parse."
    }}
}`),
```

Your handler still receives `content` as a plain string — now holding the bytes. The
model only ever handles the path, so a 400 KB payload costs it ~40 characters of
context. Paths typically come from a spilled tool result (`spill/<agent>/…`, see
[tool-output-spill.md](tool-output.md)), but any stored path works.

Only marked properties are expanded, so a tool whose arguments are genuinely paths is
unaffected. One expansion is capped at 8 MiB.

### Large output

You do not need to do anything about large *output*. A result over the dispatcher's cap
is spilled to the file store automatically and the model is handed a path — this applies
to every tool, including MCP servers, with no plugin involvement
([tool-output-spill.md](tool-output.md)).

---

## User plugins

Built-in plugins are baked into the image. **User plugins** are operator-supplied
executables Nine discovers at boot from `[plugins].user_dir` (env
`NINE_PLUGINS_USER_DIR`, wired to `/plugins.d` under Docker but not mounted —
add `-v /my/plugins.d:/plugins.d:ro` yourself, and note a plugin only runs in
the container if it was built for it). They are purely additive — built-ins are
never affected — and the directory is never required: empty or absent means "no
user plugins".

Discovery is boot-only, mirroring skills (`skills.md`), with a live
convenience path: `nine plugins reload` re-scans without a restart.

### Layout — sidecar manifest

A user plugin is a **pre-built executable** beside a `<name>.toml` manifest:

```
plugins.d/
  weather          the plugin executable (built however you like)
  weather.toml     name + entrypoint
```

```toml
name = "weather"
entrypoint = "./weather"   # resolved relative to the manifest
```

The manifest is the **gate**: a binary with no manifest beside it is never
executed. It declares intent only — the authoritative tool list still comes from
`plugin.describe` at load time — so it needs just `name` and `entrypoint`.

### Loading rules

At boot (and on reload), for each manifest in name order:

1. A malformed manifest or a missing binary is **skipped** — the binary is never
   run.
2. The binary is started and must pass the same handshake as a built-in
   (`plugin.describe` + a matching `ProtocolVersion`). A non-plugin is **skipped**.
3. Its tools must not collide with an already-loaded plugin — a built-in **or** an
   earlier user plugin (built-ins load first, so they always win). A collision
   **skips the whole plugin**; there is no overriding, ever.

Any single failure is logged at ERROR and surfaced in `nine plugins`, but never
aborts the others — one bad drop-in cannot take the daemon down.

### CLI

| Command | Effect |
|---------|--------|
| `nine plugins` | The live roster: built-in and user plugins with their tools, plus any skipped user plugins and the reason. |
| `nine plugins reload` | Re-scan `user_dir` and start/stop user plugins live. New turns pick up the new set; in-flight turns keep theirs. |
| `nine plugin validate [path]` | Run the load-time handshake locally (no daemon needed) against `user_dir`, a `.toml` manifest, or a binary — vet before you deploy. |

See `plugins.d/README.md` for an operator walkthrough.

---

## Plugin lifecycle

```
build time            built-in:  go build ./cmd/nine    (handlers linked into nine)
                      MCP:       nothing — the server is fetched or hosted elsewhere
     │
daemon start          built-in:  TryStartBuiltin → spawn `nine plugin serve <name>`
                      MCP:       one `nine plugin serve mcp` bridge per [[mcp.server]]
                      user:      spawn the manifest's entrypoint binary
                      then all:  plugin.describe → register tools
     │
[in use]              Dispatcher routes tool calls via plugin.call
     │
[crash]               Subprocess exit is isolated from the daemon; restart from the
                      existing binary is the plugin manager's responsibility
     │
[shutdown]            SIGTERM → wait for exit
```

---

## Environment variables passed to plugins

The plugin manager passes these to each subprocess:

| Variable | Value | Purpose |
|----------|-------|---------|
| `NINE_BIN` | `plugins.bin` from config | Directory of plugins that ship their own binary |
| `NINE_PLUGIN_SOCKET` | per-plugin socket path | Where the plugin listens |
| `NINE_PLUGIN_CACHE_DIR` | per-plugin scratch dir | Cache directory (below) |
| `NINE_PLUGIN_CACHE_PERSISTENT` | `0`/`1` | Whether the cache dir persists |

Individual plugins also receive their built-in defaults (`NINE_WORKSPACE` for
`shell`, so a relative path means the same file there as in the workspace tools)
plus any operator settings. An MCP bridge additionally receives
`NINE_MCP_SERVER`, the JSON spec of the one server it fronts.

### Operator settings (no rebuild needed)

An operator configures a plugin Nine has never heard of via a singular
`[plugin.<name>]` table in `nine.toml`. Everything under
`[plugin.<name>.settings]` is copied through to the process as environment
variables at spawn — Nine never declares a schema, so a third-party plugin's API
key or tuning is set in config, not code:

```toml
[plugin.weather.settings]
WEATHER_API_KEY = "sk-…"
UNITS           = "metric"

[plugin.shell.settings]
NINE_WORKSPACE = "/srv/data"   # operator settings override a built-in default
```

Keys are used verbatim as env-var names (a malformed key is a config error at
load); values are TOML scalars, stringified. `NINE_PLUGIN_SOCKET` and the two
`NINE_PLUGIN_CACHE_*` vars are reserved and cannot be set here. Settings are read
at spawn — edit and `nine plugins reload` (user plugins) or restart (built-ins)
to apply. Applies to MCP servers too.

### The cache directory

Each plugin gets its own scratch directory at `NINE_PLUGIN_CACHE_DIR` (partial
downloads, an extracted archive, a SQLite file, a cursor). Nine creates it and
never reads it. It is **ephemeral by default** — wiped when the plugin exits — so
do not put anything there you need to survive. Set `persist_cache = true` under
`[plugin.<name>]` to keep it across restarts (`NINE_PLUGIN_CACHE_PERSISTENT`
tells the plugin which it got). The root is `[plugins].cache_dir`
(default `~/.cache/nine/plugins`).

### Long-running work (jobs)

A tool that would hold the turn open for minutes — a download, a scan — can run
detached. Return a `plugin.Job` from a job handler: the call replies immediately
with a one-line ack and a job id, and the work runs on a context **detached from
the request** so writing the reply does not cancel it. The daemon polls the job
and surfaces its result on a later turn; the model steers it with
`job_wait`/`job_check`/`job_list`/`job_cancel`.

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

Inside `Run`, use `plugin.JobDir(ctx)` for per-job scratch and
`plugin.SetProgress(ctx, "41% · 1.2 GB/2.9 GB")` to report progress. `Serve`
honours the plugin's `max_concurrent` for jobs (excess jobs queue) and evicts
finished jobs after a TTL. Jobs are native-plugin only (protocol v2); MCP servers
cannot use them. See [Plugin capabilities § 5](plugin-capabilities.md).

## Limits

| Limit | Detail |
|-------|--------|
| Fixed at build time | Built-in plugins are compiled into the `nine` binary. Adding one means editing the source repo and rebuilding; there is no runtime path for an agent or an operator to add a native plugin. |
| User plugins need a restart or reload | A new plugin under `[plugins].user_dir` is discovered at boot. `nine plugins reload` picks up changes for user plugins; built-ins need a daemon restart. |
| Unbounded concurrency by default | `max_concurrent` defaults to 0, meaning unbounded. A plugin holding shared mutable state must declare its own cap. |
| No capability sandbox | A plugin is an ordinary subprocess running as the daemon's process user, with the daemon's filesystem and network reach. Only sandboxed tools run behind a capability boundary. |
| A reloaded plugin reaches only new loops | `nine plugins reload` starts the plugin, but a session already running keeps the tool set its loop was built with. Sandboxed tools differ — they re-sync at every turn boundary — so a reloaded plugin tool reaches an existing conversation only after it ends. |
| MCP tools are invisible to built-in roles | Their names carry an operator-chosen prefix, and role allowlists match exactly. |
| A plugin cannot call back | The daemon dials the plugin and never the reverse. A plugin reports long-running work by being polled. |
