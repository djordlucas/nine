package docindex

import (
	"strings"
	"testing"
)

// TestStemBridgesWordForms covers the job the stemmer exists for: letting a
// question's wording reach a document's wording.
func TestStemBridgesWordForms(t *testing.T) {
	pairs := [][2]string{
		{"configure", "configuration"},
		{"schedule", "scheduling"},
		{"retrieved", "retrieval"},
		{"delegating", "delegate"},
	}
	for _, p := range pairs {
		if stem(p[0]) != stem(p[1]) {
			t.Errorf("stem(%q)=%q and stem(%q)=%q do not meet", p[0], stem(p[0]), p[1], stem(p[1]))
		}
	}
	// The length guard must keep short words from collapsing into collisions.
	for _, w := range []string{"goals", "ties", "was", "ends"} {
		if len(stem(w)) < 3 {
			t.Errorf("stem(%q) = %q, over-stemmed", w, stem(w))
		}
	}
}

// TestLexicalIDFBeatsTermFrequency is the reason this index exists: a long
// section that mentions a term once among many must not outrank a short section
// that is about it. This is the exact failure the plain keyword embedder shows.
func TestLexicalIDFBeatsTermFrequency(t *testing.T) {
	// IDF is a corpus statistic, so the corpus has to be big enough to have
	// one: "agent" must be common across documents and "schedule" rare. With
	// only two documents every term looks equally rare and IDF says nothing.
	secs := []Section{
		// The failure case: long, vocabulary-dense, mentions the rare term once.
		{Addr: "docs/matrix.md#compatibility", Title: "Model compatibility", Heading: "Compatibility matrix",
			Body: strings.Repeat("daemon session agent tool plugin workflow memory context ", 60) + " scheduling "},
		// The right answer: short and actually about it.
		{Addr: "docs/scheduling.md#cron-triggers", Title: "Scheduling", Heading: "Cron triggers",
			Body: "Interval and cron wake triggers for standing agents."},
	}
	// Background documents that make "agent" ordinary and "schedule" distinctive.
	for _, topic := range []string{"daemon", "plugins", "memory", "workflows", "roles", "hitl", "evals", "browser"} {
		secs = append(secs, Section{
			Addr: "docs/" + topic + ".md#overview", Title: topic, Heading: "Overview",
			Body: "The agent uses the " + topic + " subsystem during a session with tools and context.",
		})
	}
	l := newLexical(secs)

	ranked := l.Rank("how do I schedule a recurring agent")
	if len(ranked) == 0 {
		t.Fatal("no results")
	}
	if !strings.Contains(ranked[0], "scheduling.md") {
		t.Errorf("ranked %q first; the vocabulary-dense section won on raw term frequency", ranked[0])
	}
}

// TestLexicalRankSkipsNonMatches keeps the result usable as a candidate list
// rather than a ranking of the whole corpus.
func TestLexicalRankSkipsNonMatches(t *testing.T) {
	l := newLexical([]Section{
		{Addr: "docs/a.md#one", Title: "Alpha", Body: "workflows and goals"},
		{Addr: "docs/b.md#two", Title: "Beta", Body: "entirely unrelated prose"},
	})
	ranked := l.Rank("workflow")
	if len(ranked) != 1 || !strings.Contains(ranked[0], "a.md") {
		t.Errorf("Rank returned %v, want only the matching section", ranked)
	}
	if got := l.Rank("nothing here matches xyzzy"); len(got) != 0 {
		t.Errorf("Rank on a non-matching query returned %v", got)
	}
}

// TestFuseRRFPrefersConsensus checks the property that makes fusion worth doing:
// a document both retrievers like beats one that only a single retriever ranks
// first.
func TestFuseRRFPrefersConsensus(t *testing.T) {
	vector := []string{"solo-a", "both", "filler"}
	lexical := []string{"solo-b", "both", "filler"}
	fused := FuseRRF(RRFK, vector, lexical)
	if fused[0] != "both" {
		t.Errorf("fused = %v, want the consensus document first", fused)
	}
}

// TestFuseRRFHandlesDisjointLists confirms a document found by only one
// retriever still survives fusion — the case that lets the lexical side rescue
// what the vector side missed entirely.
func TestFuseRRFHandlesDisjointLists(t *testing.T) {
	fused := FuseRRF(RRFK, []string{"a", "b"}, []string{"c"})
	if len(fused) != 3 {
		t.Fatalf("fused = %v, want all three documents", fused)
	}
	var sawC bool
	for _, f := range fused {
		if f == "c" {
			sawC = true
		}
	}
	if !sawC {
		t.Error("a document ranked by only one retriever was dropped")
	}
	if got := FuseRRF(RRFK); len(got) != 0 {
		t.Errorf("FuseRRF() with no lists = %v", got)
	}
}

// TestLexicalIndexOverRealCorpus exercises the cached process-wide index and
// the contract doc_search depends on: every ranked address is fetchable.
func TestLexicalIndexOverRealCorpus(t *testing.T) {
	l, err := LexicalIndex()
	if err != nil {
		t.Fatalf("LexicalIndex: %v", err)
	}
	ranked := l.Rank("how are skills stored and retrieved")
	if len(ranked) == 0 {
		t.Fatal("no results over the real corpus")
	}
	for i, addr := range ranked {
		if i >= 5 {
			break
		}
		if _, found, err := Fetch(addr); err != nil || !found {
			t.Errorf("ranked address %q is not fetchable (found %v, err %v)", addr, found, err)
		}
	}
	// The same call must return the cached index, not rebuild it.
	l2, _ := LexicalIndex()
	if l != l2 {
		t.Error("LexicalIndex rebuilt instead of returning the cached index")
	}
}
