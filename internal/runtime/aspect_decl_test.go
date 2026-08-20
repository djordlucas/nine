package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// registerTestStage adds a aspect kind for the duration of a test.
//
// AspectRegistry is populated by whoever assembles the daemon — "active" here,
// "pursue" by Assemble, "idle-reflection" by cmd/nine — so a package-level test
// sees only the first. Registering explicitly keeps these tests independent of
// boot order rather than quietly depending on it.
func registerTestStage(t *testing.T, kind string) {
	t.Helper()
	prev, had := AspectRegistry[kind]
	AspectRegistry[kind] = func() AspectHandler { return noopStage{} }
	t.Cleanup(func() {
		if had {
			AspectRegistry[kind] = prev
		} else {
			delete(AspectRegistry, kind)
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
		[]AspectDecl{{Kind: "idle-reflection", Interval: time.Hour}})
	if err != nil {
		t.Fatalf("newStandingPursuePlan: %v", err)
	}
	if len(plan.Aspects) != 2 {
		t.Fatalf("stages = %d, want 2 (pursue + aspect)", len(plan.Aspects))
	}
	if plan.Aspects[0].Kind != "pursue" {
		t.Errorf("stage[0] = %q, want the pursue shell first", plan.Aspects[0].Kind)
	}
	if plan.Aspects[1].Kind != "idle-reflection" {
		t.Errorf("stage[1] = %q, want the declared aspect", plan.Aspects[1].Kind)
	}

	// Each stage keeps its own cadence — that is the whole point of an aspect.
	var pursueCfg, aspectCfg aspectConfig
	if err := json.Unmarshal(plan.Aspects[0].Config, &pursueCfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plan.Aspects[1].Config, &aspectCfg); err != nil {
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
		t.Errorf("aspect role = %q, want empty — the pursue shell is the role-bearing aspect", aspectCfg.Role)
	}
}

// With no aspects declared, the plan is exactly what it was before.
func TestStandingPlanWithoutAspectsIsUnchanged(t *testing.T) {
	plan, err := newStandingPursuePlan("g1", "monitor", true, 5*time.Minute, "", nil)
	if err != nil {
		t.Fatalf("newStandingPursuePlan: %v", err)
	}
	if len(plan.Aspects) != 1 || plan.Aspects[0].Kind != "pursue" {
		t.Fatalf("stages = %+v, want a single pursue aspect", plan.Aspects)
	}
}

// Both stages are schedulable and wake independently, which is what makes the
// multi-aspect capability real rather than merely representable.
func TestAspectStagesScheduleIndependently(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	plan, err := newStandingPursuePlan("g1", "monitor", false, time.Minute, "",
		[]AspectDecl{{Kind: "idle-reflection", Interval: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lastFire := now.Add(-90 * time.Minute) // both overdue

	var due []string
	for _, st := range plan.Aspects {
		if _, ok := aspectOverdueBy(st.Config, lastFire, now); ok {
			due = append(due, st.Kind)
		}
	}
	if len(due) != 2 {
		t.Fatalf("due stages = %v, want both", due)
	}
	// The 1m stage is further past due than the 1h one, so fairness picks it.
	pursueOverdue, _ := aspectOverdueBy(plan.Aspects[0].Config, lastFire, now)
	aspectOverdue, _ := aspectOverdueBy(plan.Aspects[1].Config, lastFire, now)
	if pursueOverdue <= aspectOverdue {
		t.Errorf("overdue pursue=%v aspect=%v, want the 1m stage further past due", pursueOverdue, aspectOverdue)
	}
}

func TestValidateAspectsRejectsBadDeclarations(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	cases := []struct {
		name    string
		aspects []AspectDecl
		want    string
	}{
		{"no kind", []AspectDecl{{Interval: time.Hour}}, "no kind"},
		{"unknown kind", []AspectDecl{{Kind: "nonsense", Interval: time.Hour}}, "not a registered aspect kind"},
		{"duplicate pursue", []AspectDecl{{Kind: "pursue", Interval: time.Hour}}, "duplicates the session's own pursue shell"},
		{"no cadence", []AspectDecl{{Kind: "idle-reflection"}}, "would never wake"},
		{"both cadences", []AspectDecl{{Kind: "idle-reflection", Interval: time.Hour, Schedule: "0 9 * * *"}}, "mutually exclusive"},
		{"declared twice", []AspectDecl{
			{Kind: "idle-reflection", Interval: time.Hour},
			{Kind: "idle-reflection", Interval: time.Minute},
		}, "declared twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateAspectDecls(tc.aspects)
			if err == nil {
				t.Fatalf("ValidateAspectDecls accepted %+v", tc.aspects)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateAspectsAcceptsGoodDeclarations(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	for _, a := range [][]AspectDecl{
		nil,
		{{Kind: "idle-reflection", Interval: time.Hour}},
		{{Kind: "idle-reflection", Schedule: "0 9 * * 1-5"}},
	} {
		if err := ValidateAspectDecls(a); err != nil {
			t.Errorf("ValidateAspectDecls rejected %+v: %v", a, err)
		}
	}
}

// A plan whose aspects would violate the one-role rule fails where the operator
// can see it, not at the next load.
func TestStandingPlanRejectsRoleBearingAspect(t *testing.T) {
	_, err := newStandingPursuePlan("g1", "monitor", false, time.Minute, "",
		[]AspectDecl{{Kind: "pursue", Interval: time.Hour}})
	if err == nil {
		t.Fatal("newStandingPursuePlan accepted a second goal-owning aspect")
	}
}
