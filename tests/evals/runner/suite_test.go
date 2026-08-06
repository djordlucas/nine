package runner

import (
	"context"
	"strings"
	"testing"

	"nine/internal/llm"
)

// TestSuite_RunBothTracks exercises the full orchestration: a live case graded
// against two scripted "models" plus a replay case with a freshly recorded
// fixture, then checks the aggregate report and grid. Uses scripted providers, so
// it needs no external service.
func TestSuite_RunBothTracks(t *testing.T) {
	h := requireHarness(t)

	// A live case: store a KV value and confirm.
	eq := "db.prod.example.com"
	live := &Case{
		ID:      "suite-kv",
		Track:   TrackLive,
		Prompts: []string{"store it"},
		Runs:    2,
		Expect:  Expect{SideEffects: SideEffects{KV: map[string]StringMatch{"self/prod_db": {Equals: &eq}}}},
	}
	live.defaults()

	// A replay case, whose fixture we record into a temp replay root first.
	replayCase := &Case{
		ID:      "suite-replay",
		Track:   TrackReplay,
		Prompts: []string{"go"},
		Expect:  Expect{Answer: Answer{Contains: []string{"ok"}}},
	}
	replayCase.defaults()

	replayRoot := t.TempDir()
	rec, err := h.Run(context.Background(), replayCase, scriptedProvider([]llm.Response{
		{Text: "ok done", StopReason: "end_turn"},
	}))
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
	if err := RecordFixture(ReplayDir(replayRoot, replayCase.ID), rec.Events); err != nil {
		t.Fatalf("record fixture: %v", err)
	}
	rec.Close()

	// A provider factory whose provider is stateless — the Suite builds one
	// provider per model and reuses it across all runs (as real HTTP providers
	// are), so it must decide from the request history, not a positional script:
	// call memory_set until a tool result is present, then answer.
	providerFor := func(model string) (llm.Provider, error) {
		return llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
			for _, m := range req.Messages {
				if len(m.ToolResults) > 0 {
					return llm.Response{Text: "stored", StopReason: "end_turn"}, nil
				}
			}
			return llm.Response{ToolCalls: []llm.ToolCall{toolCall("1", "memory_set", map[string]any{
				"key": "self/prod_db", "value": "db.prod.example.com",
			})}, StopReason: "tool_use"}, nil
		}), nil
	}

	suite := &Suite{
		Harness:     h,
		Models:      []string{"claude-haiku", "qwen3.5:9b"},
		ProviderFor: providerFor,
		ReplayRoot:  replayRoot,
	}
	report := suite.Run(context.Background(), []*Case{live, replayCase})

	if report.Fatal() {
		t.Errorf("expected non-fatal report; live=%+v replay=%+v", report.Live, report.Replay)
	}
	if len(report.Live) != 2 {
		t.Fatalf("expected 2 live results (case × 2 models), got %d", len(report.Live))
	}
	for _, lc := range report.Live {
		if !lc.ThresholdOK {
			t.Errorf("model %s: expected threshold met, failures %+v", lc.Model, lc.Runs)
		}
	}
	if len(report.Replay) != 1 || !report.Replay[0].Pass {
		t.Errorf("expected replay pass, got %+v", report.Replay)
	}

	grid := report.RenderGrid()
	if !strings.Contains(grid, "suite-kv") || !strings.Contains(grid, "suite-replay") {
		t.Errorf("grid missing cases:\n%s", grid)
	}
	t.Logf("\n%s", grid)
}
