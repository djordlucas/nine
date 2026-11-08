package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// web_page_read and web_search in Go, ported from the sandbox's nine:html.

// shippedWebPageRead implements web_page_read: fetch a page, return readable
// text with markup, scripts and styles stripped.
func shippedWebPageRead(ctx context.Context, args json.RawMessage) (string, error) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("web_page_read: %w", err)
	}
	client := shippedHTTPClient([]string{"*"}, []string{"GET"}, 60*time.Second)
	headers := map[string]string{"User-Agent": "Mozilla/5.0 (compatible; nine-agent/1.0)"}
	status, body, err := fetchURL(ctx, client, []string{"*"}, []string{"GET"}, "GET", req.URL, headers, nil)
	if err != nil {
		return "", err
	}
	out, _ := json.Marshal(map[string]any{"status": status, "text": htmlToText(body)})
	return string(out), nil
}

// htmlToText is the Go port of nine:html's textOf: a tokenizer that drops script
// and style contents wholesale (a "<" inside them is not markup) and unescapes
// entities in the remaining text.
func htmlToText(html string) string {
	var b strings.Builder
	i := 0
	n := len(html)
	skip := "" // inside <script> or <style>
	for i < n {
		c := html[i]
		if c == '<' {
			// comment
			if strings.HasPrefix(html[i:], "<!--") {
				end := strings.Index(html[i+4:], "-->")
				if end < 0 {
					break
				}
				i += 4 + end + 3
				continue
			}
			end := strings.IndexByte(html[i:], '>')
			if end < 0 {
				break
			}
			tag := html[i+1 : i+end]
			name := tagName(tag)
			if skip != "" {
				if isCloseTag(tag) && name == skip {
					skip = ""
				}
				i += end + 1
				continue
			}
			if name == "script" || name == "style" {
				if !strings.HasSuffix(strings.TrimSpace(tag), "/") {
					skip = name
				}
				i += end + 1
				continue
			}
			i += end + 1
			if name == "br" || name == "p" || name == "div" || name == "li" || name == "tr" || name == "h1" || name == "h2" || name == "h3" || name == "h4" || name == "h5" || name == "h6" {
				b.WriteString("\n")
			}
			continue
		}
		if c == '&' {
			if ent := decodeEntity(html[i:]); ent != "" {
				b.WriteString(ent)
				i += entityLen(html[i:])
				continue
			}
		}
		b.WriteByte(c)
		i++
	}
	// collapse whitespace runs and blank lines
	text := b.String()
	lines := strings.Split(text, "\n")
	var out []string
	for _, l := range lines {
		l = strings.Join(strings.Fields(l), " ")
		if l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// tagName extracts the lowercased tag name from tag content.
func tagName(tag string) string {
	t := strings.TrimSpace(tag)
	t = strings.TrimPrefix(t, "/")
	if i := strings.IndexAny(t, " \t\n\r/>"); i >= 0 {
		t = t[:i]
	}
	return strings.ToLower(t)
}

func isCloseTag(tag string) bool { return strings.HasPrefix(strings.TrimSpace(tag), "/") }

// decodeEntity decodes one HTML entity at the start of s, or "".
func decodeEntity(s string) string {
	if !strings.HasPrefix(s, "&") {
		return ""
	}
	if strings.HasPrefix(s, "&#x") || strings.HasPrefix(s, "&#X") {
		end := strings.IndexByte(s, ';')
		if end < 0 {
			return ""
		}
		var v int
		if _, err := fmt.Sscanf(s[3:end], "%x", &v); err == nil && v > 0 && v < 0x110000 {
			return string(rune(v))
		}
		return ""
	}
	if strings.HasPrefix(s, "&#") {
		end := strings.IndexByte(s, ';')
		if end < 0 {
			return ""
		}
		var v int
		if _, err := fmt.Sscanf(s[2:end], "%d", &v); err == nil && v > 0 && v < 0x110000 {
			return string(rune(v))
		}
		return ""
	}
	named := map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&apos;": "'",
		"&nbsp;": " ", "&mdash;": "—", "&ndash;": "–", "&hellip;": "…",
	}
	for k, v := range named {
		if strings.HasPrefix(s, k) {
			return v
		}
	}
	return ""
}

// entityLen is how many bytes the entity at s occupies.
func entityLen(s string) int {
	if strings.HasPrefix(s, "&") {
		if i := strings.IndexByte(s, ';'); i > 0 {
			return i + 1
		}
	}
	return 1
}

// findByClass is the Go port of nine:html's findByClass: elements carrying the
// given class, as {text, attrs}. Used by web_search to scrape DuckDuckGo's HTML
// results (result__a titles, result__snippet snippets).
func findByClass(html, className string) []htmlElement {
	var out []htmlElement
	i := 0
	for i < len(html) {
		if html[i] != '<' || strings.HasPrefix(html[i:], "<!--") {
			i++
			continue
		}
		end := strings.IndexByte(html[i:], '>')
		if end < 0 {
			break
		}
		tag := html[i+1 : i+end]
		name := tagName(tag)
		if name == "script" || name == "style" {
			// skip contents
			close := strings.Index(strings.ToLower(html[i+end+1:]), "</"+name)
			if close < 0 {
				break
			}
			gt := strings.IndexByte(html[i+end+1+close:], '>')
			if gt < 0 {
				break
			}
			i = i + end + 1 + close + gt + 1
			continue
		}
		attrs := parseAttrs(tag)
		if hasClass(attrs["class"], className) {
			// find the close tag for this element (naive: first </name>)
			close := strings.Index(strings.ToLower(html[i+end+1:]), "</"+name+">")
			var inner string
			if close >= 0 {
				inner = html[i+end+1 : i+end+1+close]
			}
			out = append(out, htmlElement{text: htmlToText(inner), attrs: attrs})
			if close < 0 {
				break
			}
			i = i + end + 1 + close + len(name) + 3
			continue
		}
		i += end + 1
	}
	return out
}

type htmlElement struct {
	text  string
	attrs map[string]string
}

// parseAttrs parses the attribute portion of a tag.
func parseAttrs(tag string) map[string]string {
	out := map[string]string{}
	t := strings.TrimSpace(tag)
	if i := strings.IndexAny(t, " \t\n\r"); i >= 0 {
		t = t[i+1:]
	}
	for len(t) > 0 {
		t = strings.TrimLeft(t, " \t\n\r/")
		if t == "" {
			break
		}
		eq := strings.IndexByte(t, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(t[:eq]))
		rest := strings.TrimSpace(t[eq+1:])
		var val string
		if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
			q := rest[0]
			if end := strings.IndexByte(rest[1:], q); end >= 0 {
				val = rest[1 : 1+end]
				t = rest[1+end+1:]
			} else {
				break
			}
		} else {
			if sp := strings.IndexAny(rest, " \t\n\r"); sp >= 0 {
				val = rest[:sp]
				t = rest[sp:]
			} else {
				val = rest
				t = ""
			}
		}
		out[key] = val
	}
	return out
}

func hasClass(classAttr, want string) bool {
	for _, c := range strings.Fields(classAttr) {
		if c == want {
			return true
		}
	}
	return false
}

// searchAllowHosts is web_search's real allowlist: three known search endpoints.
// The tool that can be constrained is constrained.
var searchAllowHosts = []string{"html.duckduckgo.com", "api.search.brave.com", "serpapi.com"}

// shippedWebSearch implements web_search: DuckDuckGo HTML by default, Brave or
// SerpAPI when SEARCH_PROVIDER/SEARCH_API_KEY are set in the daemon's env.
func shippedWebSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("web_search: %w", err)
	}
	n := req.Limit
	if n <= 0 || n > 25 {
		n = 10
	}
	provider := os.Getenv("SEARCH_PROVIDER")
	apiKey := os.Getenv("SEARCH_API_KEY")

	client := shippedHTTPClient(searchAllowHosts, []string{"GET"}, 60*time.Second)

	var results []map[string]string
	switch provider {
	case "brave":
		results = searchBrave(ctx, client, req.Query, n, apiKey)
	case "serpapi":
		results = searchSerpAPI(ctx, client, req.Query, n, apiKey)
	default:
		results = searchDDG(ctx, client, req.Query, n)
	}
	out, _ := json.Marshal(results)
	return string(out), nil
}

func searchDDG(ctx context.Context, client *http.Client, query string, limit int) []map[string]string {
	u := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	headers := map[string]string{
		"User-Agent":      "Mozilla/5.0 (compatible; nine-agent/1.0)",
		"Accept-Language": "en-US,en;q=0.9",
	}
	_, body, err := fetchURL(ctx, client, searchAllowHosts, []string{"GET"}, "GET", u, headers, nil)
	if err != nil {
		return []map[string]string{{"error": err.Error()}}
	}
	titles := findByClass(body, "result__a")
	snippets := findByClass(body, "result__snippet")
	var out []map[string]string
	for i := 0; i < len(titles) && i < len(snippets) && len(out) < limit; i++ {
		out = append(out, map[string]string{
			"title":   titles[i].text,
			"url":     decodeDDGURL(titles[i].attrs["href"]),
			"snippet": snippets[i].text,
		})
	}
	return out
}

// decodeDDGURL unwraps DuckDuckGo's redirector (the real URL is the uddg param).
func decodeDDGURL(href string) string {
	if !strings.Contains(href, "uddg=") {
		return href
	}
	abs := href
	if strings.HasPrefix(abs, "//") {
		abs = "https:" + abs
	}
	u, err := url.Parse(abs)
	if err != nil {
		return href
	}
	if u.Host == "" {
		u, err = url.Parse(href)
		if err != nil {
			return href
		}
		base, _ := url.Parse("https://duckduckgo.com")
		u = base.ResolveReference(u)
	}
	if v := u.Query().Get("uddg"); v != "" {
		return v
	}
	return href
}

func searchBrave(ctx context.Context, client *http.Client, query string, limit int, apiKey string) []map[string]string {
	u := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d", url.QueryEscape(query), limit)
	headers := map[string]string{"Accept": "application/json", "X-Subscription-Token": apiKey}
	_, body, err := fetchURL(ctx, client, searchAllowHosts, []string{"GET"}, "GET", u, headers, nil)
	if err != nil {
		return []map[string]string{{"error": err.Error()}}
	}
	var parsed struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return []map[string]string{{"error": err.Error()}}
	}
	var out []map[string]string
	for _, r := range parsed.Web.Results {
		if len(out) >= limit {
			break
		}
		out = append(out, map[string]string{"title": r.Title, "url": r.URL, "snippet": r.Description})
	}
	return out
}

func searchSerpAPI(ctx context.Context, client *http.Client, query string, limit int, apiKey string) []map[string]string {
	u := fmt.Sprintf("https://serpapi.com/search?q=%s&num=%d&api_key=%s", url.QueryEscape(query), limit, url.QueryEscape(apiKey))
	_, body, err := fetchURL(ctx, client, searchAllowHosts, []string{"GET"}, "GET", u, nil, nil)
	if err != nil {
		return []map[string]string{{"error": err.Error()}}
	}
	var parsed struct {
		OrganicResults []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic_results"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return []map[string]string{{"error": err.Error()}}
	}
	var out []map[string]string
	for _, r := range parsed.OrganicResults {
		if len(out) >= limit {
			break
		}
		out = append(out, map[string]string{"title": r.Title, "url": r.Link, "snippet": r.Snippet})
	}
	return out
}
