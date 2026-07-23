package runner

import (
	"context"
	"os"
	"testing"

	"nine/internal/llm"
)

// TestGenerateSeedFixtures records the deterministic Track-R fixtures shipped in
// tests/evals/replay from scripted provider runs, so the every-PR replay gate has
// real fixtures without needing a live model. Opt-in — it writes files:
//
//	NINE_EVALS_GENERATE=1 go test ./tests/evals/runner -run TestGenerateSeedFixtures
//
// Re-run it deliberately when a seed fixture's recorded behavior should change
// (docs/evals.md §4). The scripted responses mirror what a competent model does
// for each case's prompts.
func TestGenerateSeedFixtures(t *testing.T) {
	if os.Getenv("NINE_EVALS_GENERATE") != "1" {
		t.Skip("fixture generation skipped (set NINE_EVALS_GENERATE=1 to write files)")
	}
	h := requireHarness(t)

	// replay-memory-roundtrip: store a KV value + confirm, then recall it.
	c := &Case{
		ID: "replay-memory-roundtrip",
		Prompts: []string{
			"Store my prod DB host db.prod.example.com under self/prod_db.",
			"What is my prod DB host?",
		},
	}
	c.defaults()
	res, err := h.Run(context.Background(), c, scriptedProvider([]llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("s1", "memory_set", map[string]any{
			"key": "self/prod_db", "value": "db.prod.example.com",
		})}, StopReason: "tool_use"},
		{Text: "Stored: self/prod_db = db.prod.example.com.", StopReason: "end_turn"},
		{ToolCalls: []llm.ToolCall{toolCall("g1", "memory_get", map[string]any{
			"key": "self/prod_db",
		})}, StopReason: "tool_use"},
		{Text: "Your prod DB host is db.prod.example.com.", StopReason: "end_turn"},
	}))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	defer res.Close()

	dir := ReplayDir("../replay", c.ID)
	if err := RecordFixture(dir, res.Events); err != nil {
		t.Fatalf("record fixture: %v", err)
	}
	t.Logf("wrote fixture: %s", dir)

	// Sanity: it must replay cleanly right after recording.
	rr, err := ReplayFixture(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify replay: %v", err)
	}
	if !rr.Pass() {
		t.Fatalf("freshly recorded fixture diverges: %s", rr.Divergence)
	}
}
