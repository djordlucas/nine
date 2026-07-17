package memory_test

import (
	"testing"
	"time"
)

func TestHumanRequestCreateAndGetPending(t *testing.T) {
	s := openTestStore(t)

	exp := time.Now().Add(time.Minute)
	if err := s.HumanRequestCreate("req-1", "agent-1", "Proceed?", []string{"yes", "no"}, exp); err != nil {
		t.Fatalf("HumanRequestCreate: %v", err)
	}

	got, err := s.HumanRequestGetPending("agent-1")
	if err != nil {
		t.Fatalf("HumanRequestGetPending: %v", err)
	}
	if got == nil {
		t.Fatal("HumanRequestGetPending = nil, want row")
	}
	if got.ID != "req-1" || got.Question != "Proceed?" || got.Status != "pending" {
		t.Errorf("got %+v", got)
	}
	if len(got.Options) != 2 || got.Options[0] != "yes" {
		t.Errorf("options = %v", got.Options)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("ExpiresAt not parsed")
	}
}

func TestHumanRequestGetPendingNone(t *testing.T) {
	s := openTestStore(t)
	got, err := s.HumanRequestGetPending("nobody")
	if err != nil {
		t.Fatalf("HumanRequestGetPending: %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestHumanRequestAnswer(t *testing.T) {
	s := openTestStore(t)
	_ = s.HumanRequestCreate("req-1", "agent-1", "Q?", nil, time.Now().Add(time.Minute))

	if err := s.HumanRequestAnswer("req-1", "go ahead"); err != nil {
		t.Fatalf("HumanRequestAnswer: %v", err)
	}
	// Answered rows are no longer pending.
	got, _ := s.HumanRequestGetPending("agent-1")
	if got != nil {
		t.Errorf("answered row still pending: %+v", got)
	}
}

func TestHumanRequestExpireStale(t *testing.T) {
	s := openTestStore(t)
	_ = s.HumanRequestCreate("old", "agent-1", "Q?", nil, time.Now().Add(-time.Minute))
	_ = s.HumanRequestCreate("fresh", "agent-2", "Q?", nil, time.Now().Add(time.Minute))

	if err := s.HumanRequestExpireStale(time.Now()); err != nil {
		t.Fatalf("HumanRequestExpireStale: %v", err)
	}
	if got, _ := s.HumanRequestGetPending("agent-1"); got != nil {
		t.Errorf("stale row still pending: %+v", got)
	}
	if got, _ := s.HumanRequestGetPending("agent-2"); got == nil {
		t.Error("fresh row was expired")
	}
}

func TestInteractiveSession(t *testing.T) {
	s := openTestStore(t)

	if ok, _ := s.InteractiveSessionExists("a"); ok {
		t.Error("unexpected interactive session")
	}
	if err := s.InteractiveSessionAdd("a"); err != nil {
		t.Fatalf("InteractiveSessionAdd: %v", err)
	}
	if err := s.InteractiveSessionAdd("a"); err != nil {
		t.Fatalf("InteractiveSessionAdd (idempotent): %v", err)
	}
	if ok, _ := s.InteractiveSessionExists("a"); !ok {
		t.Error("interactive session not recorded")
	}
}
