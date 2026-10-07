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
	{"what happens when a tool result is too large", "tool-output"},
	{"how are skills stored and retrieved", "skills"},
	{"what roles can delegate to sub agents", "roles"},
	{"what is the wire protocol between cli and daemon", "wire-protocol"},
	{"how does nine replay a session deterministically", "replay"},
	{"what happens on daemon restart to active sessions", "daemon"},
	{"how does the browser plugin take a screenshot", "browser"},
	{"what is a process session", "processes"},
	// Cases the default embedder gets wrong or nearly wrong, kept deliberately:
	// a suite pruned to what already passes cannot show an improvement, and
	// these are the failure mode worth watching — a vocabulary-dense reference
	// table outranking the section actually about the subject.
	{"difference between a goal and a workflow", "workflow"},
	{"how do I add a custom plugin", "plugins"},
	{"how do I approve a dangerous tool call", "hitl"},
	{"how do I configure the database connection", "configuration"},
	{"how do I schedule a recurring background agent", "scheduling"},
	{"how are tools selected for a turn", "tool-selection"},
	{"how do I install nine", "installation"},
	{"how does nine decide whether to think before acting", "thinking"},
	{"how do plugins communicate over http", "http-transport"},
	{"what environment variables does a plugin receive", "plugin-capabilities"},
	{"how do I run everything in a single docker container", "single-container"},
	{"what is the append only event journal", "event-journal"},
	{"how do I subscribe to journal events", "event-journal"},
	{"what slash commands does the tui support", "slash"},
	{"how do I declare a standing agent in configuration", "predefined-agents"},
	{"can nine modify its own source code", "self-modification"},
	{"how are evals run against recorded replays", "evals"},
	{"what version numbering scheme does nine use", "versioning"},
	{"how does the supervisor detect a stalled session", "supervisor"},
	{"what is stored in a checkpoint", "checkpoint"},
}

// Regression floors: how many of the cases above must place the right document
// in the top 3, and in the top 5. They are floors, not targets — the default
// keyword embedder is term frequency with no IDF, so perfect ranking is not on
// offer, and a real embedding model scores higher. The point is that a change to
// chunking, indexing, or fusion cannot quietly make retrieval worse.
//
// Top 5 is the number that matters most in practice, since that is what
// doc_search returns by default: it decides whether the answer is in front of
// the model at all. Measured when rank fusion landed: 24/30 and 27/30, up from
// 22/30 and 23/30 on cosine alone. The floors sit a couple below, because
// editing the bundled docs shifts these without any code regression.
const (
	docRetrievalFloorTop3 = 22
	docRetrievalFloorTop5 = 25
)

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

	var top3, top5 int
	for _, c := range docRetrievalCases {
		args, _ := json.Marshal(map[string]any{"query": c.query, "top_k": 5})
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
		rank := -1
		for i, h := range out.Results {
			if strings.Contains(h.Addr, c.want) {
				rank = i
				break
			}
		}
		switch {
		case rank < 0:
			top := "(none)"
			if len(out.Results) > 0 {
				top = out.Results[0].Addr
			}
			t.Logf("miss: %q wanted %q, top hit %s", c.query, c.want, top)
		case rank < 3:
			top3++
			top5++
		default:
			top5++
		}
	}
	n := len(docRetrievalCases)
	t.Logf("retrieval: top3=%d/%d top5=%d/%d", top3, n, top5, n)
	if top3 < docRetrievalFloorTop3 {
		t.Errorf("retrieval regressed: %d/%d in top 3, floor is %d", top3, n, docRetrievalFloorTop3)
	}
	if top5 < docRetrievalFloorTop5 {
		t.Errorf("retrieval regressed: %d/%d in top 5, floor is %d", top5, n, docRetrievalFloorTop5)
	}
}
