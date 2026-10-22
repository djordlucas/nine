package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
)

// A model that makes the same call again and again gets the result with a
// notice from the third call on, and the turn ends at the fifth, before
// another model request; the stop answer stays out of the history.
func TestRepeatedCallIsNoticedThenEndsTheTurn(t *testing.T) {
	var calls atomic.Int32
	var lastPrompt atomic.Value
	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		calls.Add(1)
		lastPrompt.Store(requestText(req))
		return toolCallResp("tc", "echo", json.RawMessage(`{"goal_id": "g1"}`)), nil
	})
	loop := newTestLoop(provider, []ninectx.ToolWithVector{echoToolVec()})

	answer, err := loop.Run(context.Background(), "check the goal")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "echo was called 5 times") {
		t.Errorf("answer = %q, want the stop on the repeated call", answer)
	}
	if n := calls.Load(); n != agent.RepeatStopAt {
		t.Errorf("model requests = %d, want %d: the turn ends on the call that reaches the limit", n, agent.RepeatStopAt)
	}
	if p, _ := lastPrompt.Load().(string); !strings.Contains(p, "this is call 4 of echo") {
		t.Error("the model was not told the call repeated before the turn ended")
	}
	state, err := loop.SaveState()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), "Stopped this turn") {
		t.Error("the stop answer was put in the history as the model's own words")
	}
}

// A call whose result changes is not stuck, however often it is made.
func TestRepeatedCallWithChangingResultsRuns(t *testing.T) {
	var n atomic.Int32
	queue := llm.NewQueue(llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		if n.Add(1) > 6 {
			return finalResp("finished"), nil
		}
		return toolCallResp("tc", "poll", json.RawMessage(`{}`)), nil
	}), 1)
	dispatcher := agent.New()
	var polls atomic.Int32
	dispatcher.InjectHandler("poll", func(_ context.Context, _ json.RawMessage) (string, error) {
		return strings.Repeat(".", int(polls.Add(1))), nil
	})
	loop := agent.NewLoop(agent.Config{
		SystemCore: "You are a test agent.",
		Priority:   llm.PriorityConversation,
		Tools:      []ninectx.ToolWithVector{{Tool: llm.ToolDef{Name: "poll", Description: "poll", InputSchema: json.RawMessage(`{"type":"object"}`)}}},
	}, newTestBuilder(), queue, dispatcher)

	answer, err := loop.Run(context.Background(), "wait for it")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "finished" {
		t.Errorf("answer = %q, want the model's own answer", answer)
	}
}

// requestText joins a request's messages and tool results, for asserting what
// the model was shown.
func requestText(req llm.Request) string {
	b, _ := json.Marshal(req.Messages)
	return string(b)
}
