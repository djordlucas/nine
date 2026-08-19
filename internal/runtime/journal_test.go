package runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

// scriptedResponses returns the responses in order, one per Complete call.
func scriptedResponses(responses ...llm.Response) llm.Provider {
	i := 0
	return llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		r := responses[i]
		if i < len(responses)-1 {
			i++
		}
		return r, nil
	})
}

// TestJournalReconstructsTurn is the v1 gate (docs/event-log.md §11): after a
// turn runs, its exact LLM request/response and full tool trajectory are
// reconstructable from the durable journal, independent of the live worker.
func TestJournalReconstructsTurn(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Turn shape: model calls a tool, then answers with the tool's result.
	provider := scriptedResponses(
		llm.Response{
			Text:       "let me check",
			ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "echo", Input: json.RawMessage(`{"v":"hi"}`)}},
			StopReason: "tool_use",
		},
		llm.Response{Text: "the answer is hi", StopReason: "end_turn"},
	)

	builder := ninectx.New(ninectx.Config{Budget: 100_000})
	queue := llm.NewQueue(provider, 1)
	dispatcher := agent.New()
	dispatcher.InjectHandler("echo", func(_ context.Context, args json.RawMessage) (string, error) {
		return "echoed:" + string(args), nil
	})
	loop := agent.NewLoop(agent.Config{
		SystemCore: "test-core",
		Priority:   llm.PriorityConversation,
		Tools:      []ninectx.ToolWithVector{{Tool: llm.ToolDef{Name: "echo", Description: "echoes input"}}},
	}, builder, queue, dispatcher)

	sink := runtime.NewSQLEventSinkForTest(store)
	w := runtime.NewAgentWorkerWithSinkForTest("agent-journal", loop, sink)

	answer, err := w.TurnAgentWorker(context.Background(), "what is it?")
	if err != nil {
		t.Fatalf("turn: %v", err)
	}
	if answer != "the answer is hi" {
		t.Fatalf("answer = %q", answer)
	}
	w.StopAgentWorker()
	// Flush the async sink so every event is durably written before we read.
	if err := sink.Close(); err != nil {
		t.Fatalf("sink close: %v", err)
	}

	evs, err := store.SessionEventsByAgent("agent-journal")
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	byType := map[string][]memory.SessionEvent{}
	for _, e := range evs {
		byType[e.Type] = append(byType[e.Type], e)
	}

	// The turn is bracketed and both inner LLM calls are captured.
	for _, want := range []struct {
		typ string
		n   int
	}{
		{"turn_start", 1},
		{"turn_end", 1},
		{"llm_request", 2},
		{"llm_response", 2},
		{"tool_start", 1},
		{"tool_end", 1},
	} {
		if got := len(byType[want.typ]); got != want.n {
			t.Errorf("event %q: got %d, want %d", want.typ, got, want.n)
		}
	}

	// The exact request is reconstructable: the first llm_request carries the
	// user's text and the advertised tool.
	var req struct {
		System    string        `json:"system"`
		Messages  []llm.Message `json:"messages"`
		ToolNames []string      `json:"tool_names"`
		LLMCallN  int           `json:"llm_call_n"`
	}
	if err := json.Unmarshal(byType["llm_request"][0].Payload, &req); err != nil {
		t.Fatalf("unmarshal llm_request: %v", err)
	}
	if len(req.Messages) == 0 || req.Messages[0].Text != "what is it?" {
		t.Errorf("llm_request messages missing user text: %+v", req.Messages)
	}
	if len(req.ToolNames) != 1 || req.ToolNames[0] != "echo" {
		t.Errorf("llm_request tool_names = %v, want [echo]", req.ToolNames)
	}

	// The raw response is reconstructable: the first llm_response carries the
	// tool call the model requested.
	var resp struct {
		Text       string         `json:"text"`
		ToolCalls  []llm.ToolCall `json:"tool_calls"`
		StopReason string         `json:"stop_reason"`
	}
	if err := json.Unmarshal(byType["llm_response"][0].Payload, &resp); err != nil {
		t.Fatalf("unmarshal llm_response: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "echo" {
		t.Errorf("llm_response tool_calls = %+v", resp.ToolCalls)
	}

	// The tool trajectory is reconstructable with the model-visible output.
	var end struct {
		Name     string `json:"name"`
		Output   string `json:"output"`
		Attempts int    `json:"attempts"`
	}
	if err := json.Unmarshal(byType["tool_end"][0].Payload, &end); err != nil {
		t.Fatalf("unmarshal tool_end: %v", err)
	}
	if end.Name != "echo" || end.Output != `echoed:{"v":"hi"}` || end.Attempts != 1 {
		t.Errorf("tool_end payload = %+v", end)
	}

	// The tree links: the tool span's parent is the first LLM call's span.
	ts := byType["tool_start"][0]
	if ts.ParentSpanID != byType["llm_request"][0].SpanID {
		t.Errorf("tool_start parent = %q, want llm span %q", ts.ParentSpanID, byType["llm_request"][0].SpanID)
	}

	// turn_end reflects the tool count and terminal answer.
	var te struct {
		Result    string `json:"result"`
		ToolCount int    `json:"tool_count"`
	}
	if err := json.Unmarshal(byType["turn_end"][0].Payload, &te); err != nil {
		t.Fatalf("unmarshal turn_end: %v", err)
	}
	if te.ToolCount != 1 || te.Result != "the answer is hi" {
		t.Errorf("turn_end = %+v", te)
	}
}

// Provider-reported token usage reaches the journal, and reconciles against the
// context builder's pre-send estimate through the shared LLM-call span: the
// estimate lives on llm_request, the actual on llm_response, and the two are
// joined on span_id rather than duplicated onto one event.
func TestJournalRecordsTokenUsage(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	provider := scriptedResponses(llm.Response{
		Text:       "done",
		StopReason: "end_turn",
		Usage:      llm.Usage{InputTokens: 4321, OutputTokens: 21},
	})

	builder := ninectx.New(ninectx.Config{Budget: 100_000})
	loop := agent.NewLoop(agent.Config{
		SystemCore: "test-core",
		Priority:   llm.PriorityConversation,
	}, builder, llm.NewQueue(provider, 1), agent.New())

	sink := runtime.NewSQLEventSinkForTest(store)
	w := runtime.NewAgentWorkerWithSinkForTest("agent-usage", loop, sink)
	if _, err := w.TurnAgentWorker(context.Background(), "hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	w.StopAgentWorker()
	if err := sink.Close(); err != nil {
		t.Fatalf("sink close: %v", err)
	}

	evs, err := store.SessionEventsByAgent("agent-usage")
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}

	var reqEv, respEv *memory.SessionEvent
	for i, e := range evs {
		switch e.Type {
		case "llm_request":
			reqEv = &evs[i]
		case "llm_response":
			respEv = &evs[i]
		}
	}
	if reqEv == nil || respEv == nil {
		t.Fatalf("missing llm_request/llm_response in %d events", len(evs))
	}
	if reqEv.SpanID != respEv.SpanID {
		t.Fatalf("span ids differ (%q vs %q); estimate and actual are not joinable",
			reqEv.SpanID, respEv.SpanID)
	}

	var resp struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	if err := json.Unmarshal(respEv.Payload, &resp); err != nil {
		t.Fatalf("unmarshal llm_response: %v", err)
	}
	if resp.InputTokens != 4321 || resp.OutputTokens != 21 {
		t.Errorf("llm_response usage = {%d %d}, want {4321 21}", resp.InputTokens, resp.OutputTokens)
	}

	// The other half of the reconciliation: the estimate is on llm_request.
	var req struct {
		TokensUsed int `json:"tokens_used"`
		Budget     int `json:"budget"`
	}
	if err := json.Unmarshal(reqEv.Payload, &req); err != nil {
		t.Fatalf("unmarshal llm_request: %v", err)
	}
	if req.TokensUsed <= 0 || req.Budget != 100_000 {
		t.Errorf("llm_request estimate = {used %d, budget %d}, want a positive estimate against a 100000 budget",
			req.TokensUsed, req.Budget)
	}
}

// A provider that reports no counts must leave the usage keys off the payload
// entirely, so a zero is never mistaken for a measured zero.
func TestJournalOmitsUnreportedUsage(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	provider := scriptedResponses(llm.Response{Text: "done", StopReason: "end_turn"})
	loop := agent.NewLoop(agent.Config{
		SystemCore: "test-core",
		Priority:   llm.PriorityConversation,
	}, ninectx.New(ninectx.Config{Budget: 100_000}), llm.NewQueue(provider, 1), agent.New())

	sink := runtime.NewSQLEventSinkForTest(store)
	w := runtime.NewAgentWorkerWithSinkForTest("agent-nousage", loop, sink)
	if _, err := w.TurnAgentWorker(context.Background(), "hello"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	w.StopAgentWorker()
	if err := sink.Close(); err != nil {
		t.Fatalf("sink close: %v", err)
	}

	evs, err := store.SessionEventsByAgent("agent-nousage")
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	for _, e := range evs {
		if e.Type != "llm_response" {
			continue
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(e.Payload, &keys); err != nil {
			t.Fatalf("unmarshal llm_response: %v", err)
		}
		if _, ok := keys["input_tokens"]; ok {
			t.Errorf("llm_response carries input_tokens when the provider reported none: %s", e.Payload)
		}
		if _, ok := keys["output_tokens"]; ok {
			t.Errorf("llm_response carries output_tokens when the provider reported none: %s", e.Payload)
		}
	}
}
