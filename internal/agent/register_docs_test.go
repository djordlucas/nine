package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/agent"
	"nine/internal/docindex"
	"nine/internal/embed"
	"nine/internal/memory"
)

// docDispatcher wires the doc tools over a real store, indexing the bundled
// corpus with a deterministic stand-in embedder so doc_search has something to
// rank without depending on a live embedding provider.
func docDispatcher(t *testing.T) (*agent.Dispatcher, *memory.Store) {
	t.Helper()
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterDocTools(d, store, docTestEmbedder())
	return d, store
}

// docTestEmbedder maps text to a crude bag-of-characters vector. It is not
// semantic, but it is deterministic and gives similar strings similar vectors —
// enough for the handler tests, which check plumbing rather than ranking quality.
func docTestEmbedder() embed.Embedder {
	return embed.EmbedderFunc(func(_ context.Context, text string) ([]float32, error) {
		vec := make([]float32, 26)
		for _, r := range strings.ToLower(text) {
			if r >= 'a' && r <= 'z' {
				vec[r-'a']++
			}
		}
		return vec, nil
	})
}

// indexDocs puts the real bundled sections into the docs namespace.
func indexDocs(t *testing.T, store *memory.Store) []docindex.Section {
	t.Helper()
	secs, err := docindex.Sections()
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	e := docTestEmbedder()
	for _, s := range secs {
		vec, err := e.Embed(context.Background(), s.EmbedText())
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		if err := store.VectorStore(memory.DocsNamespace+":"+s.Addr, memory.DocsNamespace, s.Addr, vec); err != nil {
			t.Fatalf("VectorStore: %v", err)
		}
	}
	return secs
}

// TestDocReadWholeDocument covers the plain "read me this topic" path, by the
// short name a user or the CLI would use. The result is Markdown behind an
// address header, so it survives the dispatcher's output cap: a document large
// enough to be truncated still reads as a document.
func TestDocReadWholeDocument(t *testing.T) {
	d, _ := docDispatcher(t)
	res, err := d.Dispatch(context.Background(), "doc_read", json.RawMessage(`{"ref":"glossary"}`))
	if err != nil {
		t.Fatalf("doc_read: %v", err)
	}
	header, body, ok := strings.Cut(res.Output, "\n\n")
	if !ok {
		t.Fatalf("doc_read output has no address header: %q", res.Output)
	}
	if !strings.HasPrefix(header, "docs/glossary.md") {
		t.Errorf("header = %q, want it to name the address", header)
	}
	if !strings.Contains(body, "# Glossary") {
		t.Errorf("body does not look like the glossary: %q", truncateForLog(body))
	}
}

// truncateForLog keeps a failure message readable when the body is a whole
// document.
func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// TestDocReadSectionIsNarrow is the context-economy property: reading an
// address returns that section, not the document that contains it.
func TestDocReadSectionIsNarrow(t *testing.T) {
	d, _ := docDispatcher(t)
	secs, err := docindex.Sections()
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	var target docindex.Section
	for _, s := range secs {
		if s.Heading != "" && len(s.Body) > 300 {
			target = s
			break
		}
	}
	args, _ := json.Marshal(map[string]string{"ref": target.Addr})
	res, err := d.Dispatch(context.Background(), "doc_read", args)
	if err != nil {
		t.Fatalf("doc_read(%s): %v", target.Addr, err)
	}
	header, body, ok := strings.Cut(res.Output, "\n\n")
	if !ok || !strings.HasPrefix(header, target.Addr) {
		t.Fatalf("doc_read(%s) header = %q", target.Addr, header)
	}
	if body != target.Body {
		t.Errorf("doc_read(%s) returned different text than the index holds", target.Addr)
	}
	whole, _, err := docindex.Fetch(target.Bundle + "/" + target.Path)
	if err != nil {
		t.Fatalf("fetch whole: %v", err)
	}
	if len(body) >= len(whole.Body) {
		t.Errorf("section read returned %d bytes, the whole document is %d", len(body), len(whole.Body))
	}
}

// TestDocReadUnknownRefListsTopics checks a wrong guess answers itself: the
// error carries the catalog, so recovering costs no extra tool call.
func TestDocReadUnknownRefListsTopics(t *testing.T) {
	d, _ := docDispatcher(t)
	_, err := d.Dispatch(context.Background(), "doc_read", json.RawMessage(`{"ref":"how-nine-works"}`))
	if err == nil {
		t.Fatal("doc_read of an unknown ref succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, "docs/glossary") || !strings.Contains(msg, "spec/overview") {
		t.Errorf("error does not list the catalog: %q", msg)
	}
}

// TestDocSearchReturnsReadableAddresses is the contract between the two tools:
// every address doc_search hands back must be one doc_read accepts.
func TestDocSearchReturnsReadableAddresses(t *testing.T) {
	d, store := docDispatcher(t)
	indexDocs(t, store)

	res, err := d.Dispatch(context.Background(), "doc_search",
		json.RawMessage(`{"query":"how does the daemon route a turn to a worker","top_k":5}`))
	if err != nil {
		t.Fatalf("doc_search: %v", err)
	}
	var out struct {
		Count   int `json:"count"`
		Results []struct {
			Addr    string `json:"addr"`
			Title   string `json:"title"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
		t.Fatalf("unmarshal: %v (%q)", err, res.Output)
	}
	if out.Count == 0 || len(out.Results) != out.Count {
		t.Fatalf("doc_search returned count=%d results=%d", out.Count, len(out.Results))
	}
	for _, hit := range out.Results {
		if hit.Snippet == "" || hit.Title == "" {
			t.Errorf("hit %q missing snippet or title", hit.Addr)
		}
		if len(hit.Snippet) > 400 {
			t.Errorf("hit %q snippet is %d bytes, want a preview", hit.Addr, len(hit.Snippet))
		}
		args, _ := json.Marshal(map[string]string{"ref": hit.Addr})
		if _, err := d.Dispatch(context.Background(), "doc_read", args); err != nil {
			t.Errorf("doc_read rejected the address doc_search returned (%q): %v", hit.Addr, err)
		}
	}
}

// TestDocSearchBundleFilter checks the filter still fills top_k rather than
// returning short because the unfiltered ranking happened to favour one bundle.
func TestDocSearchBundleFilter(t *testing.T) {
	d, store := docDispatcher(t)
	indexDocs(t, store)

	for _, bundle := range []string{"docs", "spec"} {
		args, _ := json.Marshal(map[string]any{"query": "agent loop tool calls", "top_k": 3, "bundle": bundle})
		res, err := d.Dispatch(context.Background(), "doc_search", args)
		if err != nil {
			t.Fatalf("doc_search(%s): %v", bundle, err)
		}
		var out struct {
			Results []struct {
				Addr string `json:"addr"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(out.Results) != 3 {
			t.Errorf("bundle=%s returned %d hits, want 3", bundle, len(out.Results))
		}
		for _, hit := range out.Results {
			if !strings.HasPrefix(hit.Addr, bundle+"/") {
				t.Errorf("bundle=%s returned out-of-bundle hit %q", bundle, hit.Addr)
			}
		}
	}
}

// TestDocSearchRequiresEmbedder pins the gating: with no embedder there is no
// index to rank, so doc_search must not be dispatchable while doc_read — which
// only needs the embedded FS — still is.
func TestDocSearchRequiresEmbedder(t *testing.T) {
	store := newTestStore(t)
	d := agent.New()
	agent.RegisterDocTools(d, store, nil)

	if _, err := d.Dispatch(context.Background(), "doc_search", json.RawMessage(`{"query":"anything"}`)); err == nil {
		t.Error("doc_search dispatched without an embedder")
	}
	if _, err := d.Dispatch(context.Background(), "doc_read", json.RawMessage(`{"ref":"glossary"}`)); err != nil {
		t.Errorf("doc_read should work without an embedder: %v", err)
	}
}

// TestDocSearchStaleAddressSkipped covers index/binary drift: an address left
// over from an older corpus must be dropped rather than handed to the model as
// a section it can read.
func TestDocSearchStaleAddressSkipped(t *testing.T) {
	d, store := docDispatcher(t)
	indexDocs(t, store)
	stale := "docs/removed-topic.md#gone"
	vec, _ := docTestEmbedder().Embed(context.Background(), "daemon worker turn routing")
	if err := store.VectorStore(memory.DocsNamespace+":"+stale, memory.DocsNamespace, stale, vec); err != nil {
		t.Fatalf("VectorStore: %v", err)
	}

	res, err := d.Dispatch(context.Background(), "doc_search",
		json.RawMessage(`{"query":"daemon worker turn routing","top_k":10}`))
	if err != nil {
		t.Fatalf("doc_search: %v", err)
	}
	if strings.Contains(res.Output, stale) {
		t.Errorf("stale address surfaced in results: %s", res.Output)
	}
}
