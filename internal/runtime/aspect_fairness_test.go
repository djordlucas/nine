package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
)

func cfgJSON(t *testing.T, c aspectConfig) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// aspectNextWake clamps at 0, which is why fairness needs its own reading: two
// stages overdue by wildly different amounts are indistinguishable through it.
func TestStageOverdueByDistinguishesLateStages(t *testing.T) {
	now := time.Now()
	short := cfgJSON(t, aspectConfig{IdleIntervalSeconds: 60})
	long := cfgJSON(t, aspectConfig{IdleIntervalSeconds: 3600})

	// Both last fired 2h ago: the 60s stage is ~119m late, the 3600s one ~60m.
	lastFire := now.Add(-2 * time.Hour)

	shortRemaining, _ := aspectNextWake(short, lastFire, now)
	longRemaining, _ := aspectNextWake(long, lastFire, now)
	if shortRemaining != 0 || longRemaining != 0 {
		t.Fatalf("aspectNextWake = %v/%v, want both clamped to 0", shortRemaining, longRemaining)
	}

	shortOverdue, ok1 := aspectOverdueBy(short, lastFire, now)
	longOverdue, ok2 := aspectOverdueBy(long, lastFire, now)
	if !ok1 || !ok2 {
		t.Fatal("both stages are past due; aspectOverdueBy should report both as due")
	}
	if shortOverdue <= longOverdue {
		t.Errorf("overdue: short=%v long=%v, want the 60s stage to be further past due", shortOverdue, longOverdue)
	}
}

// A stage that is not yet due is not due, and reports no overdueness.
func TestStageOverdueByRejectsFutureStages(t *testing.T) {
	now := time.Now()
	cfg := cfgJSON(t, aspectConfig{IdleIntervalSeconds: 300})
	if overdue, due := aspectOverdueBy(cfg, now.Add(-10*time.Second), now); due {
		t.Errorf("due = true (overdue %v) for a stage with 290s left", overdue)
	}
}

// A stage due exactly now is due, with zero overdueness.
func TestStageOverdueByAtExactlyDue(t *testing.T) {
	now := time.Now()
	cfg := cfgJSON(t, aspectConfig{IdleIntervalSeconds: 300})
	overdue, due := aspectOverdueBy(cfg, now.Add(-300*time.Second), now)
	if !due {
		t.Fatal("a stage due exactly now should be due")
	}
	if overdue < 0 || overdue > time.Second {
		t.Errorf("overdue = %v, want ~0", overdue)
	}
}

// A non-schedulable stage is never due, however long it has been idle.
func TestStageOverdueByIgnoresUnscheduledStages(t *testing.T) {
	now := time.Now()
	for _, cfg := range []json.RawMessage{
		nil,
		cfgJSON(t, aspectConfig{}),
		cfgJSON(t, aspectConfig{Schedule: "not a cron"}),
	} {
		if _, due := aspectOverdueBy(cfg, now.Add(-24*time.Hour), now); due {
			t.Errorf("cfg %s reported due; an unscheduled stage never is", cfg)
		}
	}
}

// C2: at most one stage may decide the session's role. Two would make the role —
// and so the tool boundary — depend on JSON array order.
func TestValidateStagesRejectsTwoRoleBearingStages(t *testing.T) {
	// One claims the role by kind (pursue), the other by config. Two different
	// routes to the same claim, which is exactly the case that must be caught —
	// a kind-only check would miss it.
	err := validateAspects("a1", []memory.SessionAspect{
		{Name: "pursue", Kind: "pursue", Status: "active"},
		{Name: "reflect", Kind: "idle-reflection", Status: "active",
			Config: cfgJSON(t, aspectConfig{Role: "reflection"})},
	})
	if err == nil {
		t.Fatal("validateAspects accepted two role-bearing aspects")
	}
	for _, want := range []string{"role-bearing", "pursue", "reflect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// A pursue session owns its goal 1:1 (agentID == goalID), so two pursue aspects
// describe something the identity relation cannot express.
func TestValidateStagesRejectsTwoGoalOwningStages(t *testing.T) {
	err := validateAspects("a1", []memory.SessionAspect{
		{Name: "pursue-a", Kind: "pursue", Status: "active"},
		{Name: "pursue-b", Kind: "pursue", Status: "active"},
	})
	if err == nil {
		t.Fatal("validateAspects accepted two goal-owning aspects")
	}
	if !strings.Contains(err.Error(), "goal-owning") {
		t.Errorf("error = %v, want it to name the goal-ownership conflict", err)
	}
}

// The shapes that must keep working: today's single-aspect plans, and the
// two-aspect profile C3 is about to introduce.
func TestValidateStagesAcceptsValidPlans(t *testing.T) {
	cases := []struct {
		name   string
		stages []memory.SessionAspect
	}{
		{"empty", nil},
		{"single active stage", []memory.SessionAspect{{Name: "active", Kind: "active"}}},
		{"single pursue", []memory.SessionAspect{{Name: "pursue", Kind: "pursue"}}},
		{"single reflection", []memory.SessionAspect{{Name: "reflect", Kind: "idle-reflection"}}},
		{"pursue plus a role-free companion", []memory.SessionAspect{
			{Name: "pursue", Kind: "pursue"},
			{Name: "report", Kind: "report"},
		}},
		// The shape C3 exists to make possible: a pursue shell with a reflection
		// aspect riding alongside, the aspect declaring no role of its own.
		{"pursue plus a role-free reflection aspect", []memory.SessionAspect{
			{Name: "pursue", Kind: "pursue"},
			{Name: "idle-reflection", Kind: "idle-reflection"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateAspects("a1", tc.stages); err != nil {
				t.Errorf("validateAspects rejected a valid plan: %v", err)
			}
		})
	}
}
