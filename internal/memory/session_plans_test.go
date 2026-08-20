package memory_test

import (
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func openTestStore(t *testing.T) *memory.Store {
	t.Helper()
	s, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestSessionPlanGetMissing(t *testing.T) {
	s := openTestStore(t)

	p, err := s.SessionPlanGet("no-such-id")
	if err != nil {
		t.Fatalf("SessionPlanGet: %v", err)
	}
	if p != nil {
		t.Errorf("SessionPlanGet = %+v, want nil", p)
	}
}

func TestSessionPlanSaveAndGet(t *testing.T) {
	s := openTestStore(t)

	plan := &memory.SessionPlan{
		ID:     "agent-1",
		Status: "active",
		Aspects: []memory.SessionAspect{
			{Name: "active", Kind: "active", Status: "active", UpdatedAt: "2026-06-11T00:00:00Z"},
		},
	}
	if err := s.SessionPlanSave(plan); err != nil {
		t.Fatalf("SessionPlanSave: %v", err)
	}

	got, err := s.SessionPlanGet("agent-1")
	if err != nil {
		t.Fatalf("SessionPlanGet: %v", err)
	}
	if got == nil {
		t.Fatal("SessionPlanGet = nil, want a plan")
	}
	if got.ID != "agent-1" || got.Status != "active" {
		t.Errorf("got = %+v", got)
	}
	if len(got.Aspects) != 1 || got.Aspects[0].Kind != "active" {
		t.Errorf("got.Aspects = %+v", got.Aspects)
	}
}

func TestSessionPlanSaveUpdatesExisting(t *testing.T) {
	s := openTestStore(t)

	plan := &memory.SessionPlan{
		ID:      "agent-1",
		Status:  "active",
		Aspects: []memory.SessionAspect{{Name: "active", Kind: "active", Status: "active"}},
	}
	if err := s.SessionPlanSave(plan); err != nil {
		t.Fatalf("SessionPlanSave (insert): %v", err)
	}

	plan.Status = "paused"
	plan.Aspects[0].Status = "paused"
	if err := s.SessionPlanSave(plan); err != nil {
		t.Fatalf("SessionPlanSave (update): %v", err)
	}

	got, err := s.SessionPlanGet("agent-1")
	if err != nil {
		t.Fatalf("SessionPlanGet: %v", err)
	}
	if got.Status != "paused" || got.Aspects[0].Status != "paused" {
		t.Errorf("got = %+v, want status/stage paused", got)
	}
}

func TestSessionPlanListActive(t *testing.T) {
	s := openTestStore(t)

	active := &memory.SessionPlan{ID: "active-1", Status: "active", Aspects: []memory.SessionAspect{{Name: "active", Kind: "active", Status: "active"}}}
	paused := &memory.SessionPlan{ID: "paused-1", Status: "paused", Aspects: []memory.SessionAspect{{Name: "active", Kind: "active", Status: "active"}}}
	if err := s.SessionPlanSave(active); err != nil {
		t.Fatal(err)
	}
	if err := s.SessionPlanSave(paused); err != nil {
		t.Fatal(err)
	}

	plans, err := s.SessionPlanListActive()
	if err != nil {
		t.Fatalf("SessionPlanListActive: %v", err)
	}
	if len(plans) != 1 || plans[0].ID != "active-1" {
		t.Errorf("plans = %+v, want only active-1", plans)
	}
}
