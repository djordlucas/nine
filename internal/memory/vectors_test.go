package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

func TestVectorStoreQueryNearest(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Three unit vectors in a namespace; a query closest to "a".
	if err := store.VectorStore("id-a", "ns", "a", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.VectorStore("id-b", "ns", "b", []float32{0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.VectorStore("id-c", "ns", "c", []float32{0.9, 0.1, 0}); err != nil {
		t.Fatal(err)
	}

	res, err := store.VectorQuery("ns", []float32{1, 0, 0}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("VectorQuery returned %d, want 3", len(res))
	}
	// Ordered by descending similarity: a (1.0) ≥ c (~0.99) ≥ b (0).
	if res[0].Key != "a" {
		t.Errorf("nearest = %q, want a", res[0].Key)
	}
	if res[len(res)-1].Key != "b" {
		t.Errorf("farthest = %q, want b", res[len(res)-1].Key)
	}
	if !(res[0].Score >= res[1].Score && res[1].Score >= res[2].Score) {
		t.Errorf("scores not descending: %+v", res)
	}
}

func TestVectorNamespaceIsolationAndDelete(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.VectorStore("x", "ns1", "x", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.VectorStore("y", "ns2", "y", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}

	// A query in ns1 never sees ns2's vectors.
	res, _ := store.VectorQuery("ns1", []float32{1, 0}, 5)
	if len(res) != 1 || res[0].Key != "x" {
		t.Errorf("ns1 query = %+v, want only x", res)
	}

	// Empty vector is rejected.
	if err := store.VectorStore("z", "ns1", "z", nil); err == nil {
		t.Error("VectorStore with an empty vector should error")
	}

	// Delete removes a vector.
	if err := store.VectorDelete("x"); err != nil {
		t.Fatal(err)
	}
	if res, _ := store.VectorQuery("ns1", []float32{1, 0}, 5); len(res) != 0 {
		t.Errorf("after delete ns1 query = %+v, want empty", res)
	}
}

func TestVectorUpsertByID(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	// Storing the same id twice replaces (ON CONFLICT (id)) rather than duplicating.
	if err := store.VectorStore("same", "ns", "k", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := store.VectorStore("same", "ns", "k", []float32{0, 1, 0}); err != nil {
		t.Fatal(err)
	}
	res, _ := store.VectorQuery("ns", []float32{0, 1, 0}, 5)
	if len(res) != 1 {
		t.Fatalf("upsert produced %d rows, want 1", len(res))
	}
	if res[0].Score < 0.99 {
		t.Errorf("upsert did not replace the vector; score=%.3f", res[0].Score)
	}
}
