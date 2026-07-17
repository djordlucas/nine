---
name: web-research
description: How to research topics on the web using web_search and web_page_read
tags: [web, search, research, http]
---

## Web Research

Use `web_search` and `web_page_read` together to research topics efficiently.

### Basic pattern

1. Search for the topic with `web_search`
2. Pick the most relevant result URLs
3. Fetch full content with `web_page_read` for detail

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

### Combining results

For comprehensive research:
1. Run 2-3 searches with different phrasings
2. Read the top 2-3 pages from each
3. Synthesise across sources rather than trusting a single one

### Making HTTP requests directly

For APIs that return JSON, use `http_get` directly:
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
