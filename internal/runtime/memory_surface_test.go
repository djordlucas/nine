package runtime

import (
	"context"
	"strings"
	"testing"

	"nine/internal/memory"
)

// fakeMemoryReader scripts the two reads the memory surfacer makes: rank the
// pool (VectorQuery) then resolve each hit's current value (Get).
type fakeMemoryReader struct {
	matches []memory.VectorResult
	values  map[string]string
}

func (f *fakeMemoryReader) VectorQuery(string, []float32, int) ([]memory.VectorResult, error) {
	return f.matches, nil
}
func (f *fakeMemoryReader) Get(key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func TestMemoryEnrichmentSurfacesRelevantMemories(t *testing.T) {
	r := &fakeMemoryReader{
		matches: []memory.VectorResult{
			{Key: "prefers-postgres", Score: 0.91},
			{Key: "stale-below-floor", Score: 0.40}, // below threshold — skipped
			{Key: "prefers-tabs", Score: 0.72},
		},
		values: map[string]string{
			"prefers-postgres": "user runs Postgres, not SQLite",
			"stale-below-floor": "irrelevant",
			"prefers-tabs":      "indent with tabs",
		},
	}
	got := memoryEnrichmentFn(r)(context.Background(), []float32{1, 0, 0})
	if !strings.Contains(got, "user runs Postgres, not SQLite") || !strings.Contains(got, "indent with tabs") {
		t.Errorf("enrichment = %q, want both above-threshold memories surfaced", got)
	}
	if strings.Contains(got, "irrelevant") {
		t.Errorf("enrichment = %q, want below-threshold memory omitted", got)
	}
}

func TestMemoryEnrichmentCapsAtTopN(t *testing.T) {
	matches := make([]memory.VectorResult, 0, memorySurfaceTopN+3)
	values := map[string]string{}
	for i, k := range []string{"a", "b", "c", "d", "e"} {
		matches = append(matches, memory.VectorResult{Key: k, Score: float32(0.99 - 0.01*float64(i))})
		values[k] = "value-" + k
	}
	r := &fakeMemoryReader{matches: matches, values: values}
	got := memoryEnrichmentFn(r)(context.Background(), []float32{1, 0, 0})
	if n := strings.Count(got, "value-"); n != memorySurfaceTopN {
		t.Errorf("surfaced %d memories, want exactly the top %d", n, memorySurfaceTopN)
	}
	// The top-N by score are kept; lower-ranked ones are dropped.
	if !strings.Contains(got, "value-a") || strings.Contains(got, "value-d") || strings.Contains(got, "value-e") {
		t.Errorf("enrichment = %q, want the three highest-scoring memories only", got)
	}
}

func TestMemoryEnrichmentSilentCases(t *testing.T) {
	// No query vector → nothing (background/standing sessions with no user turn).
	r := &fakeMemoryReader{
		matches: []memory.VectorResult{{Key: "k", Score: 0.99}},
		values:  map[string]string{"k": "v"},
	}
	if got := memoryEnrichmentFn(r)(context.Background(), nil); got != "" {
		t.Errorf("no query vector should surface nothing, got %q", got)
	}
	// A hit whose KV row was deleted between indexing and now is skipped, not
	// surfaced as an opaque key.
	deleted := &fakeMemoryReader{
		matches: []memory.VectorResult{{Key: "gone", Score: 0.99}},
		values:  map[string]string{},
	}
	if got := memoryEnrichmentFn(deleted)(context.Background(), []float32{1, 0, 0}); got != "" {
		t.Errorf("deleted memory should surface nothing, got %q", got)
	}
}

func TestComposeEnrichment(t *testing.T) {
	a := func(context.Context, []float32) string { return "AAA" }
	b := func(context.Context, []float32) string { return "" }
	c := func(context.Context, []float32) string { return "CCC" }

	if fn := composeEnrichment(nil, nil); fn != nil {
		t.Error("compose of all-nil should be nil so the loop keeps its no-enrichment fast path")
	}
	got := composeEnrichment(a, b, c)(context.Background(), []float32{1})
	if got != "AAA\n\nCCC" {
		t.Errorf("compose = %q, want non-empty parts joined and blanks dropped", got)
	}
}
