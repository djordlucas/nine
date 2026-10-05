package runtime_test

import (
	"testing"

	"nine/internal/agent"
	"nine/internal/runtime"
)

func nilLoop(string, runtime.RoleParams) *agent.Loop { return nil }

// Without a supervisor or notification store, a session has no hooks: no stall
// detection, no notifications, no completion callback.
func TestInternalAgentWithoutSupervisorHasNoHooks(t *testing.T) {
	s := runtime.InternalAgent{Build: nilLoop}.NewSession("a", runtime.RoleParams{})
	if s.Stall.Limit != 0 || s.Stall.OnStall != nil {
		t.Errorf("stall = %+v, want none", s.Stall)
	}
	if s.Notifications != nil || s.OnComplete != nil {
		t.Error("hooks set without a supervisor or notification store")
	}
}

// A supervisor brings stall detection after five quiet turns and a completion
// callback; an explicit stall config takes precedence over the default.
func TestInternalAgentSupervisorHooks(t *testing.T) {
	sup := runtime.NewSupervisor(8)

	s := runtime.InternalAgent{Build: nilLoop, Supervisor: sup}.NewSession("a", runtime.RoleParams{})
	if s.Stall.Limit != 5 || s.Stall.OnStall == nil {
		t.Errorf("stall = %+v, want limit 5 with a callback", s.Stall)
	}
	if s.OnComplete == nil {
		t.Error("no completion callback with a supervisor")
	}

	own := runtime.StallConfig{Limit: 2, OnStall: func(string) {}}
	s = runtime.InternalAgent{Build: nilLoop, Supervisor: sup, Stall: own}.NewSession("a", runtime.RoleParams{})
	if s.Stall.Limit != 2 {
		t.Errorf("stall limit = %d, want the explicit 2", s.Stall.Limit)
	}
}
