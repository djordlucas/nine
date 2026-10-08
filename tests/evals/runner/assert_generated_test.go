package runner

import (
	"context"
	"strings"
	"testing"

	"nine/internal/llm"
)

// generatedOn is the session config every generated-tool case needs: without
// tools.agent.enabled there is no store to seed and no tool_write or tool_delete.
func generatedOn() Session {
	return Session{Config: map[string]any{"tools.agent.enabled": true}}
}

func slugifySeed() map[string]GeneratedToolSetup {
	return map[string]GeneratedToolSetup{"slugify": {
		Description: "Turn a title into a URL slug",
		InputSchema: `{"type":"object","properties":{"text":{"type":"string"}}}`,
		Source:      `export default ({ text }) => text.split(" ").join("-");`,
	}}
}

func TestGrade_GeneratedTools_SeededToolIsChecked(t *testing.T) {
	c := &Case{
		ID:      "grade-generated-seeded",
		Prompts: []string{"noop"},
		Session: generatedOn(),
		Setup:   Setup{GeneratedTools: slugifySeed()},
		Expect: Expect{SideEffects: SideEffects{GeneratedTools: map[string]GeneratedToolExpect{
			"slugify": {Source: StringMatch{Contains: `split(" ")`}},
		}}},
	}
	if g := runCase(t, c, answered()); !g.Pass {
		t.Errorf("expected pass, got failures: %v", g.Failures)
	}
}

// Each way a generated_tools check fails says which: a source that does not
// match, a tool that should be gone, and a tool that was never written.
func TestGrade_GeneratedTools_Failures(t *testing.T) {
	c := &Case{
		ID:      "grade-generated-failures",
		Prompts: []string{"noop"},
		Session: generatedOn(),
		Setup:   Setup{GeneratedTools: slugifySeed()},
		Expect: Expect{SideEffects: SideEffects{GeneratedTools: map[string]GeneratedToolExpect{
			"slugify":    {Source: StringMatch{Matches: "toLowerCase"}},
			"never_made": {},
		}}},
	}
	g := runCase(t, c, answered())
	if g.Pass {
		t.Fatal("expected failures")
	}
	got := strings.Join(g.Failures, "\n")
	for _, want := range []string{"slugify source: does not match", "never_made: not found"} {
		if !strings.Contains(got, want) {
			t.Errorf("failures lack %q:\n%s", want, got)
		}
	}

	c.ID = "grade-generated-absent-but-present"
	c.Expect.SideEffects.GeneratedTools = map[string]GeneratedToolExpect{"slugify": {Absent: true}}
	if g := runCase(t, c, answered()); g.Pass || !strings.Contains(strings.Join(g.Failures, "\n"), "still exists") {
		t.Errorf("a seeded tool expected absent must fail with still exists, got pass=%v %v", g.Pass, g.Failures)
	}
}

// absent passes once the model deletes the tool through tool_delete itself, so
// the check reads the same store the tool writes.
func TestGrade_GeneratedTools_DeletedByTheModel(t *testing.T) {
	c := &Case{
		ID:      "grade-generated-deleted",
		Prompts: []string{"delete slugify"},
		Session: generatedOn(),
		Setup:   Setup{GeneratedTools: slugifySeed()},
		Expect: Expect{SideEffects: SideEffects{GeneratedTools: map[string]GeneratedToolExpect{
			"slugify": {Absent: true},
		}}},
	}
	g := runCase(t, c, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("t1", "tool_delete", map[string]any{"name": "slugify"})}, StopReason: "tool_use"},
		{Text: "Deleted slugify.", StopReason: "end_turn"},
	})
	if !g.Pass {
		t.Errorf("expected pass, got failures: %v", g.Failures)
	}
}

func TestHarness_GeneratedToolsNeedTheAgentTier(t *testing.T) {
	c := &Case{
		ID:      "harness-generated-disabled",
		Prompts: []string{"noop"},
		Setup:   Setup{GeneratedTools: slugifySeed()},
	}
	c.defaults()
	_, err := requireHarness(t).Run(context.Background(), c, scriptedProvider(answered()))
	if err == nil || !strings.Contains(err.Error(), "tools.agent.enabled") {
		t.Fatalf("Run err = %v, want one naming tools.agent.enabled", err)
	}
}

func TestEvalNumCtx(t *testing.T) {
	t.Setenv("NINE_EVAL_NUM_CTX", "")
	if got := EvalNumCtx(); got != DefaultEvalNumCtx {
		t.Errorf("unset: %d, want %d", got, DefaultEvalNumCtx)
	}
	t.Setenv("NINE_EVAL_NUM_CTX", "32768")
	if got := EvalNumCtx(); got != 32768 {
		t.Errorf("32768: got %d", got)
	}
	if got := EvalContextBudget(); got != 32768-2048 {
		t.Errorf("budget: got %d, want the window less the 2048 reply cap", got)
	}
	t.Setenv("NINE_EVAL_NUM_CTX", "lots")
	if got := EvalNumCtx(); got != DefaultEvalNumCtx {
		t.Errorf("unparsable: %d, want the default", got)
	}
}
