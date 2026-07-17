package main

import (
	"testing"

	"nine/internal/config"
	"nine/internal/memory"
)

// TestRemovedConfigGoals is the safety core of subtractive reconciliation:
// only config-origin goals absent from the config are selected for teardown,
// and conversation-created goals are never selected.
func TestRemovedConfigGoals(t *testing.T) {
	goals := []memory.Goal{
		{ID: "sec-watch", ParentType: "config", Status: "active"},       // still declared → keep
		{ID: "dep-monitor", ParentType: "config", Status: "active"},     // removed → tear down
		{ID: "old-watch", ParentType: "config", Status: "paused"},       // removed → tear down
		{ID: "user-goal", ParentType: "conversation", Status: "active"}, // human's → never touch
		{ID: "orphan", ParentType: "", Status: "active"},                // not config-origin → never touch
	}
	desired := desiredAgentIDs([]config.AgentConfig{
		{ID: "sec-watch"},
		{ID: ""}, // malformed entries contribute no id
	})

	removed := removedConfigGoals(goals, desired)

	got := make(map[string]bool)
	for _, g := range removed {
		got[g.ID] = true
	}
	if !got["dep-monitor"] || !got["old-watch"] {
		t.Errorf("removed = %v, want dep-monitor and old-watch", got)
	}
	if got["sec-watch"] {
		t.Error("still-declared config goal must not be torn down")
	}
	if got["user-goal"] || got["orphan"] {
		t.Error("non-config-origin goals must never be selected for teardown")
	}
	if len(removed) != 2 {
		t.Errorf("removed %d goals, want exactly 2", len(removed))
	}
}
