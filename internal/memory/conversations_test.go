package memory_test

import (
	"encoding/json"
	"testing"

	"nine/internal/memory/memtest"
)

func TestConversationCheckpointRoundTrip(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Missing conversation checkpoint: found=false, distinguishable from error.
	if _, found, err := store.ConversationLoad("nope"); err != nil || found {
		t.Fatalf("missing checkpoint: found=%v err=%v, want false/nil", found, err)
	}

	blob := []byte(`{"history":[{"role":"user"}],"scratchpad":[],"queued_messages":[]}`)
	if err := store.ConversationSave("c1", blob); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.ConversationLoad("c1")
	if err != nil || !found {
		t.Fatalf("ConversationLoad: found=%v err=%v", found, err)
	}
	// Compare as JSON (storage may re-serialize).
	var a, b any
	_ = json.Unmarshal(blob, &a)
	_ = json.Unmarshal(got, &b)
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	if string(ab) != string(bb) {
		t.Errorf("checkpoint round-trip mismatch: %s vs %s", ab, bb)
	}
}

func TestConversationCreateStatusAndName(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.ConversationCreate("c2"); err != nil {
		t.Fatal(err)
	}
	// Create is idempotent (ON CONFLICT DO NOTHING).
	if err := store.ConversationCreate("c2"); err != nil {
		t.Fatalf("second create should be a no-op, got %v", err)
	}

	if err := store.ConversationSetStatus("c2", "archived"); err != nil {
		t.Fatal(err)
	}
	c, err := store.ConversationGet("c2")
	if err != nil || c == nil {
		t.Fatalf("ConversationGet: c=%v err=%v", c, err)
	}
	if c.Status != "archived" {
		t.Errorf("status = %q, want archived", c.Status)
	}

	// Name save/load; missing name → empty string.
	if n := store.ConversationNameLoad("c2"); n != "" {
		t.Errorf("unset name = %q, want empty", n)
	}
	if err := store.ConversationNameSave("c2", "Repo triage"); err != nil {
		t.Fatal(err)
	}
	if n := store.ConversationNameLoad("c2"); n != "Repo triage" {
		t.Errorf("name = %q, want \"Repo triage\"", n)
	}
}
