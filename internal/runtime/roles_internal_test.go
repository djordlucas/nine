package runtime

import (
	"testing"
	"time"

	"nine/internal/memory"
)

// roleNameForPlan maps session-plan profiles to roles (docs/roles.md §6).
func TestRoleNameForPlan(t *testing.T) {
	planWith := func(kinds ...string) *sessionPlanState {
		stages := make([]memory.SessionRoutine, len(kinds))
		for i, k := range kinds {
			stages[i] = memory.SessionRoutine{Name: k, Kind: k, Status: "active"}
		}
		return &sessionPlanState{plan: &memory.SessionPlan{ID: "x", Routines: stages}}
	}
	// standingPlan is a pursue routine whose config carries an explicit work role
	// (as SpawnStandingSession seeds for a pre-defined agent).
	standingPlan := func(role string) *sessionPlanState {
		plan, err := newStandingPursuePlan("sec-watch", role, false, PursueIdleInterval, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return &sessionPlanState{plan: plan}
	}

	// reflectionPlan is the dedicated self-reflection session exactly as
	// BootstrapSelfReflection seeds it.
	reflectionPlan := func() *sessionPlanState {
		plan, err := newIdleCapablePlan(SelfReflectionAgentID, "idle-reflection", ReflectionRole, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		return &sessionPlanState{plan: plan}
	}

	cases := []struct {
		name string
		plan *sessionPlanState
		want string
	}{
		{"nil plan", nil, OrchestratorRole},
		{"active conversation", planWith("active"), OrchestratorRole},
		// A bare idle-reflection routine no longer implies the reflection role:
		// the role is data now, so a stage that declares none gets the default.
		// The dedicated reflection session declares it (BootstrapSelfReflection),
		// which is what keeps it running as the reflection role while the same
		// kind can ride role-free beside a pursue shell.
		{"bare reflection routine, no declared role", planWith("idle-reflection"), OrchestratorRole},
		{"reflection session as bootstrapped", reflectionPlan(), ReflectionRole},
		{"pursue session", planWith("pursue"), PursueRole},
		{"mixed active+pursue", planWith("active", "pursue"), PursueRole},
		{"standing agent overrides role", standingPlan("monitor"), "monitor"},
		{"standing agent empty role falls back", standingPlan(""), PursueRole},
	}
	for _, tc := range cases {
		if got := roleNameForPlan(tc.plan); got != tc.want {
			t.Errorf("%s: roleNameForPlan = %q, want %q", tc.name, got, tc.want)
		}
	}
}
