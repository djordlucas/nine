package runtime_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"nine/internal/embed"
	"nine/internal/memory"
	"nine/internal/runtime"
)

// docSeedEmbedder records how many sections were embedded, which is how the
// fingerprint tests observe whether a boot did work or skipped it.
type docSeedEmbedder struct {
	calls atomic.Int64
	fail  atomic.Bool
}

func (c *docSeedEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	c.calls.Add(1)
	if c.fail.Load() {
		return nil, fmt.Errorf("embedder unavailable")
	}
	// A trivial deterministic vector: content need not be meaningful for the
	// seeder's bookkeeping, only stable and non-empty.
	return []float32{float32(len(text)%97) / 97, 0.5, 0.25}, nil
}

// TestSeedDocsIndexesCorpus checks the first boot indexes every bundled section
// into the docs namespace.
func TestSeedDocsIndexesCorpus(t *testing.T) {
	store := seedTestStore(t)
	emb := &docSeedEmbedder{}

	if err := runtime.SeedDocs(store, emb, "test/v1"); err != nil {
		t.Fatalf("SeedDocs: %v", err)
	}
	n, err := store.VectorCount(memory.DocsNamespace)
	if err != nil {
		t.Fatalf("VectorCount: %v", err)
	}
	if n < 50 {
		t.Fatalf("indexed %d sections, expected the whole bundled corpus", n)
	}
	if got := emb.calls.Load(); int(got) != n {
		t.Errorf("embedded %d sections but stored %d vectors", got, n)
	}
}

// TestSeedDocsSkipsUnchangedCorpus is the property that makes boot-time
// indexing affordable under a remote embedder: an unchanged binary must not
// re-embed hundreds of sections on every restart.
func TestSeedDocsSkipsUnchangedCorpus(t *testing.T) {
	store := seedTestStore(t)
	emb := &docSeedEmbedder{}

	if err := runtime.SeedDocs(store, emb, "test/v1"); err != nil {
		t.Fatalf("first SeedDocs: %v", err)
	}
	first := emb.calls.Load()

	if err := runtime.SeedDocs(store, emb, "test/v1"); err != nil {
		t.Fatalf("second SeedDocs: %v", err)
	}
	if got := emb.calls.Load(); got != first {
		t.Errorf("second boot embedded %d more sections, want 0", got-first)
	}
	n, _ := store.VectorCount(memory.DocsNamespace)
	if int64(n) != first {
		t.Errorf("index holds %d vectors after a skipped reseed, want %d", n, first)
	}
}

// TestSeedDocsRebuildsOnEmbedderChange covers the case the corpus hash alone
// would miss: vectors from one embedder are meaningless to another, so
// switching providers must force a rebuild even though the docs are identical.
func TestSeedDocsRebuildsOnEmbedderChange(t *testing.T) {
	store := seedTestStore(t)
	emb := &docSeedEmbedder{}

	if err := runtime.SeedDocs(store, emb, "keyword/"); err != nil {
		t.Fatalf("first SeedDocs: %v", err)
	}
	first := emb.calls.Load()

	if err := runtime.SeedDocs(store, emb, "ollama/nomic-embed-text"); err != nil {
		t.Fatalf("SeedDocs after provider switch: %v", err)
	}
	if got := emb.calls.Load(); got != first*2 {
		t.Errorf("provider switch embedded %d sections, want a full rebuild of %d", got-first, first)
	}
	// A rebuild replaces the namespace rather than doubling it.
	n, _ := store.VectorCount(memory.DocsNamespace)
	if int64(n) != first {
		t.Errorf("index holds %d vectors after rebuild, want %d", n, first)
	}
}

// TestSeedDocsRetriesAfterPartialIndex ensures a failed pass does not record a
// fingerprint: a half-indexed manual that Nine believes is whole would silently
// answer from the sections that happened to make it in.
func TestSeedDocsRetriesAfterPartialIndex(t *testing.T) {
	store := seedTestStore(t)
	emb := &docSeedEmbedder{}
	emb.fail.Store(true)

	if err := runtime.SeedDocs(store, emb, "test/v1"); err != nil {
		t.Fatalf("SeedDocs with a failing embedder: %v", err)
	}
	if n, _ := store.VectorCount(memory.DocsNamespace); n != 0 {
		t.Fatalf("indexed %d vectors despite a failing embedder", n)
	}

	emb.fail.Store(false)
	if err := runtime.SeedDocs(store, emb, "test/v1"); err != nil {
		t.Fatalf("SeedDocs retry: %v", err)
	}
	if n, _ := store.VectorCount(memory.DocsNamespace); n < 50 {
		t.Errorf("retry after a partial pass indexed %d sections, want the full corpus", n)
	}
}

// TestSeedDocsWithoutEmbedder is the no-embeddings deployment: indexing is
// skipped rather than erroring, matching the builder omitting doc_search.
func TestSeedDocsWithoutEmbedder(t *testing.T) {
	store := seedTestStore(t)
	var nilEmbedder embed.Embedder
	if err := runtime.SeedDocs(store, nilEmbedder, "none/"); err != nil {
		t.Fatalf("SeedDocs with no embedder: %v", err)
	}
	if n, _ := store.VectorCount(memory.DocsNamespace); n != 0 {
		t.Errorf("indexed %d vectors with no embedder", n)
	}
}
