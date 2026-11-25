package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/protocol"
)

// compactTestLoop builds a loop in compact mode with a small context window,
// so a few sizable history messages push the request past CompactAt.
func compactTestLoop(provider llm.Provider, window int) *agent.Loop {
	queue := llm.NewQueue(provider, 1)
	dispatcher := agent.New()
	builder := ninectx.New(ninectx.Config{Budget: window})
	return agent.NewLoop(agent.Config{
		SystemCore:  "You are a test agent.",
		Priority:    llm.PriorityConversation,
		ContextMode: protocol.ContextModeCompact,
	}, builder, queue, dispatcher)
}

// compactProvider serves the summarizer (recognised by its system prompt)
// from summarize, then answers every other call from answers; on first main
// call it can be told to fail with an overflow error once.
type compactProvider struct {
	mu        sync.Mutex
	summarize func() (llm.Response, error)
	answers   []llm.Response
	calls     []llm.Request
	overflow  bool // fail the first non-summarizer call with an overflow error
}

func (p *compactProvider) Complete(_ context.Context, req llm.Request) (llm.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, req)
	if req.System == ninectx.SummarizerPrompt {
		return p.summarize()
	}
	if p.overflow {
		p.overflow = false
		return llm.Response{}, errors.New("llm: 400 Bad Request: context length exceeded")
	}
	if len(p.answers) == 0 {
		return llm.Response{}, errors.New("no answer left")
	}
	r := p.answers[0]
	p.answers = p.answers[1:]
	return r, nil
}

func bigMsg(role, text string) llm.Message { return llm.Message{Role: role, Text: text} }

func TestLoopCompactFoldsHistoryNearWindow(t *testing.T) {
	window := 1_000 // tokens; CompactAt → 750, KeepShare → 250 tokens ≈ 860 chars
	big := strings.Repeat("x", 900)
	p := &compactProvider{
		summarize: func() (llm.Response, error) { return llm.Response{Text: "SUMMARY of the old part"}, nil },
		answers:   []llm.Response{finalResp("done")},
	}
	loop := compactTestLoop(p, window)
	// Seed history past the threshold: 4 × ~900-char messages ≈ 1000+ tokens.
	loop.LoadState(mustJSON(t, agent.ConversationState{
		History: []llm.Message{bigMsg("user", big), bigMsg("assistant", big), bigMsg("user", big), bigMsg("assistant", big)},
	}))

	answer, err := loop.Run(context.Background(), "continue please")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("answer = %q", answer)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Some summarizer calls (chunked), then exactly one main call, last.
	mainCalls := 0
	var main llm.Request
	for _, c := range p.calls {
		if c.System == ninectx.SummarizerPrompt {
			continue
		}
		mainCalls++
		main = c
	}
	if mainCalls != 1 {
		t.Fatalf("main calls = %d, want 1 (summarize + answer)", mainCalls)
	}
	if len(p.calls) < 2 {
		t.Fatalf("no summarizer call happened: %d calls", len(p.calls))
	}
	if !strings.Contains(main.System, "Conversation summary") || !strings.Contains(main.System, "SUMMARY of the old part") {
		t.Errorf("main request system lacks the summary:\n%s", main.System)
	}
	// The folded part is gone from the sent messages: only the kept tail plus
	// the new user message remain.
	for _, m := range main.Messages {
		if m.Text == big {
			t.Errorf("a folded message was still sent")
		}
	}
	// State persists the summary.
	state, err := loop.SaveState()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(state), "SUMMARY of the old part") || !strings.Contains(string(state), `"compactions":1`) {
		t.Errorf("checkpoint missing summary/compactions: %s", state)
	}
}

func TestLoopCompactOnOverflowError(t *testing.T) {
	window := 1_000
	big := strings.Repeat("y", 900)
	p := &compactProvider{
		summarize: func() (llm.Response, error) { return llm.Response{Text: "folded summary"}, nil },
		answers:   []llm.Response{finalResp("recovered")},
		overflow:  true, // the first main call overflows
	}
	loop := compactTestLoop(p, window)
	loop.LoadState(mustJSON(t, agent.ConversationState{
		History: []llm.Message{bigMsg("user", big), bigMsg("assistant", big)},
	}))
	// Small window means the threshold may not trigger first; force the path
	// by making the window tiny for the estimate: history alone is ~520 tokens.
	// Instead rely on the overflow retry: the first submit fails, compaction
	// runs, the rebuilt request is sent.
	answer, err := loop.Run(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "recovered" {
		t.Fatalf("answer = %q, want recovered", answer)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	summarized := false
	mainCalls := 0
	for _, c := range p.calls {
		if c.System == ninectx.SummarizerPrompt {
			summarized = true
		} else {
			mainCalls++
		}
	}
	if !summarized {
		t.Error("no summarizer call after overflow")
	}
	if mainCalls != 2 {
		t.Errorf("main calls = %d, want 2 (failed + retried)", mainCalls)
	}
}

func TestLoopTrimModeNeverCompacts(t *testing.T) {
	window := 1_000
	big := strings.Repeat("z", 900)
	p := &compactProvider{
		summarize: func() (llm.Response, error) {
			t.Error("summarizer called in trim mode")
			return llm.Response{}, nil
		},
		answers: []llm.Response{finalResp("trimmed")},
	}
	queue := llm.NewQueue(p, 1)
	builder := ninectx.New(ninectx.Config{Budget: window})
	loop := agent.NewLoop(agent.Config{
		SystemCore: "You are a test agent.",
		Priority:   llm.PriorityConversation,
		// ContextMode "" → trim
	}, builder, queue, agent.New())
	loop.LoadState(mustJSON(t, agent.ConversationState{
		History: []llm.Message{bigMsg("user", big), bigMsg("assistant", big), bigMsg("user", big)},
	}))
	if _, err := loop.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
