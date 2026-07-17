package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

func TestRelatedSessions(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Self-links and empty ids are ignored.
	if err := store.RelatedSessionAdd("a", "a", 1.0); err != nil {
		t.Fatal(err)
	}
	if err := store.RelatedSessionAdd("a", "", 1.0); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.RelatedSessions("a"); len(got) != 0 {
		t.Fatalf("self/empty links recorded: %+v", got)
	}

	// Two links, returned most-similar first.
	if err := store.RelatedSessionAdd("a", "b", 0.80); err != nil {
		t.Fatal(err)
	}
	if err := store.RelatedSessionAdd("a", "c", 0.92); err != nil {
		t.Fatal(err)
	}
	got, err := store.RelatedSessions("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].RelatedAgentID != "c" || got[1].RelatedAgentID != "b" {
		t.Fatalf("RelatedSessions = %+v, want [c, b] by score desc", got)
	}

	// Upsert: re-adding a pair updates the score in place, no duplicate row.
	if err := store.RelatedSessionAdd("a", "b", 0.99); err != nil {
		t.Fatal(err)
	}
	got, _ = store.RelatedSessions("a")
	if len(got) != 2 {
		t.Fatalf("upsert produced a duplicate: %+v", got)
	}
	if got[0].RelatedAgentID != "b" || got[0].Score != 0.99 {
		t.Errorf("after upsert, top = %+v, want b @0.99", got[0])
	}
}
