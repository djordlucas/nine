package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

func TestReflectionsCreateAndList(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := store.ReflectionList(); len(got) != 0 {
		t.Fatalf("empty ReflectionList = %v", got)
	}

	if err := store.ReflectionCreate("r1", "learned about pgvector"); err != nil {
		t.Fatal(err)
	}
	if err := store.ReflectionCreate("r2", "cleaned up stale goals"); err != nil {
		t.Fatal(err)
	}

	got, err := store.ReflectionList()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ReflectionList = %d, want 2", len(got))
	}
	summaries := map[string]bool{}
	for _, r := range got {
		summaries[r.Summary] = true
		if r.RanAt == "" {
			t.Errorf("reflection %q has empty ran_at", r.ID)
		}
	}
	if !summaries["learned about pgvector"] || !summaries["cleaned up stale goals"] {
		t.Errorf("missing summaries: %+v", got)
	}
}
