package docindex

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// testBundle wraps a synthetic FS so the chunker can be exercised on documents
// shaped for the test rather than on whatever the real corpus happens to hold.
func testBundle(files map[string]string) Bundle {
	m := fstest.MapFS{}
	for p, body := range files {
		m[p] = &fstest.MapFile{Data: []byte(body)}
	}
	return Bundle{FS: m, Cmd: "docs", Desc: "test"}
}

func sectionsOf(t *testing.T, b Bundle, name string) []Section {
	t.Helper()
	topics, err := b.Topics()
	if err != nil {
		t.Fatalf("Topics: %v", err)
	}
	for _, top := range topics {
		if top.Name == name {
			secs, err := b.Sections(top)
			if err != nil {
				t.Fatalf("Sections: %v", err)
			}
			return secs
		}
	}
	t.Fatalf("topic %q not found", name)
	return nil
}

// TestSectionsSplitAtH2 checks the basic unit of retrieval: preamble plus one
// section per H2, each addressed by a slug of its heading.
func TestSectionsSplitAtH2(t *testing.T) {
	b := testBundle(map[string]string{"guide.md": `# Guide

Intro prose.

## First Section

Alpha.

## Second Section

Beta.
`})
	secs := sectionsOf(t, b, "guide")
	if len(secs) != 3 {
		t.Fatalf("got %d sections, want 3: %+v", len(secs), secs)
	}
	if secs[0].Heading != "" || !strings.Contains(secs[0].Body, "Intro prose") {
		t.Errorf("preamble section = %+v", secs[0])
	}
	if secs[0].Addr != "docs/guide.md#guide" {
		t.Errorf("preamble addr = %q, want the title anchor docs/guide.md#guide", secs[0].Addr)
	}
	if secs[1].Addr != "docs/guide.md#first-section" {
		t.Errorf("addr = %q, want docs/guide.md#first-section", secs[1].Addr)
	}
	if secs[1].Title != "Guide" || secs[1].Heading != "First Section" {
		t.Errorf("section metadata = %+v", secs[1])
	}
	if strings.Contains(secs[1].Body, "Beta") {
		t.Errorf("section bled into the next: %q", secs[1].Body)
	}
}

// TestSectionsIgnoreFencedHeadings is the case that matters most for this
// corpus: the real docs quote Markdown samples (skills.md embeds an entire
// example skill file), and splitting on a heading inside a fence would shred a
// document into fragments addressing text that is really just an example.
func TestSectionsIgnoreFencedHeadings(t *testing.T) {
	b := testBundle(map[string]string{"guide.md": "# Guide\n\n## Real Section\n\n```markdown\n## Not A Section\n\nsample body\n```\n\nAfter the fence.\n"})
	secs := sectionsOf(t, b, "guide")
	for _, s := range secs {
		if s.Heading == "Not A Section" {
			t.Fatalf("split on a heading inside a code fence: %+v", s)
		}
	}
	var real Section
	for _, s := range secs {
		if s.Heading == "Real Section" {
			real = s
		}
	}
	if !strings.Contains(real.Body, "Not A Section") || !strings.Contains(real.Body, "After the fence") {
		t.Errorf("fenced sample or trailing prose lost from section: %q", real.Body)
	}
}

// TestSectionsSplitOversizedH2 verifies a long section is broken at its H3s, so
// one vector never has to stand for several unrelated subtopics.
func TestSectionsSplitOversizedH2(t *testing.T) {
	filler := strings.Repeat("padding text. ", SectionMaxBytes/10)
	b := testBundle(map[string]string{"big.md": "# Big\n\n## Huge\n\nlead-in\n\n### One\n\n" + filler + "\n\n### Two\n\n" + filler + "\n"})
	secs := sectionsOf(t, b, "big")
	var headings []string
	for _, s := range secs {
		headings = append(headings, s.Heading)
	}
	if !slices.Contains(headings, "One") || !slices.Contains(headings, "Two") {
		t.Fatalf("oversized H2 not split at its H3s, got headings %v", headings)
	}
	// The H2's own lead-in must survive under the H2 anchor rather than vanish.
	var found bool
	for _, s := range secs {
		if strings.Contains(s.Body, "lead-in") {
			found = true
		}
	}
	if !found {
		t.Error("lead-in prose before the first H3 was dropped")
	}
}

// TestAnchorsDisambiguateRepeats keeps every address resolvable to exactly one
// section when a document reuses a heading.
func TestAnchorsDisambiguateRepeats(t *testing.T) {
	b := testBundle(map[string]string{"repeat.md": "# Repeat\n\n## Notes\n\nfirst\n\n## Notes\n\nsecond\n"})
	secs := sectionsOf(t, b, "repeat")
	seen := map[string]bool{}
	for _, s := range secs {
		if seen[s.Addr] {
			t.Fatalf("duplicate address %q", s.Addr)
		}
		seen[s.Addr] = true
	}
	if !seen["docs/repeat.md#notes"] || !seen["docs/repeat.md#notes-2"] {
		t.Errorf("repeated heading not disambiguated: %v", seen)
	}
}

// TestSectionsOverRealBundles indexes the shipped corpus and asserts the two
// invariants the retrieval path depends on: addresses are unique, and every
// address a search could return can be fetched back.
func TestSectionsOverRealBundles(t *testing.T) {
	secs, err := Sections()
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	if len(secs) < 50 {
		t.Fatalf("got %d sections from the bundled corpus, expected far more", len(secs))
	}
	seen := map[string]bool{}
	var docsN, specN int
	for _, s := range secs {
		if seen[s.Addr] {
			t.Errorf("duplicate address %q", s.Addr)
		}
		seen[s.Addr] = true
		switch s.Bundle {
		case "docs":
			docsN++
		case "spec":
			specN++
		}
		if s.Body == "" {
			t.Errorf("empty section body at %q", s.Addr)
		}
	}
	if docsN == 0 || specN == 0 {
		t.Errorf("expected sections from both bundles, got docs=%d spec=%d", docsN, specN)
	}
	for _, s := range secs {
		got, found, err := Fetch(s.Addr)
		if err != nil || !found {
			t.Fatalf("Fetch(%q) = found %v, err %v", s.Addr, found, err)
		}
		if got.Body != s.Body {
			t.Errorf("Fetch(%q) returned different text than the index holds", s.Addr)
		}
	}
	// ROADMAP is hidden from the CLI listing; it must not leak in via the index.
	for _, s := range secs {
		if strings.Contains(s.Path, "ROADMAP") {
			t.Errorf("hidden document indexed: %q", s.Addr)
		}
	}
}

// TestFetchReferenceForms covers the address shapes a model or a human might
// hold, since doc_read accepts whatever doc_search, the CLI, or a guess produced.
func TestFetchReferenceForms(t *testing.T) {
	for _, ref := range []string{
		"agent-loop",
		"agent-loop.md",
		"docs/agent-loop",
		"docs/agent-loop.md",
		"./docs/agent-loop.md",
	} {
		sec, found, err := Fetch(ref)
		if err != nil || !found {
			t.Errorf("Fetch(%q) = found %v, err %v", ref, found, err)
			continue
		}
		if sec.Topic != "agent-loop" || sec.Heading != "" {
			t.Errorf("Fetch(%q) = %+v, want the whole agent-loop document", ref, sec)
		}
	}
	// A spec contract, which is only addressable by its path within the bundle.
	if _, found, err := Fetch("spec/contracts/wire-protocol"); err != nil || !found {
		t.Errorf("Fetch(spec contract) = found %v, err %v", found, err)
	}
	if _, found, _ := Fetch("no-such-topic"); found {
		t.Error("Fetch of an unknown topic reported found")
	}
}

// TestFetchSectionIsNarrowerThanDocument confirms an anchored fetch returns just
// the section — the property that keeps a doc_read from spending a whole
// document's tokens to answer one question.
func TestFetchSectionIsNarrowerThanDocument(t *testing.T) {
	secs, err := Sections()
	if err != nil {
		t.Fatalf("Sections: %v", err)
	}
	var anchored Section
	for _, s := range secs {
		if s.Heading != "" && len(s.Body) > 200 {
			anchored = s
			break
		}
	}
	if anchored.Addr == "" {
		t.Skip("no anchored section large enough to compare")
	}
	whole, found, err := Fetch(anchored.Path)
	if err != nil || !found {
		// Path is bundle-relative; qualify it and retry.
		whole, found, err = Fetch(anchored.Bundle + "/" + anchored.Path)
		if err != nil || !found {
			t.Fatalf("fetching whole document: found %v, err %v", found, err)
		}
	}
	if len(anchored.Body) >= len(whole.Body) {
		t.Errorf("section %q is not narrower than its document (%d >= %d bytes)",
			anchored.Addr, len(anchored.Body), len(whole.Body))
	}
}

// TestEmbedTextCarriesContext guards the retrieval property that a section is
// findable by words that appear only in its document title or heading.
func TestEmbedTextCarriesContext(t *testing.T) {
	s := Section{Title: "Skills", Heading: "Tools", Body: "The table below lists them."}
	got := s.EmbedText()
	if !strings.Contains(got, "Skills") || !strings.Contains(got, "Tools") || !strings.Contains(got, "table below") {
		t.Errorf("EmbedText dropped context: %q", got)
	}
}
