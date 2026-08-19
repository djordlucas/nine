package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// registerTestStage adds a stage kind for the duration of a test.
//
// StageRegistry is populated by whoever assembles the daemon — "active" here,
// "pursue" by Assemble, "idle-reflection" by cmd/nine — so a package-level test
// sees only the first. Registering explicitly keeps these tests independent of
// boot order rather than quietly depending on it.
func registerTestStage(t *testing.T, kind string) {
	t.Helper()
	prev, had := StageRegistry[kind]
	StageRegistry[kind] = func() StageHandler { return noopStage{} }
	t.Cleanup(func() {
		if had {
			StageRegistry[kind] = prev
		} else {
			delete(StageRegistry, kind)
		}
	})
}

type noopStage struct{}

func (noopStage) Init(context.Context, string, json.RawMessage) error    { return nil }
func (noopStage) OnIdle(context.Context, string) (string, bool)          { return "", false }
func (noopStage) OnTurnEnd(context.Context, string, string, error) error { return nil }

// C3: the scheduler has always handled several stages; this is the first caller
// that can ask for more than one.
func TestStandingPlanCarriesAspectsBesideThePursueShell(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	plan, err := newStandingPursuePlan("g1", "monitor", false, 5*time.Minute, "",
		[]StageAspect{{Kind: "idle-reflection", Interval: time.Hour}})
	if err != nil {
		t.Fatalf("newStandingPursuePlan: %v", err)
	}
	if len(plan.Stages) != 2 {
		t.Fatalf("stages = %d, want 2 (pursue + aspect)", len(plan.Stages))
	}
	if plan.Stages[0].Kind != "pursue" {
		t.Errorf("stage[0] = %q, want the pursue shell first", plan.Stages[0].Kind)
	}
	if plan.Stages[1].Kind != "idle-reflection" {
		t.Errorf("stage[1] = %q, want the declared aspect", plan.Stages[1].Kind)
	}

	// Each stage keeps its own cadence — that is the whole point of an aspect.
	var pursueCfg, aspectCfg stageConfig
	if err := json.Unmarshal(plan.Stages[0].Config, &pursueCfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plan.Stages[1].Config, &aspectCfg); err != nil {
		t.Fatal(err)
	}
	if pursueCfg.IdleIntervalSeconds != 300 {
		t.Errorf("pursue interval = %ds, want 300", pursueCfg.IdleIntervalSeconds)
	}
	if aspectCfg.IdleIntervalSeconds != 3600 {
		t.Errorf("aspect interval = %ds, want 3600", aspectCfg.IdleIntervalSeconds)
	}
	// The pursue shell owns the role; an aspect must never claim one (C2).
	if pursueCfg.Role != "monitor" {
		t.Errorf("pursue role = %q, want monitor", pursueCfg.Role)
	}
	if aspectCfg.Role != "" {
		t.Errorf("aspect role = %q, want empty — the pursue shell is the role-bearing stage", aspectCfg.Role)
	}
}

// With no aspects declared, the plan is exactly what it was before.
func TestStandingPlanWithoutAspectsIsUnchanged(t *testing.T) {
	plan, err := newStandingPursuePlan("g1", "monitor", true, 5*time.Minute, "", nil)
	if err != nil {
		t.Fatalf("newStandingPursuePlan: %v", err)
	}
	if len(plan.Stages) != 1 || plan.Stages[0].Kind != "pursue" {
		t.Fatalf("stages = %+v, want a single pursue stage", plan.Stages)
	}
}

// Both stages are schedulable and wake independently, which is what makes the
// multi-stage capability real rather than merely representable.
func TestAspectStagesScheduleIndependently(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	plan, err := newStandingPursuePlan("g1", "monitor", false, time.Minute, "",
		[]StageAspect{{Kind: "idle-reflection", Interval: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lastFire := now.Add(-90 * time.Minute) // both overdue

	var due []string
	for _, st := range plan.Stages {
		if _, ok := stageOverdueBy(st.Config, lastFire, now); ok {
			due = append(due, st.Kind)
		}
	}
	if len(due) != 2 {
		t.Fatalf("due stages = %v, want both", due)
	}
	// The 1m stage is further past due than the 1h one, so fairness picks it.
	pursueOverdue, _ := stageOverdueBy(plan.Stages[0].Config, lastFire, now)
	aspectOverdue, _ := stageOverdueBy(plan.Stages[1].Config, lastFire, now)
	if pursueOverdue <= aspectOverdue {
		t.Errorf("overdue pursue=%v aspect=%v, want the 1m stage further past due", pursueOverdue, aspectOverdue)
	}
}

func TestValidateAspectsRejectsBadDeclarations(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	cases := []struct {
		name    string
		aspects []StageAspect
		want    string
	}{
		{"no kind", []StageAspect{{Interval: time.Hour}}, "no kind"},
		{"unknown kind", []StageAspect{{Kind: "nonsense", Interval: time.Hour}}, "not a registered stage kind"},
		{"duplicate pursue", []StageAspect{{Kind: "pursue", Interval: time.Hour}}, "duplicates the session's own pursue shell"},
		{"no cadence", []StageAspect{{Kind: "idle-reflection"}}, "would never wake"},
		{"both cadences", []StageAspect{{Kind: "idle-reflection", Interval: time.Hour, Schedule: "0 9 * * *"}}, "mutually exclusive"},
		{"declared twice", []StageAspect{
			{Kind: "idle-reflection", Interval: time.Hour},
			{Kind: "idle-reflection", Interval: time.Minute},
		}, "declared twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAspects(tc.aspects)
			if err == nil {
				t.Fatalf("ValidateAspects accepted %+v", tc.aspects)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateAspectsAcceptsGoodDeclarations(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	for _, a := range [][]StageAspect{
		nil,
		{{Kind: "idle-reflection", Interval: time.Hour}},
		{{Kind: "idle-reflection", Schedule: "0 9 * * 1-5"}},
	} {
		if err := ValidateAspects(a); err != nil {
			t.Errorf("ValidateAspects rejected %+v: %v", a, err)
		}
	}
}

// A plan whose aspects would violate the one-role rule fails where the operator
// can see it, not at the next load.
func TestStandingPlanRejectsRoleBearingAspect(t *testing.T) {
	_, err := newStandingPursuePlan("g1", "monitor", false, time.Minute, "",
		[]StageAspect{{Kind: "pursue", Interval: time.Hour}})
	if err == nil {
		t.Fatal("newStandingPursuePlan accepted a second goal-owning stage")
	}
}
