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

// docRetrievalCases are questions a user would plausibly ask about Nine, each
// labelled with a substring of the document that ought to answer it. They are
// deliberately phrased as questions rather than keyword queries, since that is
// what actually reaches doc_search.
var docRetrievalCases = []struct{ query, want string }{
	{"what does context_budget control", "context-builder"},
	{"how does a turn reach a worker", "agent-worker"},
	{"what happens when a tool result is too large", "tool-output-spill"},
	{"how are skills stored and retrieved", "skills"},
	{"what roles can delegate to sub agents", "roles"},
	{"what is the wire protocol between cli and daemon", "wire-protocol"},
	{"how does nine replay a session deterministically", "replay"},
	{"what happens on daemon restart to active sessions", "daemon"},
	{"how does the browser plugin take a screenshot", "browser"},
	{"what is an idle reflection session", "session-plans"},
	// Cases the default embedder currently gets wrong, kept deliberately: a
	// suite pruned to what already passes cannot show an improvement, and these
	// are the failure mode worth watching — a vocabulary-dense reference table
	// outranking the section actually about the subject.
	{"difference between a goal and a workflow", "workflow"},
	{"how do I add a custom plugin", "plugins"},
	{"how do I approve a dangerous tool call", "hitl"},
	{"how do I configure the database connection", "configuration"},
	{"how do I schedule a recurring background agent", "scheduling"},
	{"how are tools selected for a turn", "tool-exposition"},
}

// docRetrievalFloor is the number of cases above that must place the right
// document in the top 3. It is a regression floor, not a target: the default
// keyword embedder is term-frequency with no IDF, so perfect ranking is not on
// offer here, and a real embedding model scores higher. The point is that a
// change to chunking, indexing, or reranking cannot quietly make retrieval
// worse. Raise it if a change earns it.
const docRetrievalFloor = 11

// TestDocSearchRetrievalQuality measures end-to-end retrieval through the real
// tool, over the real corpus, with the default embedder.
func TestDocSearchRetrievalQuality(t *testing.T) {
	store := newTestStore(t)
	e := embed.Build("keyword", "", "")
	secs, err := docindex.Sections()
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	for _, s := range secs {
		vec, err := e.Embed(context.Background(), s.EmbedText())
		if err != nil {
			t.Fatalf("embed: %v", err)
		}
		if err := store.VectorStore(memory.DocsNamespace+":"+s.Addr, memory.DocsNamespace, s.Addr, vec); err != nil {
			t.Fatalf("VectorStore: %v", err)
		}
	}
	d := agent.New()
	agent.RegisterDocTools(d, store, e)

	var hits int
	for _, c := range docRetrievalCases {
		args, _ := json.Marshal(map[string]any{"query": c.query, "top_k": 3})
		res, err := d.Dispatch(context.Background(), "doc_search", args)
		if err != nil {
			t.Fatalf("doc_search(%q): %v", c.query, err)
		}
		var out struct {
			Results []struct {
				Addr string `json:"addr"`
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		var found bool
		for _, h := range out.Results {
			if strings.Contains(h.Addr, c.want) {
				found = true
				break
			}
		}
		if found {
			hits++
		} else {
			top := "(none)"
			if len(out.Results) > 0 {
				top = out.Results[0].Addr
			}
			t.Logf("miss: %q wanted %q, top hit %s", c.query, c.want, top)
		}
	}
	t.Logf("retrieval: %d/%d cases hit in top 3", hits, len(docRetrievalCases))
	if hits < docRetrievalFloor {
		t.Errorf("retrieval regressed: %d/%d in top 3, floor is %d",
			hits, len(docRetrievalCases), docRetrievalFloor)
	}
}
