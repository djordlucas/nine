# Browser Plugin

The browser plugin gives Nine a headless Chromium browser. It can navigate pages, extract text, take screenshots, interact with forms, and run arbitrary JavaScript — all driven by natural-language instructions.

The plugin is a self-contained binary compiled with Bun. It requires no Node.js or npm on the host; only Chromium is needed at runtime.

---

## Tools

| Tool | Description |
|------|-------------|
| `browser_navigate` | Go to a URL and wait for the page to load |
| `browser_screenshot` | Capture the current page as a PNG or JPEG image |
| `browser_extract` | Pull text or attribute values from the page |
| `browser_click` | Click an element |
| `browser_fill` | Fill a form field with a value |
| `browser_eval` | Execute JavaScript in the page context |
| `browser_wait` | Wait for a selector, text, or URL pattern to appear |
| `browser_status` | Report whether a page is open and its current URL/title |
| `browser_reset` | Close the current page and start a fresh session |

---

## Tool Reference

### `browser_navigate`

Navigate to a URL and wait for the page to load. Returns the final URL, page title, and HTTP status code.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `url` | string | yes | URL to navigate to |
| `wait_until` | string | no | When to consider load complete: `load` (default), `domcontentloaded`, `networkidle` |
| `timeout` | integer | no | Timeout in ms (default: `BROWSER_TIMEOUT`) |

**Example prompts:**
```
Open https://news.ycombinator.com and give me the top 5 headlines.
Navigate to https://example.com/login and tell me what form fields are on the page.
```

---

### `browser_screenshot`

Take a screenshot of the current page. Returns base64-encoded image data, type (`png` or `jpeg`), and viewport dimensions.

Large PNGs are automatically re-encoded as JPEG at 80% quality if they exceed `BROWSER_MAX_SCREENSHOT_BYTES` (default 3 MB).

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `full_page` | boolean | no | Capture the full scrollable page (default: false) |
| `selector` | string | no | Capture only the element matching this CSS selector |
| `quality` | integer | no | JPEG quality 1–100; forces JPEG output |

**Example prompts:**
```
Take a screenshot of the page and describe what you see.
Screenshot just the #main-content div.
```

---

### `browser_extract`

Extract text or attribute values from the current page. Without a selector, returns all visible body text.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `selector` | string | no | CSS selector to match elements. Omit for full-page text. |
| `attribute` | string | no | Extract this HTML attribute instead of text content (e.g. `href`, `src`) |
| `as_json` | boolean | no | Return results as a JSON array instead of newline-joined text |
| `max_chars` | integer | no | Truncate output to this many characters (default: 50 000) |

**Example prompts:**
```
Extract all the links on the current page.
Get the text of every element matching .product-title as a JSON array.
```

---

### `browser_click`

Click an element on the current page.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `selector` | string | yes | CSS selector or Playwright text selector (e.g. `text=Submit`) |
| `timeout` | integer | no | Timeout in ms |
| `wait_for_navigation` | boolean | no | Wait for a page navigation to complete after clicking (default: false) |

**Example prompts:**
```
Click the "Accept cookies" button.
Click the first search result link and wait for the next page to load.
```

---

### `browser_fill`

Fill a form input field.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `selector` | string | yes | CSS selector targeting the input |
| `value` | string | yes | Value to enter |
| `clear_first` | boolean | no | Clear the existing value before filling (default: true) |
| `timeout` | integer | no | Timeout in ms |

**Example prompts:**
```
Fill in the username field with "testuser" and the password field with the value from memory.
```

---

### `browser_eval`

Execute JavaScript in the browser page context and return the result as JSON. Useful for reading DOM state, calling page functions, or extracting data that has no clean CSS selector.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `script` | string | yes | JavaScript expression or function body to evaluate |
| `timeout` | integer | no | Timeout in ms |

**Example prompts:**
```
Run document.title in the browser and return it.
Get the value of window.__APP_STATE__ as JSON.
```

**Note:** `browser_eval` executes arbitrary JavaScript. The primary security boundary is URL filtering — if Nine cannot be directed to a malicious page, eval cannot exfiltrate data to it.

---

### `browser_wait`

Wait for a condition before continuing. Exactly one of `selector`, `text`, or `url_pattern` must be provided.

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `selector` | string | conditional | Wait for this CSS selector to be visible |
| `text` | string | conditional | Wait for this text to appear anywhere on the page |
| `url_pattern` | string | conditional | Wait for the URL to match this glob (e.g. `**/success**`) |
| `timeout` | integer | no | Timeout in ms |

**Example prompts:**
```
After clicking submit, wait for the text "Order confirmed" to appear.
Wait until the URL contains "/dashboard" before taking a screenshot.
```

---

### `browser_status`

Return the current browser state. No parameters required.

Returns: `{"open": true, "url": "...", "title": "...", "viewport": {"width": 1280, "height": 800}}`

When no browser session is active: `{"open": false}`

---

### `browser_reset`

Close the current page and open a fresh one, clearing all cookies, localStorage, and session state. The browser process itself keeps running.

No parameters required.

**Example prompts:**
```
Reset the browser and then log in as a different user.
```

---

## Session Model

The browser plugin maintains a single browser instance and a single page per process. Sessions are:

- **Started implicitly** on the first tool call — no explicit open step needed
- **Stateful** — cookies, localStorage, and history persist across tool calls within the same conversation
- **Resettable** via `browser_reset`
- **Torn down automatically** when Nine stops the plugin (stdin closes)

If Chromium crashes mid-session, the plugin detects it on the next call and relaunches the browser transparently.

---

## Configuration

All configuration is via environment variables. Defaults work for most use cases.

| Variable | Default | Description |
|----------|---------|-------------|
| `BROWSER_HEADLESS` | `1` | Set to `0` to show the browser window (local development only) |
| `BROWSER_TIMEOUT` | `30000` | Default timeout in ms for navigation, clicks, and waits |
| `BROWSER_VIEWPORT_WIDTH` | `1280` | Viewport width in pixels |
| `BROWSER_VIEWPORT_HEIGHT` | `800` | Viewport height in pixels |
| `BROWSER_ALLOW_URLS` | _(empty)_ | Comma-separated glob patterns. If set, only matching URLs are permitted. |
| `BROWSER_BLOCK_URLS` | _(empty)_ | Comma-separated glob patterns. Matching URLs are rejected. |
| `BROWSER_ALLOW_PRIVATE` | _(empty)_ | Set to `1` to allow private/loopback addresses (disabled by default) |
| `BROWSER_MAX_SCREENSHOT_BYTES` | `3000000` | Size cap before PNG screenshots fall back to JPEG |
| `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` | _(Playwright default)_ | Path to the Chromium binary. Set automatically in Docker. |

To override defaults, pass env vars when starting the daemon, or modify the `browserPlug` entry in `cmd/nine/main.go`.

---

## Security

### Private address blocking

By default, the plugin blocks navigation to private and loopback addresses (e.g. `127.0.0.1`, `192.168.x.x`, `10.x.x.x`, `::1`). This prevents server-side request forgery (SSRF) in environments where the agent has network access to internal services.

To allow private addresses (e.g. for testing against a local dev server):

```bash
BROWSER_ALLOW_PRIVATE=1
```

### URL allowlists and blocklists

Restrict which URLs the browser can visit:

```bash
# Only allow a specific domain
BROWSER_ALLOW_URLS=https://example.com/**

# Block a specific path pattern
BROWSER_BLOCK_URLS=**/admin/**,**/internal/**
```

Glob patterns use [minimatch](https://github.com/isaacs/minimatch) syntax.

### `--no-sandbox` in Docker

Chromium's process sandbox requires Linux namespace privileges unavailable in most Docker configurations. The plugin launches Chromium with `--no-sandbox`. The Docker container boundary provides the isolation layer.

---

## Docker

The Docker image uses Alpine's system `chromium` package rather than Playwright's bundled Chromium. Playwright's bundled binary is compiled for glibc and does not run on Alpine's musl libc.

`npm install --production` runs in a dedicated `node:alpine` build stage. The resulting `node_modules` are copied into the runtime image alongside the plugin source. The Alpine `nodejs` package provides the `node` binary.

The `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH` environment variable is set automatically in the Dockerfile to point at `/usr/bin/chromium-browser`.

No changes to `nine.toml` are needed — the plugin launcher, JavaScript, and `node_modules` are all baked into the image under `/opt/nine/browser` and run from there.

---

## Building Locally

Node.js (v18+) is required. The plugin runs as a `node` script; `npm install` fetches the dependencies.

```bash
# Build just the browser plugin
make browser-plugin

# Or build everything
make all
```

`make browser-plugin` runs `npm install` in `plugins/browser/` and writes a shell launcher to `dist/bin/browser`. The launcher embeds the absolute path to `plugins/browser/index.js` so `node_modules` is always found. Nine picks it up automatically on next daemon start.

To test the plugin directly without running a full daemon:

```bash
cd plugins/browser
npm install
echo '{"jsonrpc":"2.0","id":1,"method":"plugin.describe","params":{}}' | node index.js
```
