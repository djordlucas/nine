package runner

import (
	"context"
	"testing"

	"nine/internal/llm"
)

// TestReplay_RecordAndReplay records a scripted run's journal to a fixture, then
// re-executes it deterministically and asserts the reproduced answers equal the
// recorded ones — the full Track-R round trip (docs/evals.md §4).
func TestReplay_RecordAndReplay(t *testing.T) {
	h := requireHarness(t)

	c := &Case{
		ID:      "replay-roundtrip",
		Prompts: []string{"store it", "and confirm"},
	}
	c.defaults()

	res, err := h.Run(context.Background(), c, scriptedProvider([]llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("1", "memory_set", map[string]any{
			"key": "k", "value": "v",
		})}, StopReason: "tool_use"},
		{Text: "Stored k=v.", StopReason: "end_turn"},
		{Text: "Confirmed: k is v.", StopReason: "end_turn"},
	}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	dir := ReplayDir(t.TempDir(), c.ID)
	if err := RecordFixture(dir, res.Events); err != nil {
		t.Fatalf("RecordFixture: %v", err)
	}

	rr, err := ReplayFixture(context.Background(), dir)
	if err != nil {
		t.Fatalf("ReplayFixture: %v", err)
	}
	if !rr.Pass() {
		t.Fatalf("replay diverged: %s", rr.Divergence)
	}
	if len(rr.Reproduced) != 2 {
		t.Fatalf("reproduced %d turns, want 2", len(rr.Reproduced))
	}
	if rr.Recorded[1] != "Confirmed: k is v." {
		t.Errorf("recorded[1] = %q", rr.Recorded[1])
	}
}
