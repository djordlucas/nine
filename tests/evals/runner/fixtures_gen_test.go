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
//
// Only single-session, core-tool cases are recorded here. Cases whose tools spawn
// a background or sub-agent session (goal_create's pursue session, run_agent) share
// the one injected provider, so those extra sessions would consume the scripted
// responses meant for the main turn — non-deterministic to record and outside what
// single-session replay reconstructs. Those stay Track L only.
func TestGenerateSeedFixtures(t *testing.T) {
	if os.Getenv("NINE_EVALS_GENERATE") != "1" {
		t.Skip("fixture generation skipped (set NINE_EVALS_GENERATE=1 to write files)")
	}
	h := requireHarness(t)

	// replay-memory-roundtrip: store a KV value + confirm, then recall it.
	genFixture(t, h, "replay-memory-roundtrip", Setup{}, []string{
		"Store my prod DB host db.prod.example.com under self/prod_db.",
		"What is my prod DB host?",
	}, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("s1", "memory_set", map[string]any{
			"key": "self/prod_db", "value": "db.prod.example.com",
		})}, StopReason: "tool_use"},
		{Text: "Stored: self/prod_db = db.prod.example.com.", StopReason: "end_turn"},
		{ToolCalls: []llm.ToolCall{toolCall("g1", "memory_get", map[string]any{
			"key": "self/prod_db",
		})}, StopReason: "tool_use"},
		{Text: "Your prod DB host is db.prod.example.com.", StopReason: "end_turn"},
	})

	// replay-memory-delete: delete a seeded key, then confirm it is gone.
	genFixture(t, h, "replay-memory-delete", Setup{KV: map[string]string{
		"notes/api_token": "tok-live-abc123",
	}}, []string{
		"Delete the key notes/api_token from memory, then check whether it still exists and tell me.",
	}, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("d1", "memory_delete", map[string]any{
			"key": "notes/api_token",
		})}, StopReason: "tool_use"},
		{ToolCalls: []llm.ToolCall{toolCall("g1", "memory_get", map[string]any{
			"key": "notes/api_token",
		})}, StopReason: "tool_use"},
		{Text: "Deleted notes/api_token; it no longer exists.", StopReason: "end_turn"},
	})

	// replay-skill-write-recall: write a skill, then read it back a turn later.
	genFixture(t, h, "replay-skill-write-recall", Setup{}, []string{
		"Save a skill named deploy-nine documenting our deploy steps: run make build, then nine daemon restart.",
		"What are the deploy steps from your saved deploy-nine skill?",
	}, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("w1", "skill_write", map[string]any{
			"name":        "deploy-nine",
			"description": "How to deploy nine.",
			"content":     "Deploy steps:\n1. make build\n2. nine daemon restart",
		})}, StopReason: "tool_use"},
		{Text: "Saved the deploy-nine skill.", StopReason: "end_turn"},
		{ToolCalls: []llm.ToolCall{toolCall("r1", "skill_read", map[string]any{
			"name": "deploy-nine",
		})}, StopReason: "tool_use"},
		{Text: "Deploy steps: run make build, then nine daemon restart.", StopReason: "end_turn"},
	})

	// replay-workspace-write-search: write a file, then find it by full-text
	// search. This was replay-file-store-search until file_store was retired;
	// the loop/dispatcher/context path it guards is the same one, over the
	// workspace rather than the store (adr/file-namespaces.md).
	genFixture(t, h, "replay-workspace-write-search", Setup{}, []string{
		"Write a file at runbooks/db-restore.md with content 'To restore prod: pg_restore from the nightly snapshot in s3://backups/pg.'",
		"Search your files for the database restore runbook and tell me exactly where the snapshot lives.",
	}, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("f1", "write_file", map[string]any{
			"path":    "runbooks/db-restore.md",
			"content": "To restore prod: pg_restore from the nightly snapshot in s3://backups/pg.",
		})}, StopReason: "tool_use"},
		{Text: "Wrote runbooks/db-restore.md.", StopReason: "end_turn"},
		{ToolCalls: []llm.ToolCall{toolCall("s1", "file_search_text", map[string]any{
			"query": "restore database snapshot",
		})}, StopReason: "tool_use"},
		{Text: "The snapshot lives at s3://backups/pg.", StopReason: "end_turn"},
	})

	// replay-workflow-plan: lay out multi-step work as a named workflow.
	genFixture(t, h, "replay-workflow-plan", Setup{}, []string{
		"Lay out a REST-to-gRPC migration as a named workflow with these ordered steps: audit endpoints, define protobufs, implement services, cut over clients. Just create the plan.",
	}, []llm.Response{
		{ToolCalls: []llm.ToolCall{toolCall("wf1", "workflow_create", map[string]any{
			"name":  "REST to gRPC migration",
			"steps": []string{"audit endpoints", "define protobufs", "implement services", "cut over clients"},
		})}, StopReason: "tool_use"},
		{Text: "Created the REST to gRPC migration workflow with 4 steps.", StopReason: "end_turn"},
	})
}

// genFixture runs one scripted case through the harness, records its journal to
// tests/evals/replay/<id>, and asserts the fixture replays cleanly right after
// recording (so a bad script fails generation, not a later PR).
func genFixture(t *testing.T, h *Harness, id string, setup Setup, prompts []string, responses []llm.Response) {
	t.Helper()
	c := &Case{ID: id, Setup: setup, Prompts: prompts}
	c.defaults()

	res, err := h.Run(context.Background(), c, scriptedProvider(responses))
	if err != nil {
		t.Fatalf("%s: run: %v", id, err)
	}
	defer res.Close()

	dir := ReplayDir("../replay", id)
	if err := RecordFixture(dir, res.Events); err != nil {
		t.Fatalf("%s: record fixture: %v", id, err)
	}
	t.Logf("wrote fixture: %s", dir)

	rr, err := ReplayFixture(context.Background(), dir)
	if err != nil {
		t.Fatalf("%s: verify replay: %v", id, err)
	}
	if !rr.Pass() {
		t.Fatalf("%s: freshly recorded fixture diverges: %s", id, rr.Divergence)
	}
}
