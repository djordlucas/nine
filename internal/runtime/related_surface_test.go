package runtime

import (
	"context"
	"strings"
	"testing"

	"nine/internal/memory"
)

// fakeRelatedReader scripts the three reads the surfacer makes.
type fakeRelatedReader struct {
	links   []memory.RelatedSession
	matches []memory.VectorResult
	gists   map[string]string
}

func (f *fakeRelatedReader) RelatedSessions(string) ([]memory.RelatedSession, error) {
	return f.links, nil
}
func (f *fakeRelatedReader) VectorQuery(string, []float32, int) ([]memory.VectorResult, error) {
	return f.matches, nil
}
func (f *fakeRelatedReader) LatestTurnResult(agentID string) (string, error) {
	return f.gists[agentID], nil
}

func TestRelatedEnrichmentSurfacesRelevantLink(t *testing.T) {
	r := &fakeRelatedReader{
		links: []memory.RelatedSession{{RelatedAgentID: "B", Score: 0.90}},
		matches: []memory.VectorResult{
			{Key: "A", Score: 0.99}, // self — skipped
			{Key: "B", Score: 0.88}, // linked + above threshold — surfaced
		},
		gists: map[string]string{"B": "we chose Postgres over SQLite"},
	}
	got := relatedEnrichmentFn(r, "A")(context.Background(), []float32{1, 0, 0})
	if !strings.Contains(got, "we chose Postgres over SQLite") {
		t.Errorf("enrichment = %q, want the related session's gist surfaced", got)
	}
}

func TestRelatedEnrichmentSilentWhenNotRelevantToQuery(t *testing.T) {
	// B is a recorded link, but the current query isn't close to it.
	r := &fakeRelatedReader{
		links:   []memory.RelatedSession{{RelatedAgentID: "B", Score: 0.90}},
		matches: []memory.VectorResult{{Key: "B", Score: 0.40}}, // below threshold
		gists:   map[string]string{"B": "unrelated topic"},
	}
	if got := relatedEnrichmentFn(r, "A")(context.Background(), []float32{1, 0, 0}); got != "" {
		t.Errorf("enrichment = %q, want empty when no link is relevant to the query", got)
	}
}

func TestRelatedEnrichmentSilentWhenMatchNotLinked(t *testing.T) {
	// A vector match that isn't a recorded link must not be surfaced — the
	// derived store is the gate, the query only ranks within it.
	r := &fakeRelatedReader{
		links:   []memory.RelatedSession{{RelatedAgentID: "B", Score: 0.90}},
		matches: []memory.VectorResult{{Key: "C", Score: 0.95}}, // relevant but unlinked
		gists:   map[string]string{"C": "some other session"},
	}
	if got := relatedEnrichmentFn(r, "A")(context.Background(), []float32{1, 0, 0}); got != "" {
		t.Errorf("enrichment = %q, want empty when the relevant match is not a recorded link", got)
	}
}

func TestRelatedEnrichmentNoLinksOrNoQuery(t *testing.T) {
	withLinks := &fakeRelatedReader{
		links:   []memory.RelatedSession{{RelatedAgentID: "B", Score: 0.9}},
		matches: []memory.VectorResult{{Key: "B", Score: 0.9}},
		gists:   map[string]string{"B": "gist"},
	}
	if got := relatedEnrichmentFn(withLinks, "A")(context.Background(), nil); got != "" {
		t.Errorf("no query vector should surface nothing, got %q", got)
	}
	noLinks := &fakeRelatedReader{}
	if got := relatedEnrichmentFn(noLinks, "A")(context.Background(), []float32{1, 0, 0}); got != "" {
		t.Errorf("no recorded links should surface nothing, got %q", got)
	}
}

func TestRelatedEnrichmentSkipsEmptyGist(t *testing.T) {
	// A linked, relevant session with no recorded turn text is skipped rather
	// than surfaced as an opaque id.
	r := &fakeRelatedReader{
		links:   []memory.RelatedSession{{RelatedAgentID: "B", Score: 0.90}},
		matches: []memory.VectorResult{{Key: "B", Score: 0.88}},
		gists:   map[string]string{}, // no gist for B
	}
	if got := relatedEnrichmentFn(r, "A")(context.Background(), []float32{1, 0, 0}); got != "" {
		t.Errorf("enrichment = %q, want empty when the link has no gist", got)
	}
}
