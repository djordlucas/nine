package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"nine/internal/llm"
)

// scriptedProvider returns the given responses in order, then a terminal
// end_turn. It ignores the request entirely, standing in for a live model.
func scriptedProvider(responses []llm.Response) llm.Provider {
	var i atomic.Int32
	return llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		idx := int(i.Add(1)) - 1
		if idx >= len(responses) {
			return llm.Response{Text: "done", StopReason: "end_turn"}, nil
		}
		return responses[idx], nil
	})
}

func toolCall(id, name string, args map[string]any) llm.ToolCall {
	b, _ := json.Marshal(args)
	return llm.ToolCall{ID: id, Name: name, Input: b}
}

// requireHarness builds a plugin-less harness (core tools only) or skips when the
// eval database is unreachable.
func requireHarness(t *testing.T) *Harness {
	t.Helper()
	_, cleanup, err := openIsolatedStore("")
	if errors.Is(err, ErrNoPostgres) {
		t.Skipf("eval postgres unavailable: %v", err)
	}
	if err != nil {
		t.Fatalf("openIsolatedStore: %v", err)
	}
	cleanup()
	return &Harness{}
}

// TestHarness_MemoryRoundtrip drives a two-response script that stores a KV value
// then answers, and asserts the side-effect landed in the isolated store and the
// journal recorded the tool call — the end-to-end harness contract.
func TestHarness_MemoryRoundtrip(t *testing.T) {
	h := requireHarness(t)

	c := &Case{
		ID:      "harness-memory-roundtrip",
		Prompts: []string{"remember it", "recall it"},
	}
	c.defaults()

	provider := scriptedProvider([]llm.Response{
		// Turn 1: store, then confirm.
		{ToolCalls: []llm.ToolCall{toolCall("t1", "memory_set", map[string]any{
			"key": "self/prod_db", "value": "db.prod.example.com",
		})}, StopReason: "tool_use"},
		{Text: "Stored the prod DB host.", StopReason: "end_turn"},
		// Turn 2: recall.
		{Text: "Your prod DB host is db.prod.example.com.", StopReason: "end_turn"},
	})

	res, err := h.Run(context.Background(), c, provider)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	if len(res.Answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(res.Answers))
	}

	// Side-effect: the KV write landed in the isolated store.
	got, ok, err := res.Store.Get("self/prod_db")
	if err != nil || !ok {
		t.Fatalf("Get self/prod_db: ok=%v err=%v", ok, err)
	}
	if got != "db.prod.example.com" {
		t.Errorf("kv = %q, want db.prod.example.com", got)
	}

	// Journal: a memory_set tool_start was recorded.
	var sawToolStart bool
	for _, e := range res.Events {
		if e.Type == "tool_start" {
			var p struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if p.Name == "memory_set" {
				sawToolStart = true
			}
		}
	}
	if !sawToolStart {
		t.Error("journal missing memory_set tool_start")
	}
}

// TestHarness_SetupFixtures verifies pre-seeded KV and workspace files are in
// place before the run and readable by the run.
func TestHarness_SetupFixtures(t *testing.T) {
	h := requireHarness(t)

	c := &Case{
		ID:      "harness-setup",
		Prompts: []string{"noop"},
		Setup: Setup{
			KV:    map[string]string{"self/region": "us-west-2"},
			Files: map[string]string{"/work/data.txt": "alpha\nbeta\n"},
		},
	}
	c.defaults()

	res, err := h.Run(context.Background(), c, scriptedProvider(nil))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer res.Close()

	got, ok, _ := res.Store.Get("self/region")
	if !ok || got != "us-west-2" {
		t.Errorf("seeded kv = %q ok=%v", got, ok)
	}
	abs, _ := workspacePath(res.Workspace, "/work/data.txt")
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	if string(b) != "alpha\nbeta\n" {
		t.Errorf("seeded file = %q", string(b))
	}
}
