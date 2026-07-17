package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

func TestNotificationsPendingAndDelivered(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.NotificationCreate("n1", "conv1", "task done", false); err != nil {
		t.Fatal(err)
	}
	if err := store.NotificationCreate("n2", "conv1", "needs approval", true); err != nil {
		t.Fatal(err)
	}
	if err := store.NotificationCreate("n3", "conv2", "other convo", false); err != nil {
		t.Fatal(err)
	}

	// ListPending is scoped to the conversation and returns only undelivered.
	pending, err := store.NotificationListPending("conv1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending for conv1 = %d, want 2", len(pending))
	}
	for _, n := range pending {
		if n.ConversationID != "conv1" {
			t.Errorf("leaked notification from %q", n.ConversationID)
		}
	}

	// Marking one delivered drops it from the pending set.
	if err := store.NotificationMarkDelivered("n1"); err != nil {
		t.Fatal(err)
	}
	pending, _ = store.NotificationListPending("conv1")
	if len(pending) != 1 || pending[0].ID != "n2" {
		t.Errorf("after delivery pending = %+v, want only n2", pending)
	}
	if !pending[0].RequiresApproval {
		t.Error("n2 should carry requires_approval=true")
	}
}
