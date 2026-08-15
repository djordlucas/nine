---
name: web-research
description: Research the web with a browser when one is available, falling back to web_search and web_page_read when it is not
tags: [web, search, research, http, browser, mcp]
---

## Web Research

There are two ways to read the web, and which one you have depends on how this
Nine was configured. **Find out first** — it decides the whole approach:

```
tool_search({"query": "browser navigate page"})
```

| If that returns… | Use | Section |
|---|---|---|
| a tool ending in `__browser_navigate` | the browser | [A](#a-with-a-browser) |
| only `web_search` / `web_page_read` | plain HTTP | [B](#b-without-a-browser) |

A browser reaches Nine as an MCP server, so its tools are **prefixed with the
name the operator chose** — `playwright__browser_navigate` under the standard
recipe, but it could be any prefix. Match on the `__browser_navigate` suffix,
not on a fixed name. Examples below write `<b>__` for that prefix.

**Do not assume a browser exists.** Nine ships none. If you call a browser tool
that is not in your list, the call fails and you have wasted a turn — check,
then pick a path.

---

## A. With a browser

Use it when the page needs JavaScript, a login, interaction, or when plain HTTP
already returned something useless.

### Navigate does not give you the page text

This is the mistake to avoid. `<b>__browser_navigate` returns the final URL and
the page title — **not the content**. The snapshot appended to its reply is a
*link* to a file, not the page. Reading is a second, explicit call:

```
<b>__browser_navigate({"url": "https://example.com/pricing"})
# → final URL, page title, and a link to a snapshot file

<b>__browser_snapshot({})
# → the accessibility tree, INLINE, with a [ref=eN] handle on every element
```

`browser_snapshot` is the one to reach for. It returns both the page's text and
the refs you need to act on it:

```yaml
- heading "Example Domain" [level=1] [ref=e3]
- paragraph [ref=e4]: This domain is for use in documentation examples.
- link "Learn more" [ref=e6]:
  - /url: https://iana.org/domains/example
```

Use `<b>__browser_evaluate` when you want raw text or a value the tree does not
carry:

```
<b>__browser_evaluate({"function": "() => document.body.innerText"})
```

### Interacting

Both `browser_click` and `browser_type` take a **`target`** plus a
human-readable **`element`** description. `target` is either a `ref` id from the
latest snapshot or a CSS selector — both work:

```
<b>__browser_snapshot({})                       # get refs first
<b>__browser_click({"element": "Learn more link", "target": "e6"})
<b>__browser_type({"element": "Search box", "target": "#q", "text": "query", "submit": true})
```

Snapshot **before** acting: refs come from it and go stale once the page
changes. Re-snapshot after anything that navigates or re-renders.

Waiting is a condition, not a sleep:

```
<b>__browser_wait_for({"text": "Results"})      # appears
<b>__browser_wait_for({"textGone": "Loading"})  # disappears
<b>__browser_wait_for({"time": 2})              # seconds — last resort
```

### Screenshots

`<b>__browser_take_screenshot` returns a summary plus a saved file path; the
image bytes are not pasted into your context. Read the file when you need to
look at it:

```
<b>__browser_take_screenshot({"scale": "css"})
# → "...saved to /path/to/page-<timestamp>.png"
read_file({"path": "/path/to/page-<timestamp>.png"})
```

Prefer `browser_snapshot` for anything you intend to *act* on — a screenshot
carries no refs, so you cannot click from it.

### Searching with a browser

Navigate to a search engine and read the results:

```
<b>__browser_navigate({"url": "https://duckduckgo.com/?q=go+1.26+release+notes"})
<b>__browser_snapshot({})     # result links come back with their /url
```

`web_search` is often the cheaper way to get the same result list — it costs one
call instead of two and returns clean `{title, url, snippet}` objects. Use the
browser for the *pages*, `web_search` for the *finding*, even when you have both.

### When a browser tool fails

Do not retry the same call. Fall back:

1. Browser tool errors, or the server is not loaded → use `web_page_read` on the
   same URL. Plain HTTP still gets static pages.
2. `web_page_read` returns boilerplate, a consent wall, or near-empty text → the
   page is JavaScript-rendered. If you have no browser, **say so** rather than
   reporting the empty result as the answer.

---

## B. Without a browser

`web_search` and `web_page_read` are always present. They are plain HTTP: they
fetch the raw response and never run JavaScript.

```
web_search({"query": "golang context cancellation patterns", "limit": 5})
# → [{title, url, snippet}, ...]

web_page_read({"url": "https://..."})
# → visible text of the page
```

### Search query tips

- Be specific: `"go 1.26 release notes"` beats `"go release"`
- Include version numbers when looking for docs
- Add `site:pkg.go.dev` for Go package docs: `"site:pkg.go.dev/context"`
- Add `filetype:pdf` for whitepapers when needed

### Reading pages efficiently

- `web_page_read` strips scripts, styles, nav, and footer — you get the main content
- Use `max_bytes` to limit large pages: `{"url": "...", "max_bytes": 102400}`
- If a page is too long, search for a more specific subpage

### What this path cannot do

Know the limits, and report them instead of guessing:

- **JavaScript-rendered pages** come back as an empty shell or a "enable
  JavaScript" notice. That is not the content.
- **Anything behind a login** is unreachable.
- **Interaction** — clicking, filling a form, paging through results — is not
  possible.

When you hit one of these, say which page failed and why. A wrong answer
assembled from a consent banner is worse than reporting the limit.

---

## Both paths

### Combining results

For comprehensive research:

1. Run 2-3 searches with different phrasings
2. Read the top 2-3 pages from each
3. Synthesise across sources rather than trusting a single one

Cite the URL you actually read, not the search result you meant to read.

### Making HTTP requests directly

For APIs that return JSON, skip both paths and use `http_get`:

```
http_get({"url": "https://api.example.com/data"})
```

For POST requests:

```
http_post({
    "url": "https://api.example.com/submit",
    "body": "{\"key\": \"value\"}",
    "content_type": "application/json"
})
```

An API is almost always better than scraping the page that renders it — fewer
calls, stable shape, no rendering to wait on. Look for one first.
