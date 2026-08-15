# Plugins

Plugins are the mechanism through which Nine gains most of its capabilities — running shell commands, reading/writing files, searching the web. Nine ships with four default plugins, fixed at build time. Built-in plugins are not generated or loaded at runtime; to add one, add it to the source repo and rebuild the image.

All four — `shell`, `files`, `http`, `time` — are Go, and are compiled **into the `nine` binary** (`internal/builtins`) rather than shipped as separate executables. The daemon starts each by re-executing itself as `nine plugin serve <name>`, so each still runs as its own isolated process; there is simply one artifact to build and ship.

Capabilities Nine does not implement itself arrive two other ways, both of which present as ordinary plugins: an **MCP server** declared in `[[mcp.server]]` (see [MCP servers](#mcp-servers)), and a **user plugin** dropped in `[plugins].user_dir`. Browser automation is the worked example of the first — see [Browser Automation](browser.md).

**Withholding a plugin.** `[plugins] disabled = ["shell"]` (or `NINE_PLUGINS_DISABLED=shell`) stops a plugin from ever starting — no process, no socket, no tools registered. It works by name and covers built-ins, MCP servers (as `mcp:<name>`), and your own plugins alike. This is how you run without `shell`, which executes arbitrary commands. A disabled plugin is reported by `nine plugins` as `off` rather than silently missing, so a tool that has gone absent is traceable to the decision that removed it. It is an operator setting read at boot; no agent or wire message can switch a plugin on or off.

Sharing the binary does not widen what a plugin process does. `plugin serve` is dispatched before nine's normal startup, so a plugin child loads **no config file** (the operator's `nine.toml` carries the embeddings API key and every other plugin's settings), writes **no** `nine.log`, and holds no way to start a daemon or TUI. Its output goes to stderr, which the daemon captures. Run by hand without `NINE_PLUGIN_SOCKET`, it refuses to start.

Several tools (memory, file storage, semantic search, and skills) are **core-intercepted**: built directly into the agent loop rather than served by a subprocess. See [Memory & File Tools](#memory--file-tools-core) and [Skill Tools](#skill-tools-core) below.

---

## Built-in Plugins

### `shell` — Run Shell Commands

| Tool | Description |
|------|-------------|
| `shell` | Execute a shell command with a configurable timeout. Returns stdout, stderr, and exit code. |

**Example prompt:**
```
Run "ls -la /tmp" and tell me the five largest files.
```

**Notes:**
- Commands run as the daemon's process user.
- Default timeout is 30 seconds.
- Nine does not sandbox commands — use Docker or a restricted user for untrusted workloads.

---

### `files` — Read and Write Files

| Tool | Description |
|------|-------------|
| `read_file` | Read the contents of a file at a given path. |
| `write_file` | Write content to a file, creating parent directories as needed. |

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

### Memory & File Tools (Core)

Unlike the plugins below, these tools are **core-intercepted**: they're wired
directly into the agent loop (`internal/agent/register_memory.go`) and call
`internal/memory.Store` in-process. There is no `memory` plugin subprocess — these
tools are simply always available.

| Tool | Description |
|------|-------------|
| `memory_get` | Retrieve a value from the key-value store by key. |
| `memory_set` | Store a value in the key-value store. |
| `memory_delete` | Delete a key from the key-value store. |
| `memory_list` | List keys in the key-value store, optionally filtered by prefix. |
| `file_store` | Store a file by path and content, indexed for full-text search. |
| `file_fetch` | Fetch a stored file by path. |
| `file_list` | List stored files, optionally filtered by path prefix. |
| `file_search_text` | Full-text search over stored file content. |
| `memory_embed` | Embed text and store the resulting vector under a namespace and key. |
| `memory_query` | Embed a query and return the most semantically similar stored items from a namespace. |
| `file_search_semantic` | Embed a query and search stored files by semantic similarity. |

**Example prompts:**
```
Remember that my AWS region is us-west-2.
What AWS region did I configure?
Store the output of this script as 'last_run_log'.
Find files related to database migrations.
```

---

### Skill Tools (Core)

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

### `http` — HTTP and Web

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

### `time` — Current Date and Time

| Tool | Description |
|------|-------------|
| `time` | Return the current date and time, including the local timezone. |

The system prompt instructs the agent to call this before any time-sensitive
research (news, current events, prices, status) so it never reasons from a stale or
assumed date.

**Example prompt:**
```
What's the current date and time?
```

---

### `mcp` — Model Context Protocol servers

An MCP server is a plugin. Each `[[mcp.server]]` you declare gets its own bridge
process, so it has the same shape and the same controls as anything else here.

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

---

## MCP servers

An [MCP](https://modelcontextprotocol.io) server is a capability Nine does not build.
Declare one in `nine.toml` and it becomes a plugin in every respect — its own process,
its own crash isolation, its own row in `nine plugins`, its own entry in
`[plugins].disabled`:

```toml
[[mcp.server]]
name    = "github"
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-github"]
[mcp.server.env]
GITHUB_PERSONAL_ACCESS_TOKEN = "ghp_..."
```

The daemon starts one `mcp` bridge process per server. Its tools arrive **prefixed with
the server name** — `github__create_issue` — so two servers exposing the same tool name
cannot collide and silently lose one. The plugin itself is named `mcp:github`.

A hosted server is reached by `url` instead of `command` (MCP streamable HTTP), with
`headers` for auth in place of `env`.

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

## Adding a Plugin

Plugins are part of the source repo and are compiled into the image at build time.
Nine cannot generate, build, or load a plugin at runtime — adding a capability means
adding a plugin to the tree and rebuilding.

A plugin you supply is a standalone binary (see [User Plugins](#user-plugins) below); Nine's own Go plugins skip the separate binary by registering in `internal/builtins`. Either way the code is the same shape — `plugin.Serve` with a tool list and a handler map. Here is a minimal standalone example:

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
in `cmd/nine/daemon.go` (`pluginManager.TryStart("<name>", …)`), and rebuild. Default
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
[tool-output-spill.md](tool-output-spill.md)), but any stored path works.

Only marked properties are expanded, so a tool whose arguments are genuinely paths is
unaffected. One expansion is capped at 8 MiB.

### Large output

You do not need to do anything about large *output*. A result over the dispatcher's cap
is spilled to the file store automatically and the model is handed a path — this applies
to every tool, including MCP servers, with no plugin involvement
([tool-output-spill.md](tool-output-spill.md)).

---

## User Plugins

Built-in plugins are baked into the image. **User plugins** are operator-supplied
executables Nine discovers at boot from `[plugins].user_dir` (env
`NINE_PLUGINS_USER_DIR`; mounted at `/plugins.d` under Docker). They are purely
additive — built-ins are never affected — and the directory is never required:
empty or absent means "no user plugins".

Discovery is boot-only, mirroring skills (`docs/skills.md`), with a live
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

## Plugin Lifecycle

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

## Environment Variables Passed to Plugins

The plugin manager passes these to each subprocess:

| Variable | Value | Purpose |
|----------|-------|---------|
| `NINE_BIN` | `plugins.bin` from config | Directory of plugins that ship their own binary |
| `NINE_PLUGIN_SOCKET` | per-plugin socket path | Where the plugin listens |
| `NINE_PLUGIN_CACHE_DIR` | per-plugin scratch dir | Cache directory (below) |
| `NINE_PLUGIN_CACHE_PERSISTENT` | `0`/`1` | Whether the cache dir persists |

Individual plugins also receive their built-in defaults (e.g. `NINE_WORKSPACE`
for `files`) plus any operator settings. An MCP bridge additionally receives
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

[plugin.files.settings]
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
