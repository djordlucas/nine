package keyword

import (
	"context"
	"math"
	"testing"
)

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// R-EMB.5: the keyword embedder MUST be deterministic for identical input, so
// re-indexing is stable.
func TestKeywordDeterministic(t *testing.T) {
	e := New()
	ctx := context.Background()
	v1, err := e.Embed(ctx, "read a file from disk")
	if err != nil {
		t.Fatal(err)
	}
	v2, err := e.Embed(ctx, "read a file from disk")
	if err != nil {
		t.Fatal(err)
	}
	if len(v1) != len(v2) {
		t.Fatalf("dim mismatch: %d vs %d", len(v1), len(v2))
	}
	for i := range v1 {
		if v1[i] != v2[i] {
			t.Fatalf("not deterministic at index %d: %v vs %v", i, v1[i], v2[i])
		}
	}
}

// Every vector has the fixed dimensionality and is L2-normalized to unit length
// (or exactly zero when there is no signal).
func TestKeywordDimsAndNorm(t *testing.T) {
	e := New()
	v, err := e.Embed(context.Background(), "postgres pgvector database")
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != dims {
		t.Errorf("dim = %d, want %d", len(v), dims)
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if math.Abs(math.Sqrt(sum)-1.0) > 1e-4 {
		t.Errorf("L2 norm = %v, want 1.0", math.Sqrt(sum))
	}
}

// Text with no scorable tokens (empty, whitespace, or only stop words) yields a
// zero vector — the "no signal" case ranking treats as score 0.
func TestKeywordEmptyAndStopWords(t *testing.T) {
	e := New()
	for _, in := range []string{"", "   ", "the and or of to"} {
		v, err := e.Embed(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if len(v) != dims {
			t.Errorf("%q: dim = %d, want %d", in, len(v), dims)
		}
		for _, x := range v {
			if x != 0 {
				t.Errorf("%q: expected all-zero vector, found %v", in, x)
				break
			}
		}
	}
}

// The embedding is usable for ranking: text that shares content words is more
// similar than unrelated text.
func TestKeywordSimilarityOrdering(t *testing.T) {
	e := New()
	ctx := context.Background()
	query, _ := e.Embed(ctx, "read the contents of a file")
	related, _ := e.Embed(ctx, "read a file from the filesystem")
	unrelated, _ := e.Embed(ctx, "send an email to a colleague")

	if cosine(query, related) <= cosine(query, unrelated) {
		t.Errorf("related=%.3f should exceed unrelated=%.3f",
			cosine(query, related), cosine(query, unrelated))
	}
}

// Tokenization is case- and punctuation-insensitive, so casing/punctuation
// don't change the embedding.
func TestKeywordCaseInsensitive(t *testing.T) {
	e := New()
	ctx := context.Background()
	a, _ := e.Embed(ctx, "Read-File: contents!")
	b, _ := e.Embed(ctx, "read file contents")
	if cosine(a, b) < 0.999 {
		t.Errorf("case/punctuation changed the embedding: cosine=%.4f", cosine(a, b))
	}
}
