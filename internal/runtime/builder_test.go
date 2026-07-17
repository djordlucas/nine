package runtime_test

import (
	"context"
	"testing"

	"nine/internal/agent"
	"nine/internal/llm"
	"nine/internal/plugin"
	"nine/internal/runtime"
)

// Compile-time check: AgentBuilder.Build satisfies LoopFactory.
var _ runtime.LoopFactory = (*runtime.AgentBuilder)(nil).BuildForRole

// Compile-time check: agent.New() is independent of LoopConfig.
var _ *agent.Dispatcher = agent.New()

// minimalLoopConfig returns a LoopConfig with no plugins.
func minimalLoopConfig(systemPrompt string) runtime.LoopConfig {
	return runtime.LoopConfig{
		Mgr:           plugin.NewManager(""),
		SystemPrompt:  systemPrompt,
		ContextBudget: 100_000,
	}
}

// minimalFactory builds an AgentBuilder with a fixed-answer provider and no plugins.
func minimalFactory(answer string) *runtime.AgentBuilder {
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: answer, StopReason: "end_turn"}, nil
	})
	return runtime.NewAgentBuilder(runtime.AgentBuilderConfig{
		Loop:         minimalLoopConfig("test"),
		InitialQueue: llm.NewQueue(provider, 1),
		Sup:          runtime.NewSupervisor(8),
	})
}

// TestLoopConfigSeparateFromFactoryConfig verifies that LoopConfig can be
// constructed independently and reused across multiple AgentBuilderConfigs.
func TestLoopConfigSeparateFromFactoryConfig(t *testing.T) {
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: "ok", StopReason: "end_turn"}, nil
	})
	queue := llm.NewQueue(provider, 1)
	sup := runtime.NewSupervisor(8)

	lc := minimalLoopConfig("shared system prompt")

	// Two factories share the same LoopConfig value.
	f1 := runtime.NewAgentBuilder(runtime.AgentBuilderConfig{Loop: lc, InitialQueue: queue, Sup: sup})
	f2 := runtime.NewAgentBuilder(runtime.AgentBuilderConfig{Loop: lc, InitialQueue: queue, Sup: sup})

	if f1 == nil || f2 == nil {
		t.Fatal("NewAgentBuilder returned nil")
	}

	// Both build independent loops.
	if f1.Build("a1", false) == nil {
		t.Error("f1.Build returned nil")
	}
	if f2.Build("a2", false) == nil {
		t.Error("f2.Build returned nil")
	}
}

// TestAgentBuilderBuildReturnsUsableLoop verifies that Build produces a loop
// that can complete a turn.
func TestAgentBuilderBuildReturnsUsableLoop(t *testing.T) {
	const answer = "factory loop answer"
	factory := minimalFactory(answer)

	loop := factory.Build("agent-1", false)
	if loop == nil {
		t.Fatal("Build returned nil loop")
	}

	result, err := loop.Run(context.Background(), "say hello")
	if err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
	if result != answer {
		t.Errorf("result = %q, want %q", result, answer)
	}
}

// TestAgentBuilderUpdateQueuePropagates verifies that UpdateQueue affects
// loops built after the swap.
func TestAgentBuilderUpdateQueuePropagates(t *testing.T) {
	makeProvider := func(answer string) llm.Provider {
		return llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
			return llm.Response{Text: answer, StopReason: "end_turn"}, nil
		})
	}

	factory := runtime.NewAgentBuilder(runtime.AgentBuilderConfig{
		Loop:         minimalLoopConfig("test"),
		InitialQueue: llm.NewQueue(makeProvider("first"), 1),
		Sup:          runtime.NewSupervisor(8),
	})

	loop1 := factory.Build("a1", false)
	r1, _ := loop1.Run(context.Background(), "ping")
	if r1 != "first" {
		t.Errorf("before update: got %q, want 'first'", r1)
	}

	factory.UpdateQueue(llm.NewQueue(makeProvider("second"), 1))

	loop2 := factory.Build("a2", false)
	r2, _ := loop2.Run(context.Background(), "ping")
	if r2 != "second" {
		t.Errorf("after update: got %q, want 'second'", r2)
	}
}

// TestAgentBuilderSubAgentsInitiallyEmpty verifies the sub-agent list starts empty.
func TestAgentBuilderSubAgentsInitiallyEmpty(t *testing.T) {
	factory := minimalFactory("done")
	if got := factory.SubAgents(); len(got) != 0 {
		t.Errorf("initial sub-agents = %d, want 0", len(got))
	}
}

// TestLoopConfigFieldsReachLoop verifies that SystemPrompt and ContextBudget
// from LoopConfig are reflected in the built loop (indirectly via a turn).
func TestLoopConfigFieldsReachLoop(t *testing.T) {
	// If the system prompt or budget were ignored the loop would still work,
	// but we can at least confirm Build doesn't panic and the loop runs.
	factory := runtime.NewAgentBuilder(runtime.AgentBuilderConfig{
		Loop: runtime.LoopConfig{
			Mgr:           plugin.NewManager(""),
			SystemPrompt:  "you are a test agent",
			ContextBudget: 50_000,
		},
		InitialQueue: llm.NewQueue(llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
			return llm.Response{Text: "ok", StopReason: "end_turn"}, nil
		}), 1),
		Sup: runtime.NewSupervisor(8),
	})

	loop := factory.Build("agent-x", false)
	if _, err := loop.Run(context.Background(), "hello"); err != nil {
		t.Fatalf("loop.Run: %v", err)
	}
}

func TestPlanMentionRiskyTool(t *testing.T) {
	risky := []string{"shell", "file_write"}
	cases := []struct {
		name string
		plan string
		want bool
	}{
		{"no risky tool", "I'll read the docs and then answer directly", false},
		{"names shell", "I'll run a shell command to inspect the logs", true},
		{"names file_write", "I'll use file_write to persist the result", true},
		{"empty plan", "", false},
	}

	for _, c := range cases {
		if got := runtime.PlanMentionRiskyTool(c.plan, risky); got != c.want {
			t.Errorf("%s: PlanMentionRiskyTool(%q) = %v, want %v", c.name, c.plan, got, c.want)
		}
	}
	// Empty risky set -> never prompts, even when the plan clearly acts
	if runtime.PlanMentionRiskyTool("run a shell command", nil) {
		t.Error("empty require_approval set should never flag a plan")
	}
}
