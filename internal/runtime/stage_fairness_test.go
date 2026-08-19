package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
)

func cfgJSON(t *testing.T, c stageConfig) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// stageNextWake clamps at 0, which is why fairness needs its own reading: two
// stages overdue by wildly different amounts are indistinguishable through it.
func TestStageOverdueByDistinguishesLateStages(t *testing.T) {
	now := time.Now()
	short := cfgJSON(t, stageConfig{IdleIntervalSeconds: 60})
	long := cfgJSON(t, stageConfig{IdleIntervalSeconds: 3600})

	// Both last fired 2h ago: the 60s stage is ~119m late, the 3600s one ~60m.
	lastFire := now.Add(-2 * time.Hour)

	shortRemaining, _ := stageNextWake(short, lastFire, now)
	longRemaining, _ := stageNextWake(long, lastFire, now)
	if shortRemaining != 0 || longRemaining != 0 {
		t.Fatalf("stageNextWake = %v/%v, want both clamped to 0", shortRemaining, longRemaining)
	}

	shortOverdue, ok1 := stageOverdueBy(short, lastFire, now)
	longOverdue, ok2 := stageOverdueBy(long, lastFire, now)
	if !ok1 || !ok2 {
		t.Fatal("both stages are past due; stageOverdueBy should report both as due")
	}
	if shortOverdue <= longOverdue {
		t.Errorf("overdue: short=%v long=%v, want the 60s stage to be further past due", shortOverdue, longOverdue)
	}
}

// A stage that is not yet due is not due, and reports no overdueness.
func TestStageOverdueByRejectsFutureStages(t *testing.T) {
	now := time.Now()
	cfg := cfgJSON(t, stageConfig{IdleIntervalSeconds: 300})
	if overdue, due := stageOverdueBy(cfg, now.Add(-10*time.Second), now); due {
		t.Errorf("due = true (overdue %v) for a stage with 290s left", overdue)
	}
}

// A stage due exactly now is due, with zero overdueness.
func TestStageOverdueByAtExactlyDue(t *testing.T) {
	now := time.Now()
	cfg := cfgJSON(t, stageConfig{IdleIntervalSeconds: 300})
	overdue, due := stageOverdueBy(cfg, now.Add(-300*time.Second), now)
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
		cfgJSON(t, stageConfig{}),
		cfgJSON(t, stageConfig{Schedule: "not a cron"}),
	} {
		if _, due := stageOverdueBy(cfg, now.Add(-24*time.Hour), now); due {
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
	err := validateStages("a1", []memory.SessionStage{
		{Name: "pursue", Kind: "pursue", Status: "active"},
		{Name: "reflect", Kind: "idle-reflection", Status: "active",
			Config: cfgJSON(t, stageConfig{Role: "reflection"})},
	})
	if err == nil {
		t.Fatal("validateStages accepted two role-bearing stages")
	}
	for _, want := range []string{"role-bearing", "pursue", "reflect"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to mention %q", err, want)
		}
	}
}

// A pursue session owns its goal 1:1 (agentID == goalID), so two pursue stages
// describe something the identity relation cannot express.
func TestValidateStagesRejectsTwoGoalOwningStages(t *testing.T) {
	err := validateStages("a1", []memory.SessionStage{
		{Name: "pursue-a", Kind: "pursue", Status: "active"},
		{Name: "pursue-b", Kind: "pursue", Status: "active"},
	})
	if err == nil {
		t.Fatal("validateStages accepted two goal-owning stages")
	}
	if !strings.Contains(err.Error(), "goal-owning") {
		t.Errorf("error = %v, want it to name the goal-ownership conflict", err)
	}
}

// The shapes that must keep working: today's single-stage plans, and the
// two-stage profile C3 is about to introduce.
func TestValidateStagesAcceptsValidPlans(t *testing.T) {
	cases := []struct {
		name   string
		stages []memory.SessionStage
	}{
		{"empty", nil},
		{"single active stage", []memory.SessionStage{{Name: "active", Kind: "active"}}},
		{"single pursue", []memory.SessionStage{{Name: "pursue", Kind: "pursue"}}},
		{"single reflection", []memory.SessionStage{{Name: "reflect", Kind: "idle-reflection"}}},
		{"pursue plus a role-free companion", []memory.SessionStage{
			{Name: "pursue", Kind: "pursue"},
			{Name: "report", Kind: "report"},
		}},
		// The shape C3 exists to make possible: a pursue shell with a reflection
		// aspect riding alongside, the aspect declaring no role of its own.
		{"pursue plus a role-free reflection aspect", []memory.SessionStage{
			{Name: "pursue", Kind: "pursue"},
			{Name: "idle-reflection", Kind: "idle-reflection"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateStages("a1", tc.stages); err != nil {
				t.Errorf("validateStages rejected a valid plan: %v", err)
			}
		})
	}
}
