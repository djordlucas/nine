package toolvm

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The shipped HTTP tools against the real network.
//
// Opt-in, like the live-model and browser tests: it needs egress, and one of the
// four scrapes a third party. Run it with:
//
//	NINE_NETWORK_TEST=1 go test ./internal/toolvm -run Network
//
// It exists because nothing else covers these tools. No eval case calls
// http_get, http_post, web_page_read or web_search — the live matrix would have
// reported a clean run while the riskiest part of their migration went
// unexercised. That part is the extraction: the plugin used Go's
// golang.org/x/net/html, and these use nine:html, a tokenizer. A substitution
// like that is not provable against fixtures alone, because the inputs that
// break parsers are the ones real pages contain.
func TestNetworkShippedHTTPTools(t *testing.T) {
	if os.Getenv("NINE_NETWORK_TEST") != "1" {
		t.Skip("network test; set NINE_NETWORK_TEST=1 to run")
	}
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.LoadShipped(ctx, nil)

	t.Run("http_get returns a real response", func(t *testing.T) {
		out, err := h.Call(ctx, "http_get", json.RawMessage(`{"url":"https://example.com"}`))
		if err != nil {
			t.Fatalf("http_get: %v", err)
		}
		var got struct {
			Status int    `json:"status"`
			Body   string `json:"body"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if got.Status != 200 || !strings.Contains(got.Body, "Example Domain") {
			t.Errorf("status=%d body=%.80s", got.Status, got.Body)
		}
	})

	// The substitution that needed proving: markup out, text in, block structure
	// preserved. example.com is stable enough to assert against.
	t.Run("web_page_read extracts text from a real page", func(t *testing.T) {
		out, err := h.Call(ctx, "web_page_read", json.RawMessage(`{"url":"https://example.com"}`))
		if err != nil {
			t.Fatalf("web_page_read: %v", err)
		}
		var got struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !strings.Contains(got.Text, "Example Domain") {
			t.Errorf("extraction lost the title: %q", got.Text)
		}
		for _, leak := range []string{"<", ">", "</", "<style"} {
			if strings.Contains(got.Text, leak) {
				t.Errorf("markup leaked into the text (%q): %q", leak, got.Text)
			}
		}
	})

	// The wildcard grant admits any host. It must still not reach an address the
	// dialer refuses — this is the property that makes migrating these tools an
	// improvement over the plugin, which had no such check at any layer.
	t.Run("wildcard does not reach cloud metadata", func(t *testing.T) {
		_, err := h.Call(ctx, "http_get",
			json.RawMessage(`{"url":"http://169.254.169.254/latest/meta-data/"}`))
		if err == nil {
			t.Fatal("cloud instance metadata was reachable under a wildcard grant")
		}
		if !strings.Contains(err.Error(), "link-local") {
			t.Errorf("error = %v, want the link-local refusal", err)
		}
	})

	// web_search scrapes a third party, so a network failure here is not a defect
	// in Nine — the plugin had the same exposure. The assertion is on the *shape*
	// when it does answer: that findByClass pulled a title, a URL and a snippet
	// out of DuckDuckGo's real HTML.
	t.Run("web_search parses real results", func(t *testing.T) {
		out, err := h.Call(ctx, "web_search", json.RawMessage(`{"query":"golang wazero","limit":3}`))
		if err != nil {
			t.Skipf("search endpoint unavailable (not a Nine defect): %v", err)
		}
		var results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		}
		if err := json.Unmarshal([]byte(out), &results); err != nil {
			t.Fatalf("unmarshal %.200s: %v", out, err)
		}
		if len(results) == 0 {
			t.Skip("no results returned; the endpoint may be rate-limiting")
		}
		r := results[0]
		if r.Title == "" || r.Snippet == "" {
			t.Errorf("result missing text: %+v", r)
		}
		// The redirector must have been decoded, or the model gets a DDG link
		// instead of the page.
		if !strings.HasPrefix(r.URL, "http") || strings.Contains(r.URL, "uddg=") {
			t.Errorf("url not decoded out of the redirector: %q", r.URL)
		}
	})
}
