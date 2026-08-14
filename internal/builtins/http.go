package builtins

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/html"
	"nine/internal/plugin"
)

// serveHTTP runs the `http` built-in: http_get / http_post / web_search /
// web_page_read.
func serveHTTP() {
	plugin.Serve(
		[]plugin.ToolDefinition{
			{
				Name:        "http_get",
				DisplayName: "HTTP GET",
				Description: "Make an HTTP GET request and return the response body.",
				InputSchema: plugin.Schema(`{
					"type":"object","required":["url"],
					"properties":{
						"url":{"type":"string"},
						"headers":{"type":"object","additionalProperties":{"type":"string"}},
						"timeout":{"type":"integer","description":"Timeout in seconds (default 30)"}
					}
				}`),
			},
			{
				Name:        "http_post",
				DisplayName: "HTTP POST",
				Description: "Make an HTTP POST request and return the response body.",
				InputSchema: plugin.Schema(`{
					"type":"object","required":["url"],
					"properties":{
						"url":{"type":"string"},
						"body":{"type":"string"},
						"content_type":{"type":"string","description":"Content-Type header (default application/json)"},
						"headers":{"type":"object","additionalProperties":{"type":"string"}},
						"timeout":{"type":"integer"}
					}
				}`),
			},
			{
				Name:        "web_search",
				DisplayName: "Web Search",
				Description: "FALLBACK ONLY: use browser_navigate + browser_extract instead whenever the browser plugin is available. Only call this when the browser plugin has explicitly failed or is unavailable. Searches via DuckDuckGo by default; set SEARCH_PROVIDER=brave|serpapi and SEARCH_API_KEY for a different backend.",
				InputSchema: plugin.Schema(`{
					"type":"object","required":["query"],
					"properties":{
						"query":{"type":"string"},
						"limit":{"type":"integer","description":"Max results (default 10)"}
					}
				}`),
			},
			{
				Name:        "web_page_read",
				DisplayName: "Read Web Page",
				Description: "FALLBACK ONLY: use browser_navigate + browser_extract instead whenever the browser plugin is available. Only call this when the browser plugin has explicitly failed or is unavailable.",
				InputSchema: plugin.Schema(`{
					"type":"object","required":["url"],
					"properties":{
						"url":{"type":"string"},
						"max_bytes":{"type":"integer","description":"Max bytes of HTML to fetch (default 512KB)"}
					}
				}`),
			},
		},
		map[string]plugin.ToolHandler{
			"http_get":      httpGet,
			"http_post":     httpPost,
			"web_search":    webSearch,
			"web_page_read": webPageRead,
		},
	)
}

// --- http_get ---

func httpGet(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Timeout int               `json:"timeout"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", plugin.InvalidArgs("%v", err)
	}
	if p.Timeout <= 0 {
		p.Timeout = 30
	}
	client := &http.Client{Timeout: time.Duration(p.Timeout) * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", err
	}
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// --- http_post ---

func httpPost(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		URL         string            `json:"url"`
		Body        string            `json:"body"`
		ContentType string            `json:"content_type"`
		Headers     map[string]string `json:"headers"`
		Timeout     int               `json:"timeout"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", plugin.InvalidArgs("%v", err)
	}
	if p.ContentType == "" {
		p.ContentType = "application/json"
	}
	if p.Timeout <= 0 {
		p.Timeout = 30
	}
	client := &http.Client{Timeout: time.Duration(p.Timeout) * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.URL, strings.NewReader(p.Body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", p.ContentType)
	for k, v := range p.Headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// --- web_search ---

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

func webSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", plugin.InvalidArgs("%v", err)
	}
	if p.Limit <= 0 {
		p.Limit = 10
	}

	provider := os.Getenv("SEARCH_PROVIDER")
	apiKey := os.Getenv("SEARCH_API_KEY")

	var (
		results []searchResult
		err     error
	)
	switch provider {
	case "brave":
		results, err = braveSearch(ctx, p.Query, p.Limit, apiKey)
	case "serpapi":
		results, err = serpapiSearch(ctx, p.Query, p.Limit, apiKey)
	default:
		results, err = duckDuckGoSearch(ctx, p.Query, p.Limit)
	}
	if err != nil {
		return "", err
	}

	b, _ := json.Marshal(results)
	return string(b), nil
}

func duckDuckGoSearch(ctx context.Context, query string, limit int) ([]searchResult, error) {
	reqURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; nine-agent/1.0)")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse DDG HTML: %w", err)
	}

	// Collect result__a links and result__snippet elements in DOM order,
	// then zip them together.
	type titleEntry struct{ text, href string }
	var titles []titleEntry
	var snippets []string

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && nodeHasClass(n, "result__a") {
			titles = append(titles, titleEntry{
				text: nodeText(n),
				href: nodeAttr(n, "href"),
			})
		}
		if n.Type == html.ElementNode && nodeHasClass(n, "result__snippet") {
			snippets = append(snippets, nodeText(n))
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var results []searchResult
	for i := 0; i < len(titles) && i < len(snippets) && len(results) < limit; i++ {
		results = append(results, searchResult{
			Title:   titles[i].text,
			URL:     decodeDDGURL(titles[i].href),
			Snippet: snippets[i],
		})
	}
	return results, nil
}

// decodeDDGURL extracts the real URL from a DuckDuckGo redirect href.
// DDG wraps result URLs as //duckduckgo.com/l/?uddg=<encoded-url>&...
func decodeDDGURL(href string) string {
	if !strings.Contains(href, "uddg=") {
		return href
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if uddg := u.Query().Get("uddg"); uddg != "" {
		return uddg
	}
	return href
}

func nodeHasClass(n *html.Node, cls string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == cls {
					return true
				}
			}
		}
	}
	return false
}

func nodeAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.TrimSpace(b.String())
}

func braveSearch(ctx context.Context, query string, limit int, apiKey string) ([]searchResult, error) {
	reqURL := fmt.Sprintf("https://api.search.brave.com/res/v1/web/search?q=%s&count=%d",
		url.QueryEscape(query), limit)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	out := make([]searchResult, 0, len(data.Web.Results))
	for _, r := range data.Web.Results {
		out = append(out, searchResult{Title: r.Title, URL: r.URL, Snippet: r.Description})
	}
	return out, nil
}

func serpapiSearch(ctx context.Context, query string, limit int, apiKey string) ([]searchResult, error) {
	reqURL := fmt.Sprintf("https://serpapi.com/search.json?q=%s&num=%d&api_key=%s&engine=google",
		url.QueryEscape(query), limit, url.QueryEscape(apiKey))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var data struct {
		OrganicResults []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic_results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}

	out := make([]searchResult, 0, len(data.OrganicResults))
	for _, r := range data.OrganicResults {
		out = append(out, searchResult{Title: r.Title, URL: r.Link, Snippet: r.Snippet})
	}
	return out, nil
}

// --- web_page_read ---

const defaultMaxBytes = 512 * 1024 // 512KB of HTML

func webPageRead(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		URL      string `json:"url"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", plugin.InvalidArgs("%v", err)
	}
	if p.MaxBytes <= 0 {
		p.MaxBytes = defaultMaxBytes
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	limited := io.LimitReader(resp.Body, int64(p.MaxBytes))
	return extractText(limited), nil
}

// skip is the set of HTML elements whose subtrees we skip entirely.
var skip = map[string]bool{
	"script": true, "style": true, "noscript": true,
	"nav": true, "footer": true, "header": true, "aside": true,
}

// block is the set of elements after which we add a newline.
var block = map[string]bool{
	"p": true, "div": true, "br": true, "li": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"tr": true, "td": true, "th": true, "blockquote": true, "pre": true,
}

func extractText(r io.Reader) string {
	doc, err := html.Parse(r)
	if err != nil {
		return ""
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skip[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			if t := strings.TrimSpace(n.Data); t != "" {
				b.WriteString(t)
				b.WriteByte(' ')
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && block[n.Data] {
			b.WriteByte('\n')
		}
	}
	walk(doc)
	return strings.TrimSpace(b.String())
}
