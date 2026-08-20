package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// registerTestStage adds a routine kind for the duration of a test.
//
// RoutineRegistry is populated by whoever assembles the daemon — "active" here,
// "pursue" by Assemble, "idle-reflection" by cmd/nine — so a package-level test
// sees only the first. Registering explicitly keeps these tests independent of
// boot order rather than quietly depending on it.
func registerTestStage(t *testing.T, kind string) {
	t.Helper()
	prev, had := RoutineRegistry[kind]
	RoutineRegistry[kind] = func() RoutineHandler { return noopStage{} }
	t.Cleanup(func() {
		if had {
			RoutineRegistry[kind] = prev
		} else {
			delete(RoutineRegistry, kind)
		}
	})
}

type noopStage struct{}

func (noopStage) Init(context.Context, string, json.RawMessage) error    { return nil }
func (noopStage) OnIdle(context.Context, string) (string, bool)          { return "", false }
func (noopStage) OnTurnEnd(context.Context, string, string, error) error { return nil }

// C3: the scheduler has always handled several stages; this is the first caller
// that can ask for more than one.
func TestStandingPlanCarriesRoutinesBesideThePursueShell(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	plan, err := newStandingPursuePlan("g1", "monitor", false, 5*time.Minute, "",
		[]RoutineDecl{{Kind: "idle-reflection", Interval: time.Hour}})
	if err != nil {
		t.Fatalf("newStandingPursuePlan: %v", err)
	}
	if len(plan.Routines) != 2 {
		t.Fatalf("stages = %d, want 2 (pursue + routine)", len(plan.Routines))
	}
	if plan.Routines[0].Kind != "pursue" {
		t.Errorf("stage[0] = %q, want the pursue shell first", plan.Routines[0].Kind)
	}
	if plan.Routines[1].Kind != "idle-reflection" {
		t.Errorf("stage[1] = %q, want the declared routine", plan.Routines[1].Kind)
	}

	// Each stage keeps its own cadence — that is the whole point of a routine.
	var pursueCfg, routineCfg routineConfig
	if err := json.Unmarshal(plan.Routines[0].Config, &pursueCfg); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plan.Routines[1].Config, &routineCfg); err != nil {
		t.Fatal(err)
	}
	if pursueCfg.IdleIntervalSeconds != 300 {
		t.Errorf("pursue interval = %ds, want 300", pursueCfg.IdleIntervalSeconds)
	}
	if routineCfg.IdleIntervalSeconds != 3600 {
		t.Errorf("routine interval = %ds, want 3600", routineCfg.IdleIntervalSeconds)
	}
	// The pursue shell owns the role; a routine must never claim one (C2).
	if pursueCfg.Role != "monitor" {
		t.Errorf("pursue role = %q, want monitor", pursueCfg.Role)
	}
	if routineCfg.Role != "" {
		t.Errorf("routine role = %q, want empty — the pursue shell is the role-bearing routine", routineCfg.Role)
	}
}

// With no routines declared, the plan is exactly what it was before.
func TestStandingPlanWithoutRoutinesIsUnchanged(t *testing.T) {
	plan, err := newStandingPursuePlan("g1", "monitor", true, 5*time.Minute, "", nil)
	if err != nil {
		t.Fatalf("newStandingPursuePlan: %v", err)
	}
	if len(plan.Routines) != 1 || plan.Routines[0].Kind != "pursue" {
		t.Fatalf("stages = %+v, want a single pursue routine", plan.Routines)
	}
}

// Both stages are schedulable and wake independently, which is what makes the
// multi-routine capability real rather than merely representable.
func TestRoutineStagesScheduleIndependently(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	plan, err := newStandingPursuePlan("g1", "monitor", false, time.Minute, "",
		[]RoutineDecl{{Kind: "idle-reflection", Interval: time.Hour}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lastFire := now.Add(-90 * time.Minute) // both overdue

	var due []string
	for _, st := range plan.Routines {
		if _, ok := routineOverdueBy(st.Config, lastFire, now); ok {
			due = append(due, st.Kind)
		}
	}
	if len(due) != 2 {
		t.Fatalf("due stages = %v, want both", due)
	}
	// The 1m stage is further past due than the 1h one, so fairness picks it.
	pursueOverdue, _ := routineOverdueBy(plan.Routines[0].Config, lastFire, now)
	routineOverdue, _ := routineOverdueBy(plan.Routines[1].Config, lastFire, now)
	if pursueOverdue <= routineOverdue {
		t.Errorf("overdue pursue=%v routine=%v, want the 1m stage further past due", pursueOverdue, routineOverdue)
	}
}

func TestValidateRoutinesRejectsBadDeclarations(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	cases := []struct {
		name     string
		routines []RoutineDecl
		want     string
	}{
		{"no kind", []RoutineDecl{{Interval: time.Hour}}, "no kind"},
		{"unknown kind", []RoutineDecl{{Kind: "nonsense", Interval: time.Hour}}, "not a registered routine kind"},
		{"duplicate pursue", []RoutineDecl{{Kind: "pursue", Interval: time.Hour}}, "duplicates the session's own pursue shell"},
		{"no cadence", []RoutineDecl{{Kind: "idle-reflection"}}, "would never wake"},
		{"both cadences", []RoutineDecl{{Kind: "idle-reflection", Interval: time.Hour, Schedule: "0 9 * * *"}}, "mutually exclusive"},
		{"declared twice", []RoutineDecl{
			{Kind: "idle-reflection", Interval: time.Hour},
			{Kind: "idle-reflection", Interval: time.Minute},
		}, "declared twice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRoutineDecls(tc.routines)
			if err == nil {
				t.Fatalf("ValidateRoutineDecls accepted %+v", tc.routines)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestValidateRoutinesAcceptsGoodDeclarations(t *testing.T) {
	registerTestStage(t, "idle-reflection")
	for _, a := range [][]RoutineDecl{
		nil,
		{{Kind: "idle-reflection", Interval: time.Hour}},
		{{Kind: "idle-reflection", Schedule: "0 9 * * 1-5"}},
	} {
		if err := ValidateRoutineDecls(a); err != nil {
			t.Errorf("ValidateRoutineDecls rejected %+v: %v", a, err)
		}
	}
}

// A plan whose routines would violate the one-role rule fails where the operator
// can see it, not at the next load.
func TestStandingPlanRejectsRoleBearingRoutine(t *testing.T) {
	_, err := newStandingPursuePlan("g1", "monitor", false, time.Minute, "",
		[]RoutineDecl{{Kind: "pursue", Interval: time.Hour}})
	if err == nil {
		t.Fatal("newStandingPursuePlan accepted a second goal-owning routine")
	}
}
