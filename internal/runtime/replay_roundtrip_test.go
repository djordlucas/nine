package runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/memory/memtest"
	"nine/internal/replay"
	"nine/internal/runtime"
)

// TestRecordThenReplay is the v3 gate (adr/event-log.md §11): a recorded real
// session replays deterministically with no live LLM or tool calls. It records
// a genuine worker turn to Postgres via the sink, then rebuilds the session from
// the durable journal and re-executes it through the replay harness, asserting
// the answer is reproduced.
func TestRecordThenReplay(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// --- record: a real turn (tool call, then answer) captured to the journal ---
	liveCalls := 0
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		liveCalls++
		if liveCalls == 1 {
			return llm.Response{
				Text:       "let me check",
				ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "echo", Input: json.RawMessage(`{"v":"hi"}`)}},
				StopReason: "tool_use",
			}, nil
		}
		return llm.Response{Text: "the answer is hi", StopReason: "end_turn"}, nil
	})

	builder := ninectx.New(ninectx.Config{Budget: 100_000})
	queue := llm.NewQueue(provider, 1)
	dispatcher := agent.New()
	toolRuns := 0
	dispatcher.InjectHandler("echo", func(_ context.Context, args json.RawMessage) (string, error) {
		toolRuns++
		return "echoed:" + string(args), nil
	})
	loop := agent.NewLoop(agent.Config{
		SystemCore: "test-core",
		Priority:   llm.PriorityConversation,
		Tools:      []ninectx.ToolWithVector{{Tool: llm.ToolDef{Name: "echo", Description: "echoes"}}},
	}, builder, queue, dispatcher)

	sink := runtime.NewSQLEventSinkForTest(store)
	w := runtime.NewAgentWorkerWithSinkForTest("agent-rr", loop, sink)
	original, err := w.TurnAgentWorker(context.Background(), "what is it?")
	if err != nil {
		t.Fatalf("record turn: %v", err)
	}
	w.StopAgentWorker()
	if err := sink.Close(); err != nil {
		t.Fatalf("sink close: %v", err)
	}
	if original != "the answer is hi" {
		t.Fatalf("recorded answer = %q", original)
	}
	liveBefore, toolBefore := liveCalls, toolRuns

	// --- replay: rebuild from the journal and re-execute, no live services ---
	events, err := store.SessionEventsByAgent("agent-rr")
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	rec, err := replay.FromEvents(events)
	if err != nil {
		t.Fatalf("FromEvents: %v", err)
	}
	answers, err := replay.Session(context.Background(), rec)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	if len(answers) != 1 || answers[0] != original {
		t.Errorf("replay reproduced %v, want [%q]", answers, original)
	}
	// The replay must not have touched the live provider or the real tool.
	if liveCalls != liveBefore {
		t.Errorf("replay hit the live LLM: %d extra call(s)", liveCalls-liveBefore)
	}
	if toolRuns != toolBefore {
		t.Errorf("replay ran the real tool: %d extra run(s)", toolRuns-toolBefore)
	}
}
