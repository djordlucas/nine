package memory_test

import (
	"context"
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

func TestQueuedMessagesEmpty(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	store.ConversationCreate("q1") //nolint:errcheck // test setup

	msgs, err := store.GetQueuedMessages("q1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("new conversation should have 0 queued messages, got %d", len(msgs))
	}

	count, err := store.QueuedMessagesCount("q1")
	if err != nil || count != 0 {
		t.Errorf("count = %d, want 0", count)
	}

	unconsumed, err := store.UnconsumedMessagesCount("q1")
	if err != nil || unconsumed != 0 {
		t.Errorf("unconsumed = %d, want 0", unconsumed)
	}
}

func TestQueueMessage(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	// Queue on a non-existent conversation should create the row.
	if err := store.QueueMessage("q2", "hello"); err != nil {
		t.Fatalf("QueueMessage on non-existent conversation: %v", err)
	}

	msgs, err := store.GetQueuedMessages("q2")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Text != "hello" || msgs[0].Consumed {
		t.Fatalf("unexpected messages: %+v", msgs)
	}

	if err := store.QueueMessage("q2", "world"); err != nil {
		t.Fatal(err)
	}
	msgs, _ = store.GetQueuedMessages("q2")
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	if msgs[1].Text != "world" {
		t.Errorf("second message = %q, want 'world'", msgs[1].Text)
	}

	count, _ := store.QueuedMessagesCount("q2")
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestMarkConsumed(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	store.QueueMessage("q3", "first")
	store.QueueMessage("q3", "second")

	text, err := store.MarkConsumed(context.Background(), "q3", 0)
	if err != nil {
		t.Fatalf("MarkConsumed: %v", err)
	}
	if text != "first" {
		t.Errorf("returned text = %q, want 'first'", text)
	}

	msgs, _ := store.GetQueuedMessages("q3")
	if !msgs[0].Consumed {
		t.Error("message 0 should be consumed")
	}
	if msgs[1].Consumed {
		t.Error("message 1 should not be consumed")
	}

	unconsumed, _ := store.UnconsumedMessagesCount("q3")
	if unconsumed != 1 {
		t.Errorf("unconsumed = %d, want 1", unconsumed)
	}
}

func TestMarkConsumedOutOfRange(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	store.QueueMessage("q4", "only")

	if _, err := store.MarkConsumed(context.Background(), "q4", -1); err == nil {
		t.Error("negative index should error")
	}
	if _, err := store.MarkConsumed(context.Background(), "q4", 5); err == nil {
		t.Error("out-of-range index should error")
	}
}

func TestMarkAllConsumed(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	store.QueueMessage("q5", "a")
	store.QueueMessage("q5", "b")
	store.QueueMessage("q5", "c")

	texts, err := store.MarkAllConsumed(context.Background(), "q5")
	if err != nil {
		t.Fatalf("MarkAllConsumed: %v", err)
	}
	if len(texts) != 3 {
		t.Fatalf("expected 3 consumed texts, got %d", len(texts))
	}
	if texts[0] != "a" || texts[1] != "b" || texts[2] != "c" {
		t.Errorf("texts = %v, want [a b c]", texts)
	}

	unconsumed, _ := store.UnconsumedMessagesCount("q5")
	if unconsumed != 0 {
		t.Errorf("unconsumed = %d, want 0", unconsumed)
	}

	// MarkAllConsumed on already-consumed queue should return nothing.
	texts2, err := store.MarkAllConsumed(context.Background(), "q5")
	if err != nil {
		t.Fatal(err)
	}
	if len(texts2) != 0 {
		t.Errorf("second MarkAllConsumed should return 0, got %d", len(texts2))
	}
}

func TestConversationSavePreservesQueuedMessages(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	// Queue a message.
	store.QueueMessage("q6", "preserved")
	// Simulate a checkpoint save that does NOT include queued_messages
	// (the normal case — loop.SaveState omits it).
	ckpt := []byte(`{"history":[{"Role":"user","Text":"hi"}],"scratchpad":[]}`)
	if err := store.ConversationSave("q6", ckpt); err != nil {
		t.Fatal(err)
	}
	// The queued message should survive.
	msgs, _ := store.GetQueuedMessages("q6")
	if len(msgs) != 1 || msgs[0].Text != "preserved" {
		t.Fatalf("queued messages lost after checkpoint: %+v", msgs)
	}
}

func TestConversationSaveOverwritesQueuedWhenPresent(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	store.QueueMessage("q7", "original")
	// Save a checkpoint that explicitly includes queued_messages.
	ckpt := []byte(`{"history":[],"scratchpad":[],"queued_messages":[{"text":"replacement","consumed":false}]}`)
	if err := store.ConversationSave("q7", ckpt); err != nil {
		t.Fatal(err)
	}
	msgs, _ := store.GetQueuedMessages("q7")
	if len(msgs) != 1 || msgs[0].Text != "replacement" {
		t.Fatalf("queued messages not overwritten: %+v", msgs)
	}
}
