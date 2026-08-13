package runner

import (
	"context"
	"testing"

	"nine/internal/llm"
)

// runCase is a small helper: run a case with the given scripted responses and
// grade it, returning the verdict.
func runCase(t *testing.T, c *Case, responses []llm.Response) GradeResult {
	t.Helper()
	c.defaults()
	h := requireHarness(t)
	res, err := h.Run(context.Background(), c, scriptedProvider(responses))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()
	return Grade(c, res, nil)
}

// TestGrade_SideEffectKV_Pass asserts a KV write is graded as a pass.
func TestGrade_SideEffectKV_Pass(t *testing.T) {
	eq := "db.prod.example.com"
	c := &Case{
		ID:      "grade-kv-pass",
		Prompts: []string{"store it"},
		Expect: Expect{SideEffects: SideEffects{
			KV: map[string]StringMatch{"self/prod_db": {Equals: &eq}},
		}},
	}
	g := runCase(t, c, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "memory_set", map[string]any{
			"key": "self/prod_db", "value": "db.prod.example.com",
		})}, StopReason: "tool_use"},
		{Text: "done", StopReason: "end_turn"},
	})
	if !g.Pass {
		t.Errorf("expected pass, got failures: %v", g.Failures)
	}
}

// TestGrade_SideEffectKV_Fail asserts a wrong value is graded as a failure.
func TestGrade_SideEffectKV_Fail(t *testing.T) {
	eq := "expected-host"
	c := &Case{
		ID:      "grade-kv-fail",
		Prompts: []string{"store it"},
		Expect: Expect{SideEffects: SideEffects{
			KV: map[string]StringMatch{"self/prod_db": {Equals: &eq}},
		}},
	}
	g := runCase(t, c, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "memory_set", map[string]any{
			"key": "self/prod_db", "value": "wrong-host",
		})}, StopReason: "tool_use"},
		{Text: "done", StopReason: "end_turn"},
	})
	if g.Pass {
		t.Error("expected failure for mismatched kv")
	}
}

// TestGrade_Trajectory asserts tool-set and turn-count checks against the journal.
func TestGrade_Trajectory(t *testing.T) {
	max := 1
	c := &Case{
		ID:      "grade-trajectory",
		Prompts: []string{"do it"},
		Expect: Expect{Trajectory: Trajectory{
			ToolsAllOf:  []string{"memory_set"},
			ToolsNoneOf: []string{"memory_delete"},
			MaxTurns:    &max,
			NoStall:     true,
		}},
	}
	g := runCase(t, c, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "memory_set", map[string]any{
			"key": "k", "value": "v",
		})}, StopReason: "tool_use"},
		{Text: "done", StopReason: "end_turn"},
	})
	if !g.Pass {
		t.Errorf("expected pass, got: %v", g.Failures)
	}
}

// TestGrade_TrajectoryNoneOf_Fail asserts a forbidden tool trips tools_none_of.
func TestGrade_TrajectoryNoneOf_Fail(t *testing.T) {
	c := &Case{
		ID:      "grade-noneof-fail",
		Prompts: []string{"do it"},
		Expect: Expect{Trajectory: Trajectory{
			ToolsNoneOf: []string{"memory_set"},
		}},
	}
	g := runCase(t, c, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "memory_set", map[string]any{
			"key": "k", "value": "v",
		})}, StopReason: "tool_use"},
		{Text: "done", StopReason: "end_turn"},
	})
	if g.Pass {
		t.Error("expected failure: forbidden tool was called")
	}
}

// TestGrade_Answer asserts contains/not_contains over the final answer.
func TestGrade_Answer(t *testing.T) {
	c := &Case{
		ID:      "grade-answer",
		Prompts: []string{"tell me"},
		Expect: Expect{Answer: Answer{
			Contains:    []string{"DB.PROD"}, // case-insensitive
			NotContains: []string{"error"},
			Matches:     `db\.prod\.example\.com`,
		}},
	}
	g := runCase(t, c, []llm.Response{
		{Text: "Your prod DB host is db.prod.example.com.", StopReason: "end_turn"},
	})
	if !g.Pass {
		t.Errorf("expected pass, got: %v", g.Failures)
	}
}

// TestGrade_JudgeWithoutFn fails a case that declares a judge but wires none.
func TestGrade_JudgeWithoutFn(t *testing.T) {
	c := &Case{
		ID:      "grade-judge-missing",
		Prompts: []string{"tell me"},
		Expect: Expect{Answer: Answer{
			Judge: &Judge{Rubric: "correct", Model: "qwen3.5:32b", PassScore: 0.8},
		}},
	}
	g := runCase(t, c, []llm.Response{{Text: "anything", StopReason: "end_turn"}})
	if g.Pass {
		t.Error("expected failure when judge declared but not configured")
	}
}
