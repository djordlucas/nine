package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"nine/internal/docindex"
	"nine/internal/embed"
	"nine/internal/llm"
	"nine/internal/memory"
)

// docsToolDefs expose Nine's own manual — the docs/ and spec/ trees compiled
// into the binary — as a retrieval surface rather than as context. The pair is
// deliberate: doc_search returns addresses and snippets, doc_read exchanges one
// address for exact text. Nothing from the manual enters a turn unless the model
// asks for it, which is what makes it affordable to ship documentation Nine can
// consult on every question about itself.
var docsToolDefs = []llm.ToolDef{docSearchDef, docReadDef}

var docSearchDef = llm.ToolDef{
	Name:        "doc_search",
	DisplayName: "Search Docs",
	Description: "Semantically search Nine's own bundled documentation and specification by a natural-language query, returning the most relevant section addresses with snippets. Use this whenever a question is about how Nine itself works — its architecture, configuration, tools, wire protocol, or terminology — before answering from memory. Then doc_read the address for the exact text.",
	InputSchema: json.RawMessage(`{"type":"object","required":["query"],"properties":{"query":{"type":"string","description":"What you want to know about Nine"},"top_k":{"type":"integer","description":"Number of results (default 5)"},"bundle":{"type":"string","enum":["docs","spec"],"description":"Restrict to the user documentation or the specification; omit to search both"}}}`),
}

var docReadDef = llm.ToolDef{
	Name:        "doc_read",
	DisplayName: "Read Doc",
	Description: "Read a section or a whole document from Nine's bundled documentation and specification. Accepts an address from doc_search (\"docs/skills.md#tools\"), a topic name (\"agent-loop\"), or a bundle path (\"spec/contracts/wire-protocol\"). Quote what you find rather than paraphrasing when the answer turns on exact behavior: these documents are the source of truth, and a wrong reference is a bug worth reporting.",
	InputSchema: json.RawMessage(`{"type":"object","required":["ref"],"properties":{"ref":{"type":"string","description":"Section address, topic name, or bundle path"}}}`),
}

// docSnippetBytes caps the preview carried by each doc_search hit. It is sized
// to settle relevance ("is this the section I want?") and nothing more — the
// full text is one doc_read away, so a generous snippet would just be the
// context bloat this tool pair exists to avoid.
const docSnippetBytes = 320

// docHit is one doc_search result: an address to read next, plus enough context
// to choose between them without reading any.
type docHit struct {
	Addr    string  `json:"addr"`
	Title   string  `json:"title"`
	Heading string  `json:"heading,omitempty"`
	Snippet string  `json:"snippet"`
	Score   float32 `json:"score"`
}

// RegisterDocTools registers the doc_search/doc_read handlers. doc_read needs
// nothing but the embedded FS, so it is always available; doc_search is gated on
// an embedder like the other *_search tools, since without one there is no index
// to rank against (the builder omits its def to match).
func RegisterDocTools(d *Dispatcher, store *memory.Store, embedder embed.Embedder) {
	d.handlers["doc_read"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Ref string `json:"ref"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("doc_read: %w", err)
		}
		if req.Ref == "" {
			return "", fmt.Errorf("doc_read: ref is required")
		}
		sec, found, err := docindex.Fetch(req.Ref)
		if err != nil {
			return "", fmt.Errorf("doc_read: %w", err)
		}
		if !found {
			// The catalog is small enough to hand back with the error, so a
			// wrong guess resolves itself instead of costing another call.
			return "", fmt.Errorf("doc_read: no bundled document matches %q; available topics:\n%s",
				req.Ref, strings.Join(docindex.TopicLines(), "\n"))
		}
		// Markdown, not JSON: a whole document can exceed the dispatcher's
		// output cap, and a truncated JSON object is unparseable where
		// truncated Markdown is merely short. It also avoids escaping every
		// newline in a document that is mostly newlines. The address header
		// keeps the result self-identifying so a later citation stays accurate.
		var b strings.Builder
		b.WriteString(sec.Addr)
		if sec.Title != "" {
			b.WriteString(" — " + sec.Title)
		}
		if sec.Heading != "" {
			b.WriteString(" § " + sec.Heading)
		}
		b.WriteString("\n\n")
		b.WriteString(sec.Body)
		return b.String(), nil
	}

	if embedder == nil || store == nil {
		return
	}
	d.handlers["doc_search"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Query  string `json:"query"`
			TopK   int    `json:"top_k"`
			Bundle string `json:"bundle"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("doc_search: %w", err)
		}
		if req.Query == "" {
			return "", fmt.Errorf("doc_search: query is required")
		}
		if req.TopK <= 0 {
			req.TopK = 5
		}
		vec, err := embedder.Embed(context.Background(), req.Query)
		if err != nil {
			return "", fmt.Errorf("embed: %w", err)
		}
		// Two retrievers, fused. The vector side catches phrasing the documents
		// do not use literally; the lexical side supplies the inverse document
		// frequency the default embedder lacks. They fail differently, which is
		// the point — see docindex.Lexical.
		//
		// Over-fetch on both: the bundle filter and the fusion below reorder and
		// thin the candidate list, so the pool must be wider than what is returned.
		results, err := store.VectorQuery(memory.DocsNamespace, vec, req.TopK*docCandidateFactor)
		if err != nil {
			return "", fmt.Errorf("vector query: %w", err)
		}
		vectorRanked := make([]string, 0, len(results))
		score := make(map[string]float32, len(results))
		for _, r := range results {
			vectorRanked = append(vectorRanked, r.Key)
			score[r.Key] = r.Score
		}

		var lexicalRanked []string
		// A lexical index that fails to build is not fatal: degrade to
		// vector-only ranking rather than failing the search outright.
		if lex, err := docindex.LexicalIndex(); err == nil {
			lexicalRanked = lex.Rank(req.Query)
			if len(lexicalRanked) > req.TopK*docCandidateFactor {
				lexicalRanked = lexicalRanked[:req.TopK*docCandidateFactor]
			}
		}

		ranked := docindex.FuseRRF(docindex.RRFK, vectorRanked, lexicalRanked)

		hits := make([]docHit, 0, req.TopK)
		for _, addr := range ranked {
			if len(hits) >= req.TopK {
				break
			}
			if req.Bundle != "" && !strings.HasPrefix(addr, req.Bundle+"/") {
				continue
			}
			// An address that no longer resolves means the index outran the
			// binary; skip it rather than pointing the model at nothing.
			sec, found, err := docindex.Fetch(addr)
			if err != nil || !found {
				continue
			}
			hits = append(hits, docHit{
				Addr:    sec.Addr,
				Title:   sec.Title,
				Heading: sec.Heading,
				Snippet: snippet(sec.Body, docSnippetBytes),
				// The cosine score, reported for what it is. Rank fusion has no
				// meaningful score of its own, and inventing one would suggest a
				// calibration that does not exist. A section the lexical side
				// found but the vector side did not reports 0.
				Score: score[addr],
			})
		}
		data, err := json.Marshal(map[string]any{"count": len(hits), "results": hits})
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// docCandidateFactor widens each retriever's result list beyond the requested
// count, giving the bundle filter and the rank fusion room to reorder.
//
// Fusion is only as good as the pools it draws from: a document ranked 25th by
// cosine and 2nd lexically can only be rescued if the vector pool reaches 25
// deep. Sweeping this against the labelled query set, top-5 accuracy climbed
// from 25/30 at ×6 to 27/30 at ×20 and stopped improving after — past that the
// extra candidates are noise that fusion has to sort back out.
const docCandidateFactor = 20

// snippet truncates s to at most n bytes on a rune boundary, marking the cut so
// the model can tell a preview from a complete section.
func snippet(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	// Prefer the last word boundary so the preview does not end mid-token.
	if i := strings.LastIndexAny(s[:cut], " \n\t"); i > n/2 {
		cut = i
	}
	return strings.TrimSpace(s[:cut]) + "…"
}
