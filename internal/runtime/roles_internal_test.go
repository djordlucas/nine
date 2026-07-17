package runtime

import (
	"testing"

	"nine/internal/memory"
)

// roleNameForPlan maps session-plan profiles to roles (docs/roles.md §6).
func TestRoleNameForPlan(t *testing.T) {
	planWith := func(kinds ...string) *sessionPlanState {
		stages := make([]memory.SessionStage, len(kinds))
		for i, k := range kinds {
			stages[i] = memory.SessionStage{Name: k, Kind: k, Status: "active"}
		}
		return &sessionPlanState{plan: &memory.SessionPlan{ID: "x", Stages: stages}}
	}
	// standingPlan is a pursue stage whose config carries an explicit work role
	// (as SpawnStandingSession seeds for a pre-defined agent).
	standingPlan := func(role string) *sessionPlanState {
		plan, err := newStandingPursuePlan("sec-watch", role, false, PursueIdleInterval, "")
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
		{"reflection session", planWith("idle-reflection"), ReflectionRole},
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
