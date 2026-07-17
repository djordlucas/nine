package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/llm/anthropic"
	"nine/internal/plugin"
)

// ---- helpers ----

func newTestBuilder() *ninectx.Builder {
	return ninectx.New(ninectx.Config{Budget: 100_000})
}

func echoToolVec() ninectx.ToolWithVector {
	return ninectx.ToolWithVector{
		Tool: llm.ToolDef{
			Name:        "echo",
			Description: "echo args",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
	}
}

func newTestLoop(provider llm.Provider, tools []ninectx.ToolWithVector) *agent.Loop {
	queue := llm.NewQueue(provider, 1)
	dispatcher := agent.New()
	dispatcher.InjectHandler("echo", func(_ context.Context, args json.RawMessage) (string, error) {
		return "echoed: " + string(args), nil
	})
	return agent.NewLoop(agent.Config{
		SystemCore: "You are a test agent.",
		Priority:   llm.PriorityConversation,
		Tools:      tools,
	}, newTestBuilder(), queue, dispatcher)
}

// sequenceProvider cycles through the given responses in order.
func sequenceProvider(responses []llm.Response) llm.Provider {
	var i atomic.Int32
	return llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		idx := int(i.Add(1)) - 1
		if idx >= len(responses) {
			return llm.Response{Text: "done", StopReason: "end_turn"}, nil
		}
		return responses[idx], nil
	})
}

func toolCallResp(id, name string, args json.RawMessage) llm.Response {
	return llm.Response{
		ToolCalls:  []llm.ToolCall{{ID: id, Name: name, Input: args}},
		StopReason: "tool_use",
	}
}

func finalResp(text string) llm.Response {
	return llm.Response{Text: text, StopReason: "end_turn"}
}

// ---- unit tests ----

func TestLoopNoTool(t *testing.T) {
	loop := newTestLoop(sequenceProvider([]llm.Response{finalResp("hello there")}), nil)

	answer, err := loop.Run(context.Background(), "say hi")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "hello there" {
		t.Errorf("answer = %q, want 'hello there'", answer)
	}
}

func TestLoopEmptyAnswerNoTools(t *testing.T) {
	// Model ends the turn with no tool calls and no text: the loop must not
	// return a blank answer.
	loop := newTestLoop(sequenceProvider([]llm.Response{finalResp("")}), nil)

	answer, err := loop.Run(context.Background(), "do something")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(answer) == "" {
		t.Error("expected a non-empty fallback answer, got blank")
	}
}

func TestLoopEmptyAnswerSurfacesToolFailure(t *testing.T) {
	// First call invokes a failing tool, second call gives up with empty text.
	// The returned answer must surface the tool failure rather than be blank.
	queue := llm.NewQueue(sequenceProvider([]llm.Response{
		toolCallResp("tc1", "boom", json.RawMessage(`{}`)),
		finalResp(""),
	}), 1)
	dispatcher := agent.New()
	dispatcher.InjectHandler("boom", func(_ context.Context, _ json.RawMessage) (string, error) {
		return "", fmt.Errorf("kaboom")
	})
	loop := agent.NewLoop(agent.Config{
		SystemCore: "You are a test agent.",
		Priority:   llm.PriorityConversation,
		Tools:      []ninectx.ToolWithVector{{Tool: llm.ToolDef{Name: "boom", Description: "boom", InputSchema: json.RawMessage(`{"type":"object"}`)}}},
	}, newTestBuilder(), queue, dispatcher)

	answer, err := loop.Run(context.Background(), "use the boom tool")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "boom") || !strings.Contains(answer, "kaboom") {
		t.Errorf("answer should surface the failed tool and error, got %q", answer)
	}
}

func TestLoopSingleTool(t *testing.T) {
	args := json.RawMessage(`{"msg":"ping"}`)
	var callCount atomic.Int32

	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		n := callCount.Add(1)
		if n == 1 {
			return toolCallResp("tc1", "echo", args), nil
		}
		// Second call: verify the observation from the first tool call is present.
		found := false
		for _, m := range req.Messages {
			for _, tr := range m.ToolResults {
				if strings.Contains(tr.Content, "echoed") {
					found = true
				}
			}
		}
		if !found {
			t.Error("second LLM call: observation from tool not in messages")
		}
		return finalResp("all done"), nil
	})

	loop := newTestLoop(provider, []ninectx.ToolWithVector{echoToolVec()})
	answer, err := loop.Run(context.Background(), "call echo then answer")
	if err != nil {
		t.Fatal(err)
	}
	if callCount.Load() != 2 {
		t.Errorf("llm call count = %d, want 2", callCount.Load())
	}
	if answer != "all done" {
		t.Errorf("answer = %q, want 'all done'", answer)
	}
}

func TestLoopMultiToolThreeTurns(t *testing.T) {
	responses := []llm.Response{
		toolCallResp("tc1", "echo", json.RawMessage(`{"n":1}`)),
		toolCallResp("tc2", "echo", json.RawMessage(`{"n":2}`)),
		toolCallResp("tc3", "echo", json.RawMessage(`{"n":3}`)),
		finalResp("three observations collected"),
	}
	loop := newTestLoop(sequenceProvider(responses), []ninectx.ToolWithVector{echoToolVec()})

	answer, err := loop.Run(context.Background(), "call echo 3 times")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "three observations collected" {
		t.Errorf("answer = %q", answer)
	}
}

func TestLoopObservationsAppended(t *testing.T) {
	// Verify each tool observation appears in subsequent LLM requests.
	var callCount atomic.Int32
	var failures []string

	wantByCall := map[int32]string{
		2: `{"n":1}`,
		3: `{"n":2}`,
		4: `{"n":3}`,
	}

	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		n := callCount.Add(1)
		if want, ok := wantByCall[n]; ok {
			found := false
			for _, m := range req.Messages {
				for _, tr := range m.ToolResults {
					if strings.Contains(tr.Content, want) {
						found = true
					}
				}
			}
			if !found {
				failures = append(failures, "call "+string(rune('0'+n))+": missing observation for "+want)
			}
		}
		switch n {
		case 1:
			return toolCallResp("tc1", "echo", json.RawMessage(`{"n":1}`)), nil
		case 2:
			return toolCallResp("tc2", "echo", json.RawMessage(`{"n":2}`)), nil
		case 3:
			return toolCallResp("tc3", "echo", json.RawMessage(`{"n":3}`)), nil
		default:
			return finalResp("done"), nil
		}
	})

	loop := newTestLoop(provider, []ninectx.ToolWithVector{echoToolVec()})
	loop.Run(context.Background(), "echo 3 times") //nolint:errcheck

	for _, f := range failures {
		t.Error(f)
	}
}

func TestLoopStateRoundtrip(t *testing.T) {
	// Run 3 complete user turns (each: 1 tool call + final answer).
	responses := []llm.Response{
		toolCallResp("tc1", "echo", json.RawMessage(`{"t":1}`)),
		finalResp("answer1"),
		toolCallResp("tc2", "echo", json.RawMessage(`{"t":2}`)),
		finalResp("answer2"),
		toolCallResp("tc3", "echo", json.RawMessage(`{"t":3}`)),
		finalResp("answer3"),
	}
	loop := newTestLoop(sequenceProvider(responses), []ninectx.ToolWithVector{echoToolVec()})

	for i, want := range []string{"answer1", "answer2", "answer3"} {
		got, err := loop.Run(context.Background(), "turn")
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
		if got != want {
			t.Errorf("turn %d: got %q, want %q", i+1, got, want)
		}
	}

	data, err := loop.SaveState()
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	var cp agent.ConversationState
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	// 3 user + 3 assistant = 6 history entries.
	if len(cp.History) != 6 {
		t.Errorf("history len = %d, want 6", len(cp.History))
	}
	// Scratchpad is cleared after each completed turn.
	if len(cp.Scratchpad) != 0 {
		t.Errorf("scratchpad len = %d, want 0", len(cp.Scratchpad))
	}

	// Restore and verify the loop continues from the checkpoint.
	newLoop := newTestLoop(
		sequenceProvider([]llm.Response{finalResp("restored answer")}),
		[]ninectx.ToolWithVector{echoToolVec()},
	)
	if err := newLoop.LoadState(data); err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	got, err := newLoop.Run(context.Background(), "one more")
	if err != nil {
		t.Fatal(err)
	}
	if got != "restored answer" {
		t.Errorf("restored loop answer = %q, want 'restored answer'", got)
	}
}

func TestLoopStatePreservesHistory(t *testing.T) {
	loop := newTestLoop(
		sequenceProvider([]llm.Response{finalResp("first"), finalResp("second")}),
		nil,
	)
	loop.Run(context.Background(), "msg1") //nolint:errcheck
	loop.Run(context.Background(), "msg2") //nolint:errcheck

	data, _ := loop.SaveState()
	var cp agent.ConversationState
	json.Unmarshal(data, &cp) //nolint:errcheck

	// history: user, assistant, user, assistant
	if len(cp.History) != 4 {
		t.Fatalf("history len = %d, want 4", len(cp.History))
	}
	if cp.History[0].Role != "user" || cp.History[0].Text != "msg1" {
		t.Errorf("history[0] = %+v", cp.History[0])
	}
	if cp.History[1].Role != "assistant" || cp.History[1].Text != "first" {
		t.Errorf("history[1] = %+v", cp.History[1])
	}
	if cp.History[2].Text != "msg2" {
		t.Errorf("history[2] = %+v", cp.History[2])
	}
	if cp.History[3].Text != "second" {
		t.Errorf("history[3] = %+v", cp.History[3])
	}
}

// ---- integration test ----

func TestLoopIntegration(t *testing.T) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set")
	}

	root := moduleRoot(t)
	tmpDir := t.TempDir()

	// Build shell and files plugins.
	for _, name := range []string{"shell", "files"} {
		binPath := filepath.Join(tmpDir, name)
		cmd := exec.Command("go", "build", "-mod=vendor", "-o", binPath, "./plugins/"+name)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, out)
		}
	}

	mgr := plugin.NewManager("")

	shellPlug, err := mgr.Start(filepath.Join(tmpDir, "shell"))
	if err != nil {
		t.Fatalf("start shell: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(shellPlug) })

	filesPlug, err := mgr.Start(filepath.Join(tmpDir, "files"))
	if err != nil {
		t.Fatalf("start files: %v", err)
	}
	t.Cleanup(func() { mgr.Stop(filesPlug) })

	var tools []ninectx.ToolWithVector
	for _, p := range []*plugin.Plugin{shellPlug, filesPlug} {
		for _, td := range p.Tools {
			tools = append(tools, ninectx.ToolWithVector{
				Tool: llm.ToolDef{
					Name:        td.Name,
					Description: td.Description,
					InputSchema: td.InputSchema,
				},
			})
		}
	}

	dispatcher := agent.New()
	dispatcher.RegisterPlugin(mgr, shellPlug)
	dispatcher.RegisterPlugin(mgr, filesPlug)

	provider := anthropic.New(apiKey, "claude-haiku-4-5-20251001", "", 0)
	queue := llm.NewQueue(provider, 1)
	builder := ninectx.New(ninectx.Config{Budget: 8000, ToolTopN: 10})

	loop := agent.NewLoop(agent.Config{
		SystemCore: "You are a helpful assistant. Use tools to accomplish tasks. When done, confirm what you did.",
		Priority:   llm.PriorityConversation,
		Tools:      tools,
		MaxTokens:  1024,
	}, builder, queue, dispatcher)

	ctx := context.Background()

	// Task 1: create a file.
	target := filepath.Join(t.TempDir(), "nine-test.txt")
	_, err = loop.Run(ctx, "Create a file at "+target+" with the content 'hello nine'")
	if err != nil {
		t.Fatalf("task1: %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("task1: file not created: %v", err)
	}
	if !strings.Contains(string(content), "hello nine") {
		t.Errorf("task1: file content = %q, want to contain 'hello nine'", content)
	}

	// Task 2: list /tmp.
	answer, err := loop.Run(ctx, "What files are in /tmp? List them using the shell tool.")
	if err != nil {
		t.Fatalf("task2: %v", err)
	}
	if answer == "" {
		t.Error("task2: empty answer")
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func TestLoopMaxTokensDefault(t *testing.T) {
	var recordedMaxTokens int
	provider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		recordedMaxTokens = req.MaxTokens
		return finalResp("ok"), nil
	})

	queue := llm.NewQueue(provider, 1)
	dispatcher := agent.New()
	loop := agent.NewLoop(agent.Config{
		SystemCore: "test",
		Priority:   llm.PriorityConversation,
		MaxTokens:  0, // should default to 4096
	}, newTestBuilder(), queue, dispatcher)

	_, err := loop.Run(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if recordedMaxTokens != 4096 {
		t.Errorf("MaxTokens = %d, want 4096", recordedMaxTokens)
	}
}

func TestLoadStateBadJSON(t *testing.T) {
	loop := newTestLoop(sequenceProvider(nil), nil)
	err := loop.LoadState([]byte("not-json"))
	if err == nil {
		t.Error("expected error for bad JSON, got nil")
	}
}
func TestLoopDefaultThinkPolicy(t *testing.T) {
	var seen []*bool
	thinkRecorderProvider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		seen = append(seen, req.Think)
		if len(seen) == 1 {
			return toolCallResp("c1", "echo", json.RawMessage(`{}`)), nil
		}
		return finalResp("done"), nil
	})

	queue := llm.NewQueue(thinkRecorderProvider, 1)
	dispatcher := agent.New()

	loop := agent.NewLoop(agent.Config{
		SystemCore:  "You are a test agent.",
		Priority:    llm.PriorityConversation,
		ThinkPolicy: agent.DefaultThinkPolicy,
	}, newTestBuilder(), queue, dispatcher)

	_, err := loop.Run(context.Background(), "test thinking")
	if err != nil {
		t.Fatal(err)
	}

	if *seen[0] != true {
		t.Errorf("first LLM call: Think = %v, want true", *seen[0])
	}

	if *seen[1] != false {
		t.Errorf("second LLM call: Think = %v, want false", *seen[1])
	}
}

func TestLoopNoThinkPolicy(t *testing.T) {
	var seen []*bool
	thinkRecorderProvider := llm.ProviderFunc(func(_ context.Context, req llm.Request) (llm.Response, error) {
		seen = append(seen, req.Think)
		if len(seen) == 1 {
			return toolCallResp("c1", "echo", json.RawMessage(`{}`)), nil
		}
		return finalResp("done"), nil
	})

	queue := llm.NewQueue(thinkRecorderProvider, 1)
	dispatcher := agent.New()

	loop := agent.NewLoop(agent.Config{
		SystemCore: "You are a test agent.",
		Priority:   llm.PriorityConversation,
	}, newTestBuilder(), queue, dispatcher)

	_, err := loop.Run(context.Background(), "test thinking")
	if err != nil {
		t.Fatal(err)
	}

	if seen[0] != nil {
		t.Errorf("first LLM call: Think = %v, want nil", seen[0])
	}

	if seen[1] != nil {
		t.Errorf("second LLM call: Think = %v, want nil", seen[1])
	}
}

// ---- analysis-pass (M4) ----

// recordingThinker records every request it sees and returns canned responses
// in order (falling back to a final "done"). It implements llm.ThinkingAware so
// tests can drive the queue's SupportsThinking gate. Not safe for concurrent
// use, but the loop serializes calls (each Submit blocks), so it's fine here.
type recordingThinker struct {
	supportsThinking bool
	responses        []llm.Response
	reqs             []llm.Request
	n                int
}

func (p *recordingThinker) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.reqs = append(p.reqs, req)
	i := p.n
	p.n++
	if i >= len(p.responses) {
		return finalResp("done"), nil
	}
	return p.responses[i], nil
}

func (p *recordingThinker) SupportsThinking(context.Context) bool { return p.supportsThinking }

func newEchoDispatcher() *agent.Dispatcher {
	d := agent.New()
	d.InjectHandler("echo", func(_ context.Context, _ json.RawMessage) (string, error) {
		return "echoed", nil
	})
	return d
}

// Unsupported model → the no-tool analysis pass runs first, and its output is
// injected as the plan into the subsequent (tool-carrying) loop calls.
func TestLoopAnalysisPassRunsWhenUnsupported(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: false,
		responses: []llm.Response{
			finalResp("PLAN: check the weather, then answer"), // call 1 = analysis pass
			toolCallResp("c1", "echo", json.RawMessage(`{}`)), // call 2 = loop, uses a tool
			finalResp("done"), // call 3 = final answer
		},
	}
	loop := agent.NewLoop(agent.Config{
		SystemCore:     "You are a test agent.",
		Priority:       llm.PriorityConversation,
		Tools:          []ninectx.ToolWithVector{echoToolVec()},
		AnalysisPrompt: "ANALYST_PERSONA: analyze the request; do not act.",
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	if _, err := loop.Run(context.Background(), "weather?"); err != nil {
		t.Fatal(err)
	}

	if len(prov.reqs) < 2 {
		t.Fatalf("got %d LLM calls, want >=2 (analysis + loop)", len(prov.reqs))
	}
	// Call 1 is the analysis pass: no tools, analyst persona in the system prompt.
	if len(prov.reqs[0].Tools) != 0 {
		t.Errorf("analysis pass carried %d tools, want 0", len(prov.reqs[0].Tools))
	}
	if !strings.Contains(prov.reqs[0].System, "ANALYST_PERSONA") {
		t.Errorf("analysis pass system = %q, want analyst persona", prov.reqs[0].System)
	}
	// Call 2 is the real loop call: carries tools AND the plan.
	if len(prov.reqs[1].Tools) == 0 {
		t.Error("loop call carried no tools, want the echo tool")
	}
	if !strings.Contains(prov.reqs[1].System, "PLAN: check the weather") {
		t.Errorf("loop call system missing the injected plan; got %q", prov.reqs[1].System)
	}
}

// Model supports native thinking → no analysis pass; the first call is the
// normal tool-carrying loop call and no plan is injected.
func TestLoopAnalysisPassSkippedWhenSupported(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: true,
		responses:        []llm.Response{finalResp("direct answer")},
	}
	loop := agent.NewLoop(agent.Config{
		SystemCore:     "You are a test agent.",
		Priority:       llm.PriorityConversation,
		Tools:          []ninectx.ToolWithVector{echoToolVec()},
		AnalysisPrompt: "ANALYST_PERSONA: analyze the request; do not act.",
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	if _, err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	if len(prov.reqs) != 1 {
		t.Fatalf("got %d LLM calls, want 1 (no analysis pass when thinking is supported)", len(prov.reqs))
	}
	if len(prov.reqs[0].Tools) == 0 {
		t.Error("the single call carried no tools, want the echo tool")
	}
	if strings.Contains(prov.reqs[0].System, "planning pass") {
		t.Error("a plan was injected even though thinking is supported")
	}
}

// No analyst prompt configured → the pass is disabled even on an unsupported model.
func TestLoopAnalysisPassSkippedWithoutPrompt(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: false,
		responses:        []llm.Response{finalResp("answer")},
	}
	loop := agent.NewLoop(agent.Config{
		SystemCore: "You are a test agent.",
		Priority:   llm.PriorityConversation,
		Tools:      []ninectx.ToolWithVector{echoToolVec()},
		// AnalysisPrompt intentionally empty.
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	if _, err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(prov.reqs) != 1 {
		t.Fatalf("got %d LLM calls, want 1 (no analysis pass without a prompt)", len(prov.reqs))
	}
}

func TestLoopPlanReviewProceed(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: false,
		responses: []llm.Response{
			finalResp("PLAN: answer directly"), // call #1 analysis pass
			finalResp("done"),                  // call #2 loop final answer
		},
	}
	var reviewed []string
	loop := agent.NewLoop(agent.Config{
		SystemCore:     "you are a test agent",
		Priority:       llm.PriorityConversation,
		Tools:          []ninectx.ToolWithVector{echoToolVec()},
		AnalysisPrompt: "ANALYST_PERSONA: analyse the request; do not act.",
		PlanReviewFn: func(ctx context.Context, plan string) (agent.PlanDecision, error) {
			reviewed = append(reviewed, plan)
			return agent.PlanDecision{Proceed: true}, nil
		},
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())
	if _, err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}

	if len(reviewed) != 1 {
		t.Fatalf("plan review called %d times, want 1", len(reviewed))
	}

	if len(prov.reqs) != 2 {
		t.Fatalf("got %d LLM calls, want 2 (analysis+loop)", len(prov.reqs))
	}
}

func TestLoopPlanReviewRejectReanalyses(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: false,
		responses: []llm.Response{
			finalResp("PLAN v1"),
			finalResp("PLAN v2"),
			finalResp("done"),
		},
	}

	var reviewed []string
	loop := agent.NewLoop(agent.Config{
		SystemCore:     "you are a test agent",
		Priority:       llm.PriorityConversation,
		Tools:          []ninectx.ToolWithVector{echoToolVec()},
		AnalysisPrompt: "ANALYST_PERSONA: analyse the request; do not act.",
		PlanReviewFn: func(_ context.Context, plan string) (agent.PlanDecision, error) {
			reviewed = append(reviewed, plan)
			if len(reviewed) == 1 {
				return agent.PlanDecision{Proceed: false, Clarification: "also check the calendar"}, nil
			}
			return agent.PlanDecision{Proceed: true}, nil
		},
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	if _, err := loop.Run(context.Background(), "what's my day look like?"); err != nil {
		t.Fatal(err)
	}
	if len(reviewed) != 2 {
		t.Fatalf("plan review called %d times, want 2 (reject then proceed)", len(reviewed))
	}
	if reviewed[0] != "PLAN v1" || reviewed[1] != "PLAN v2" {
		t.Errorf("reviewed plans = %v, want [PLAN v1, PLAN v2]", reviewed)
	}
	// the second analysis pass is tool-free and must see the clarification in history
	if len(prov.reqs) < 2 {
		t.Fatalf("got %d LLM calls, want >= 2 analysis passes", len(prov.reqs))
	}
	if len(prov.reqs[1].Tools) != 0 {
		t.Errorf("second analysis pass carried %d tools, want 0", len(prov.reqs[1].Tools))
	}
	found := false
	for _, m := range prov.reqs[1].Messages {
		if strings.Contains(m.Text, "also check the calendar") {
			found = true
		}
	}
	if !found {
		t.Error("second analysis dit not see the clarification in history")
	}
}

func TestDefaultThinkPolicy(t *testing.T) {
	if !agent.DefaultThinkPolicy(1, false) || agent.DefaultThinkPolicy(2, false) {
		t.Error("DefaultThinkPolicy should think on call 1 only")
	}
	if !agent.DefaultThinkPolicy(2, true) {
		t.Error("forced should think on any call")
	}
}

func TestLoopPlanModeOff(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: false, // would normally trigger the analysis pass
		responses:        []llm.Response{finalResp("answer")},
	}
	loop := agent.NewLoop(agent.Config{
		SystemCore:     "test",
		Priority:       llm.PriorityConversation,
		Tools:          []ninectx.ToolWithVector{echoToolVec()},
		AnalysisPrompt: "ANALYST_PERSONA",
		ThinkPolicy:    agent.DefaultThinkPolicy,
		PlanMode:       agent.PlanModeOff,
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	if _, err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if len(prov.reqs) != 1 {
		t.Fatalf("plan_mode=off ran %d calls, want 1 (no analysis pass)", len(prov.reqs))
	}
	if prov.reqs[0].Think != nil && *prov.reqs[0].Think {
		t.Error("plan_mode=off requested thinking")
	}
}

func TestLoopForceThinkNextTurn(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: true,
		responses:        []llm.Response{finalResp("done"), finalResp("done")},
	}
	loop := agent.NewLoop(agent.Config{
		SystemCore:  "test",
		Priority:    llm.PriorityConversation,
		Tools:       []ninectx.ToolWithVector{echoToolVec()},
		ThinkPolicy: func(_ int, forced bool) bool { return forced }, // isolates the force path
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	loop.SetForceThinkNextTurn(true)
	if _, err := loop.Run(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if prov.reqs[0].Think == nil || !*prov.reqs[0].Think {
		t.Error("forced turn did not request thinking")
	}

	// Force is per-turn: a subsequent Run without the setter must not force.
	if _, err := loop.Run(context.Background(), "again"); err != nil {
		t.Fatal(err)
	}
	if prov.reqs[1].Think == nil || *prov.reqs[1].Think {
		t.Error("force leaked into the next turn")
	}
}

func TestLoopSetPlanModeLive(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: true,
		responses:        []llm.Response{finalResp("a"), finalResp("b")},
	}
	loop := agent.NewLoop(agent.Config{
		SystemCore:  "test",
		Priority:    llm.PriorityConversation,
		Tools:       []ninectx.ToolWithVector{echoToolVec()},
		ThinkPolicy: agent.DefaultThinkPolicy,
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	if _, err := loop.Run(context.Background(), "one"); err != nil { // plan-only default
		t.Fatal(err)
	}
	if prov.reqs[0].Think == nil || !*prov.reqs[0].Think {
		t.Error("plan-only turn should think on call 1")
	}
	loop.SetPlanMode(agent.PlanModeOff)
	if _, err := loop.Run(context.Background(), "two"); err != nil {
		t.Fatal(err)
	}
	if prov.reqs[1].Think == nil || *prov.reqs[1].Think {
		t.Error("after SetPlanMode(off), should not think")
	}
}

// On an unsupported model the analysis pass emits plan_start/plan_end each turn,
// and the capability-downgrade notice fires exactly once for the session.
func TestLoopPlanEventsAndNotice(t *testing.T) {
	prov := &recordingThinker{
		supportsThinking: false,
		responses: []llm.Response{
			finalResp("PLAN one"), finalResp("done"), // turn 1: analysis + loop
			finalResp("PLAN two"), finalResp("done"), // turn 2: analysis + loop
		},
	}
	loop := agent.NewLoop(agent.Config{
		SystemCore:     "test",
		Priority:       llm.PriorityConversation,
		Tools:          []ninectx.ToolWithVector{echoToolVec()},
		AnalysisPrompt: "ANALYST_PERSONA",
	}, newTestBuilder(), llm.NewQueue(prov, 1), newEchoDispatcher())

	var starts, ends, notices int
	loop.SetOnPlanStart(func() { starts++ })
	loop.SetOnPlanEnd(func() { ends++ })
	loop.SetOnNotice(func(string) { notices++ })

	for range 2 {
		if _, err := loop.Run(context.Background(), "hi"); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 2 || ends != 2 {
		t.Errorf("plan start/end = %d/%d, want 2/2", starts, ends)
	}
	if notices != 1 {
		t.Errorf("downgrade notice fired %d times, want 1 (one-shot per session)", notices)
	}
}

// TestEmptyAnswerNotWrittenToHistory pins the invariant that the empty-answer
// fallback is returned to the caller but never recorded as something the model
// said. Writing it to history let a long-lived session (self-reflection) read it
// back and imitate it, filling the reflections table with the apology.
func TestEmptyAnswerNotWrittenToHistory(t *testing.T) {
	// A model that does its work via a tool call and then has nothing to say —
	// the normal shape of an idle reflection turn.
	var calls atomic.Int64
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		if calls.Add(1) == 1 {
			return llm.Response{ToolCalls: []llm.ToolCall{{ID: "1", Name: "echo", Input: json.RawMessage(`{}`)}}}, nil
		}
		return llm.Response{Text: ""}, nil // silence is a valid end to this turn
	})

	loop := newTestLoop(provider, []ninectx.ToolWithVector{echoToolVec()})
	answer, err := loop.Run(context.Background(), "reflect")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The caller still learns the turn produced nothing.
	if !strings.Contains(answer, "wasn't able to produce") {
		t.Errorf("answer = %q, want the empty-answer fallback", answer)
	}

	data, err := loop.SaveState()
	if err != nil {
		t.Fatalf("SaveState: %v", err)
	}
	var cp agent.ConversationState
	if err := json.Unmarshal(data, &cp); err != nil {
		t.Fatalf("unmarshal checkpoint: %v", err)
	}
	for _, m := range cp.History {
		if strings.Contains(m.Text, "wasn't able to produce") {
			t.Fatalf("fallback leaked into history as %s: %q — the model will imitate it", m.Role, m.Text)
		}
	}
	// Only the user turn is recorded; the model said nothing worth keeping.
	if len(cp.History) != 1 || cp.History[0].Role != "user" {
		t.Errorf("history = %+v, want just the user turn", cp.History)
	}
}
