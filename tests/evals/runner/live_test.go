package runner

import (
	"context"
	"strings"
	"testing"

	"nine/internal/llm"
)

func TestClassOf(t *testing.T) {
	cases := map[string]ModelClass{
		"gemma4:e2b":              ClassNano,
		"gemma4:e4b":              ClassNano, // table entry beats the 4B tag
		"llama3.2:3b":             ClassNano,
		"qwen3.5:4b":              ClassSmall,
		"qwen3.5:9b":              ClassSmall,
		"gemma4:12b":              ClassMedium,
		"qwen3.5:14b":             ClassMedium,
		"qwen3.5:32b":             ClassLarge,
		"qwen3.5:32b-instruct-q4": ClassLarge,
		"some-random-local-model": ClassSmall,
		"qwen3.5:latest":          ClassSmall,
	}
	for model, want := range cases {
		if got := ClassOf(model); got != want {
			t.Errorf("ClassOf(%q) = %v, want %v", model, got, want)
		}
	}
}

func TestThresholdMet(t *testing.T) {
	cases := []struct {
		threshold     string
		passes, total int
		want          bool
	}{
		{"2/3", 2, 3, true},
		{"2/3", 1, 3, false},
		{"2/3", 4, 6, true}, // scales: ceil(2/3*6)=4
		{"2/3", 3, 6, false},
		{"all", 3, 3, true},
		{"all", 2, 3, false},
		{"any", 1, 5, true},
		{"any", 0, 5, false},
	}
	for _, c := range cases {
		if got := thresholdMet(c.threshold, c.passes, c.total); got != c.want {
			t.Errorf("thresholdMet(%q,%d,%d) = %v, want %v", c.threshold, c.passes, c.total, got, c.want)
		}
	}
}

func TestApplicableModels(t *testing.T) {
	requested := []string{"qwen3.5:4b", "qwen3.5:9b", "gemma4:e4b"}
	c := &Case{Models: Models{Include: []string{"qwen3.5:4b", "gemma4:e4b"}}}
	got := c.ApplicableModels(requested)
	if strings.Join(got, ",") != "qwen3.5:4b,gemma4:e4b" {
		t.Errorf("ApplicableModels = %v", got)
	}
	// No include list → all requested.
	empty := &Case{}
	if len(empty.ApplicableModels(requested)) != 3 {
		t.Errorf("empty include should return all requested")
	}
}

// TestRunCaseModel_FatalClassLogic verifies a below-class miss is tolerated while
// an at-or-above-class miss is fatal, using a scripted provider that always fails
// the assertion (writes the wrong KV value).
func TestRunCaseModel_FatalClassLogic(t *testing.T) {
	h := requireHarness(t)
	eq := "expected"
	mk := func(minClass string) *Case {
		c := &Case{
			ID:      "fatal-logic",
			Prompts: []string{"go"},
			Runs:    1,
			Expect:  Expect{SideEffects: SideEffects{KV: map[string]StringMatch{"self/x": {Equals: &eq}}}},
			Models:  Models{ExpectedPassMinClass: minClass},
		}
		c.defaults()
		return c
	}
	// Always writes the wrong value → every run fails the assertion.
	provider := func() llm.Provider {
		return scriptedProvider([]llm.Response{
			{ToolCalls: []llm.ToolCall{toolCall("1", "memory_set", map[string]any{"key": "self/x", "value": "wrong"})}, StopReason: "tool_use"},
			{Text: "done", StopReason: "end_turn"},
		})
	}

	// nano model, expected_pass_min_class=large → failure is below class → tolerated.
	tol := RunCaseModel(context.Background(), h, mk("large"), "gemma4:e4b", provider(), nil)
	if tol.ThresholdOK {
		t.Fatal("expected threshold miss")
	}
	if tol.Fatal {
		t.Error("nano miss vs large bar should be tolerated, not fatal")
	}

	// large model, expected_pass_min_class=medium → failure at/above class → fatal.
	fatal := RunCaseModel(context.Background(), h, mk("medium"), "qwen3.5:32b", provider(), nil)
	if !fatal.Fatal {
		t.Error("large miss vs medium bar should be fatal")
	}
}

func TestJudge_ScriptedProvider(t *testing.T) {
	// A scripted "judge" that returns a fixed verdict.
	providerFor := func(model string) (llm.Provider, error) {
		return scriptedProvider([]llm.Response{
			{Text: `{"score": 0.9, "reason": "matches the rubric"}`, StopReason: "end_turn"},
		}), nil
	}
	judge := NewJudge(providerFor)
	c := &Case{Expect: Expect{Answer: Answer{Judge: &Judge{Rubric: "correct", Model: "qwen3.5:32b", PassScore: 0.8}}}}

	pass, detail, err := judge(c, "some answer")
	if err != nil {
		t.Fatalf("judge: %v", err)
	}
	if !pass {
		t.Errorf("expected pass, detail=%s", detail)
	}

	// Below pass_score → fail.
	providerLow := func(model string) (llm.Provider, error) {
		return scriptedProvider([]llm.Response{
			{Text: `here is my verdict: {"score": 0.5, "reason": "partially"}`, StopReason: "end_turn"},
		}), nil
	}
	if pass, _, _ := NewJudge(providerLow)(c, "x"); pass {
		t.Error("expected fail below pass_score")
	}
}

func TestRenderGrid(t *testing.T) {
	r := &Report{
		Models: []string{"qwen3.5:4b", "qwen3.5:9b"},
		Replay: []ReplayCaseResult{{CaseID: "replay-case", Pass: true}},
		Live: []CaseModelResult{
			{CaseID: "kv-case", Model: "qwen3.5:4b", Runs: make([]RunOutcome, 3), Passes: 3, ThresholdOK: true},
			{CaseID: "kv-case", Model: "qwen3.5:9b", Runs: make([]RunOutcome, 3), Passes: 1, ThresholdOK: false, Fatal: false},
		},
	}
	grid := r.RenderGrid()
	if !strings.Contains(grid, "kv-case") || !strings.Contains(grid, "qwen3.5:4b") {
		t.Errorf("grid missing rows/cols:\n%s", grid)
	}
	if !strings.Contains(grid, "3/3") || !strings.Contains(grid, "1/3 ~") {
		t.Errorf("grid missing pass fractions:\n%s", grid)
	}
}
