package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
)

func TestRegisterRunAgentAndRunAgents(t *testing.T) {
	d := agent.New()

	agent.RegisterRunAgent(d, func(_ context.Context, _, _, _ string) (string, error) { return "single", nil })
	agent.RegisterRunAgents(d, func(_ context.Context, _ []agent.SubAgentTask, _ int) []agent.SubAgentResult {
		return []agent.SubAgentResult{{Task: "t", Result: "multi"}}
	})

	single, err := d.Dispatch(context.Background(), "run_agent", json.RawMessage(`{"task":"t"}`))
	if err != nil {
		t.Fatalf("run_agent: %v", err)
	}
	if single.Output != "single" {
		t.Errorf("run_agent output = %q, want 'single'", single.Output)
	}

	multi, err := d.Dispatch(context.Background(), "run_agents", json.RawMessage(`{"tasks":[{"task":"t"}]}`))
	if err != nil {
		t.Fatalf("run_agents: %v", err)
	}
	if multi.Output == "" {
		t.Error("run_agents: empty output")
	}
}
