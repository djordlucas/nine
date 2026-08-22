# Browser Automation

Nine drives a browser through **MCP**. There is no browser plugin: you declare
[Playwright's MCP server][pw-mcp] in `nine.toml` as an `[[mcp.server]]`, and its
tools arrive on the agent loop like any other plugin's.

This document is the worked example for [MCP servers](plugins.md#mcp-servers)
generally — a browser is just the most useful one to set up first.

[pw-mcp]: https://github.com/microsoft/playwright-mcp

> **This replaces a built-in.** Nine used to ship its own Playwright wrapper as a
> Node plugin with nine `browser_*` tools. It is gone. If you are upgrading, see
> [Migrating from the browser plugin](#migrating-from-the-browser-plugin) — the
> tool names changed, and so did the security posture.

---

## 1. Install a browser

The MCP server is fetched by `npx` on demand; the browser it drives is not.

```sh
npx @playwright/mcp@latest install-browser chrome-for-testing
```

This is **not** `npx playwright install chromium`. Under `--browser chromium`
the server resolves a `chrome-for-testing` build it manages itself, and a
Playwright-managed chromium does not satisfy it — you get a bare
`Browser "chrome-for-testing" is not installed` on the first navigation, long
after the server has otherwise loaded fine.

Requires Node.js (v18+) on the host running the daemon.

---

## 2. Declare the server

```toml
[[mcp.server]]
name    = "playwright"
command = "npx"
args    = [
  "-y", "@playwright/mcp@latest",
  "--headless",
  "--browser", "chromium",
  "--isolated",
]
```

Restart the daemon and check the roster:

```sh
nine plugins
```

`mcp:playwright` should be listed with 24 tools. If it is missing, the reason is
recorded there — the daemon logs one failure and boots anyway.

### Useful flags

| Flag | Why |
|------|-----|
| `--headless` | No visible window. Omit it to watch the agent work — genuinely useful when debugging a stuck run. |
| `--browser chromium` | Use the server's portable build. The default is the `chrome` channel, which needs a real Google Chrome install and fails without one. |
| `--isolated` | Keep the profile in memory, so nothing persists between daemon restarts. |
| `--no-sandbox` | Required where Chromium cannot get namespace privileges — most containers. |
| `--viewport-size 1280x800` | Page dimensions. |
| `--timeout-navigation 60000` | Navigation timeout in ms. |
| `--output-dir <path>` | Where snapshots and downloads are written (see [§4](#4-where-output-goes)). |
| `--caps vision,pdf` | Enable extra tool groups beyond the default set. |

`npx @playwright/mcp@latest --help` is the authoritative list.

### Pinning

`@latest` is convenient and unstable. The tool surface, the reply format, and
the browser-install command have all changed across releases. For anything you
depend on, pin:

```toml
args = ["-y", "@playwright/mcp@0.0.79", "--headless", "--browser", "chromium", "--isolated"]
```

`0.0.79` is the version Nine's own end-to-end test is written against.

---

## 3. The tools

Tools arrive **prefixed with the server name** you chose, so
`browser_navigate` reaches the model as `playwright__browser_navigate`. That
prefix is what stops two MCP servers exposing the same tool name from colliding
(see [R-PLUG.9](../spec/contracts/plugin.md)).

At `0.0.79` the default set is 24 tools. The ones that carry most of the work:

| Tool | Purpose |
|------|---------|
| `browser_navigate` | Go to a URL. Returns the final URL and page title. |
| `browser_snapshot` | Accessibility-tree snapshot of the page — the idiomatic way to "read" a page. |
| `browser_evaluate` | Run JavaScript in the page and return the result **inline**. |
| `browser_click`, `browser_type`, `browser_fill_form`, `browser_select_option`, `browser_hover`, `browser_press_key` | Interaction. |
| `browser_take_screenshot` | PNG/JPEG of the page. |
| `browser_wait_for` | Wait for text, or a fixed duration. |
| `browser_find` | Locate elements without hand-writing a selector. |
| `browser_tabs` | List, open, close, and switch tabs. |
| `browser_console_messages`, `browser_network_requests` | Page diagnostics. |
| `browser_navigate_back`, `browser_resize`, `browser_handle_dialog`, `browser_file_upload`, `browser_drag`, `browser_drop` | The rest. |

### 24 tools is not free

Every tool's name, description, and JSON schema goes into the context window on
every turn. Against a small local model at `num_ctx = 32768` — Nine's default is
`qwen3.5:4b` — that is a real fraction of the budget, and it competes with the
conversation.

Nine ranks tools by relevance when an embedder is configured
(`[embeddings]`, see [tool-exposition.md](tool-selection.md)), which limits the
damage. If you are running a small model and browsing rarely, consider declaring
the server only in the config of the instance that needs it.

---

## 4. Where output goes

Two different mechanisms, worth keeping straight:

**Snapshots** behave differently depending on how you got one. The snapshot
*appended to an action's reply* (`browser_navigate`, `browser_click`) is written
into `--output-dir` and returned as a **link** — so navigate gives you the final
URL and page title, but not the content. A **direct `browser_snapshot` call
returns the accessibility tree inline**, with a `[ref=eN]` handle on every
element:

```yaml
- heading "Example Domain" [level=1] [ref=e3]
- link "Learn more" [ref=e6]:
  - /url: https://iana.org/domains/example
```

Those refs are what `browser_click`/`browser_type` take as `target` (a CSS
selector works too). `browser_evaluate` is the alternative when you want raw
text or a value the tree does not carry.

**Images** — screenshots — come back as MCP image content. Nine decodes them and
writes them into the plugin's cache dir, then names the file in the text reply
rather than inlining hundreds of kilobytes of base64 into the context window.
The agent reads the bytes back with `read_file` when it needs them. See
[plugin-capabilities.md §4](plugin-capabilities.md) for the cache dir.

---

## 5. Security

**Read this before pointing an agent at the open web.**

Nine enforces no URL policy on a browser MCP server. The server is an
`[[mcp.server]]` you chose to install, and Nine treats it like every other one:
it does not inspect, filter, or rewrite its arguments.

Upstream's filters are not a substitute. `@playwright/mcp` offers
`--allowed-origins` and `--blocked-origins`, and its own `--help` says of both:

> Important: **\*does not\* serve as a security boundary** and **\*does not\*
> affect redirects**.

There is no private-address or loopback blocking at all. An agent that follows a
link to `http://169.254.169.254/` — the cloud instance-metadata endpoint on AWS,
GCP, and Azure, which serves credentials to anything that asks — will reach it.

This is a **deliberate reduction** from the old browser plugin, which blocked
private and loopback ranges and enforced glob allow/block lists in-process. That
guard was Nine-specific code protecting a boundary Nine no longer owns.

If your agent browses untrusted pages, put the control somewhere that actually
holds:

- **Network egress rules** on the container or host — the only layer a redirect
  cannot talk its way around.
- **`--isolated`**, so a poisoned session does not persist into the next one.
- **Run the browser somewhere without credentials to steal** — no metadata
  endpoint, no ambient cloud role, no reachable internal services.
- **`--allowed-origins`** as defense in depth, understanding it is advisory.

A browsing agent is the one MCP server where the target of a call is chosen by
attacker-influenced content — the page tells the agent where to go next. Treat
it accordingly.

---

## 6. Docker

The runtime image ships **no Node and no browser**. Nine's image carries what
Nine needs, and an MCP server is by definition something Nine does not build.

Three ways to run a browser against a containerized daemon:

**a. Derive an image.** Install what the server needs on top of Nine's:

```dockerfile
FROM nine:latest
USER root
RUN apt-get update && apt-get install -y --no-install-recommends nodejs npm \
    && rm -rf /var/lib/apt/lists/*
ENV PLAYWRIGHT_BROWSERS_PATH=/data/.playwright
RUN npx -y @playwright/mcp@0.0.79 install-browser chrome-for-testing
```

Add `--no-sandbox` to the server's `args`. Pointing
`PLAYWRIGHT_BROWSERS_PATH` at `/data` keeps the browser on the volume instead of
in the image layer.

**b. Drive a browser outside the container.** Run Chromium anywhere with a
remote-debugging port open and connect to it:

```toml
args = ["-y", "@playwright/mcp@0.0.79", "--cdp-endpoint", "http://browser-host:9222"]
```

Best of the three for isolation: the browser's blast radius is its own host, not
the daemon's.

**c. A hosted MCP server.** Declare it by `url` instead of `command` — Nine
speaks MCP streamable HTTP:

```toml
[[mcp.server]]
name = "playwright"
url  = "https://mcp.example.com/rpc"
[mcp.server.headers]
Authorization = "Bearer ..."
```

The **dev** image (`make up-hot`) does carry `nodejs` and `npm`, since that is
where servers get tried out before being committed to. It still has no browser;
install one into `/data` as above.

---

## 7. Testing

Nine's own end-to-end test drives the real upstream server through the bridge:

```sh
NINE_PLAYWRIGHT_TEST=1 go test ./internal/builtins/ -run Playwright
```

It is opt-in because it needs `npx`, network for the first fetch, and an
installed browser — `make test` stays free of all three. It covers tool
prefixing, a navigate + evaluate round trip against a local `httptest` server,
and a screenshot reaching the cache dir.

The rest of the MCP suite runs against a compiled fixture in
`internal/builtins/testmcpserver/`, which is fast and hermetic but agrees with
Nine by construction. This test is the one that catches a handshake quirk or a
reply shape upstream changed.

---

## 8. Migrating from the browser plugin

The old plugin's tools do not map one-to-one.

| Old (`browser` plugin) | New (`@playwright/mcp`) |
|------------------------|-------------------------|
| `browser_navigate` | `playwright__browser_navigate` |
| `browser_extract` | `playwright__browser_evaluate`, or `browser_snapshot` |
| `browser_screenshot` | `playwright__browser_take_screenshot` |
| `browser_click` | `playwright__browser_click` |
| `browser_fill` | `playwright__browser_type` / `browser_fill_form` |
| `browser_eval` | `playwright__browser_evaluate` |
| `browser_wait` | `playwright__browser_wait_for` |
| `browser_status` | — (navigate reports URL and title) |
| `browser_reset` | `playwright__browser_close`, or `--isolated` |

Other changes:

- **`BROWSER_*` environment variables are gone.** They were the old plugin's
  config surface. Use the server's CLI flags in `args` instead:
  `BROWSER_HEADLESS=0` → omit `--headless`; `BROWSER_VIEWPORT_WIDTH/HEIGHT` →
  `--viewport-size 1280x800`; `BROWSER_TIMEOUT` → `--timeout-navigation`.
- **`BROWSER_ALLOW_URLS` / `BROWSER_BLOCK_URLS` / `BROWSER_ALLOW_PRIVATE` have
  no equivalent.** See [§5](#5-security).
- **`make browser-plugin` is gone**, along with `dist/bin/browser`. `make dev`
  no longer installs npm packages or a browser.
- **Skills and prompts that name `browser_*` tools need updating** for the
  prefix and the renames.

---

## 9. Telling the agent how to use it

Nine's system prompt says nothing about browsers — it cannot, since it does not
know whether one is configured or what the operator named it. The guidance lives
in the built-in **`web-research`** skill (`skills/web-research.md`), which is
retrieved on relevance rather than costing every turn.

That skill teaches the agent to branch on its own tool list: use the browser when
a `*__browser_navigate` tool is present, fall back to `web_search` /
`web_page_read` when it is not, and — importantly — report the limit rather than
passing off a consent wall as the page's content.

Note that a **role with a tool allowlist cannot receive MCP tools**: the list is
matched exactly and the prefix is yours to choose, so built-in roles like
`report-writer` stay on the HTTP path even here. See
[roles.md](roles.md) for the two ways around that.

---

## See also

- [plugins.md](plugins.md) — the plugin model, and MCP servers in general
- [configuration.md](configuration.md) — `[[mcp.server]]` reference
- [tool-exposition.md](tool-selection.md) — how tools are ranked into context
- [spec/contracts/plugin.md](../spec/contracts/plugin.md) — R-PLUG.15, the MCP bridge
