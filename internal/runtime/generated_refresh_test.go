package runtime_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"nine/internal/config"
	"nine/internal/llm"
	"nine/internal/runtime"
)

// A session's loop outlives a change to the sandboxed catalog, so a tool written
// with tool_write must reach that same session's next turn — advertised and
// dispatchable — and a deleted one must leave it (docs/sandboxed-tools.md §9.1).
// Before the per-turn refresh the loop kept the catalog it was built with, so the
// writing session could never call its own tool.
func TestWrittenToolReachesTheWritingSessionNextTurn(t *testing.T) {
	store := newRoleTestStore(t)
	cfg := &config.Config{}
	cfg.Tools.Enabled = true
	cfg.Tools.Agent.Enabled = true
	host := runtime.OpenSandboxedTools(context.Background(), cfg, store, nil)
	if host == nil {
		t.Fatal("sandboxed host did not open")
	}
	t.Cleanup(func() { _ = host.Close(context.Background()) })
	gen := runtime.NewGeneratedToolStore(store, host, nil, nil, false)
	if gen == nil {
		t.Fatal("generated tier is off despite [tools.agent] enabled")
	}

	writeDouble, _ := json.Marshal(map[string]any{
		"name":         "double",
		"description":  "Double a number.",
		"input_schema": map[string]any{"type": "object"},
		"source":       `export default ({n}) => ({ out: n*2 });`,
	})
	var observation string
	p := &scriptedProvider{}
	p.script = func(n int, req llm.Request) llm.Response {
		switch n {
		case 1: // turn 1: write the tool
			return llm.Response{StopReason: "tool_use",
				ToolCalls: []llm.ToolCall{{ID: "w", Name: "tool_write", Input: writeDouble}}}
		case 3: // turn 2: call it
			return llm.Response{StopReason: "tool_use",
				ToolCalls: []llm.ToolCall{{ID: "c", Name: "double", Input: json.RawMessage(`{"n":21}`)}}}
		case 4:
			for _, m := range req.Messages {
				for _, r := range m.ToolResults {
					observation += r.Content
				}
			}
		}
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	factory := rolesTestBuilder(t, p, func(c *runtime.AgentBuilderConfig) {
		c.Loop.Memory = store
		c.Loop.Tools = host
		c.Loop.GeneratedTools = gen
	})
	loop := factory.Build("root-1", false)

	if _, err := loop.Run(context.Background(), "write a tool"); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if toolNames(p.call(2))["double"] {
		t.Error("the tool was advertised in the turn that wrote it; an in-flight turn must keep its tool set")
	}

	if _, err := loop.Run(context.Background(), "use it"); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if !toolNames(p.call(3))["double"] {
		t.Fatal("the written tool is not advertised on the writing session's next turn")
	}
	if !strings.Contains(observation, `"out":42`) {
		t.Errorf("calling the written tool: observation = %q, want its result", observation)
	}

	if err := gen.Delete(context.Background(), "double"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	p.script = func(int, llm.Request) llm.Response {
		return llm.Response{Text: "done", StopReason: "end_turn"}
	}
	if _, err := loop.Run(context.Background(), "anything"); err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if toolNames(p.call(p.nCalls()))["double"] {
		t.Error("a deleted tool is still advertised on the next turn")
	}
}
