# Plugins

Plugins are the mechanism through which Nine gains most of its capabilities — running shell commands, reading/writing files, searching the web. Nine ships with five default plugins, fixed at build time. Plugins are not generated or loaded at runtime; to add one, add it to the source repo and rebuild the image.

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

`web_search` is a fallback: when the browser plugin is available, `browser_navigate` +
`browser_extract` are preferred.

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

### `browser` — Headless Browser

A Playwright/Chromium plugin for web automation and extraction. See [Browser Plugin](browser.md) for the full reference.

| Tool | Description |
|------|-------------|
| `browser_navigate` | Navigate to a URL and wait for load |
| `browser_screenshot` | Capture the current page as PNG or JPEG |
| `browser_extract` | Extract text or attribute values from the page |
| `browser_click` | Click an element |
| `browser_fill` | Fill a form field |
| `browser_eval` | Execute JavaScript in the page context |
| `browser_wait` | Wait for a selector, text, or URL pattern |
| `browser_status` | Report current URL, title, and viewport |
| `browser_reset` | Clear cookies and session state |

**Example prompts:**
```
Navigate to https://news.ycombinator.com and summarize the top 5 stories.
Take a screenshot of https://example.com and describe the layout.
Fill in the login form at https://myapp.internal/login with the credentials from memory.
```

**Notes:**
- The plugin starts automatically if `dist/bin/browser` exists. Build it with `make browser-plugin` (requires Node.js and npm).
- Private and loopback addresses are blocked by default. Set `BROWSER_ALLOW_PRIVATE=1` to allow them.
- In Docker, the Alpine system Chromium is used (`/usr/bin/chromium-browser`).

---

## Adding a Plugin

Plugins are part of the source repo and are compiled into the image at build time.
Nine cannot generate, build, or load a plugin at runtime — adding a capability means
adding a plugin to the tree and rebuilding. A plugin is a standalone Go binary; here
is a minimal example:

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
build time            go build → /opt/nine/bin/<name>   (baked into the image)
     │
daemon start          TryStart → spawn process → plugin.describe → register tools
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

The plugin manager passes this to each subprocess:

| Variable | Value | Purpose |
|----------|-------|---------|
| `NINE_BIN` | `plugins.bin` from config | Plugin binary directory |

Individual plugins may receive extra env vars at startup (see `Config.PluginEnvs`),
e.g. `NINE_SKILLS_DIR` for `skills` or the `BROWSER_*` settings for `browser`.
Plugins can also read their own env for secrets (e.g., `MY_PLUGIN_API_KEY`).
