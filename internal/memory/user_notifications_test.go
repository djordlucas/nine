package memory_test

import (
	"testing"

	"nine/internal/memory/memtest"
)

func TestUserNotifications(t *testing.T) {
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close() //nolint:errcheck

	if err := s.UserNotificationCreate("n1", "sec-watch", "CVE-2026-1 affects our deps"); err != nil {
		t.Fatal(err)
	}
	if err := s.UserNotificationCreate("n2", "dep-monitor", "3 dependencies are behind"); err != nil {
		t.Fatal(err)
	}

	// Both are unseen initially.
	if got, _ := s.UserNotificationCountUnseen(); got != 2 {
		t.Fatalf("CountUnseen = %d, want 2", got)
	}
	unseen, err := s.UserNotificationList(true)
	if err != nil {
		t.Fatal(err)
	}
	if len(unseen) != 2 {
		t.Fatalf("List(unseenOnly) = %d entries, want 2", len(unseen))
	}
	// Oldest-first ordering and attribution.
	if unseen[0].ID != "n1" || unseen[0].AgentID != "sec-watch" {
		t.Errorf("first entry = %+v, want n1/sec-watch", unseen[0])
	}

	// Mark one seen; it drops out of the unseen view but stays in the full list.
	if err := s.UserNotificationMarkSeen("n1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UserNotificationCountUnseen(); got != 1 {
		t.Errorf("CountUnseen after mark = %d, want 1", got)
	}
	all, _ := s.UserNotificationList(false)
	if len(all) != 2 {
		t.Errorf("List(all) = %d, want 2", len(all))
	}

	// Duplicate id is ignored, not an error.
	if err := s.UserNotificationCreate("n1", "x", "dup"); err != nil {
		t.Errorf("duplicate create returned error: %v", err)
	}
	if got, _ := s.UserNotificationCountUnseen(); got != 1 {
		t.Errorf("CountUnseen after dup = %d, want 1 (dup ignored)", got)
	}
}
