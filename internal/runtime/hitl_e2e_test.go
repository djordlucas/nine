package runtime_test

import (
	"encoding/json"
	"testing"
	"time"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/memory/memtest"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

// TestHITLEndToEnd drives the full daemon path the docker TUI uses: an
// interactive conversation whose LLM calls ask_human, answered over a second
// connection. Reproduces "no pending question for that request".
func TestHITLEndToEnd(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hitl := runtime.NewHITL(store, time.Minute)

	// LLM: first call asks the human, second call returns the answer text.
	provider := seqProvider([]llm.Response{
		{
			ToolCalls: []llm.ToolCall{{
				ID:    "call-1",
				Name:  "ask_human",
				Input: json.RawMessage(`{"question":"Proceed?"}`),
			}},
			StopReason: "tool_use",
		},
		finalResp("done"),
	})
	builder := ninectx.New(ninectx.Config{Budget: 100_000})
	queue := llm.NewQueue(provider, 4)
	factory := func(agentID string, params runtime.RoleParams) *agent.Loop {
		interactive := params.Interactive
		d := agent.New()
		if interactive {
			agent.RegisterAskHuman(d, agentID, hitl.Ask)
		}
		return agent.NewLoop(agent.Config{
			SystemCore: "test",
			Priority:   llm.PriorityConversation,
		}, builder, queue, d)
	}

	d, sock := startDaemon(t, factory, nil, nil)
	hitl.SetEmit(d.EmitProgress)
	d.ConfigureHITL(hitl)

	// Turn connection.
	c := dial(t, sock)
	id, _, err := c.NewConversationInteractive(true)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}

	type turnResult struct {
		text string
		err  error
	}
	done := make(chan turnResult, 1)
	reqIDCh := make(chan string, 1)
	go func() {
		text, terr := c.TurnWithProgress(id, "hi", func(evt protocol.ProgressEvent) {
			if evt.Type == "human_input_required" && evt.HumanRequest != nil {
				select {
				case reqIDCh <- evt.HumanRequest.RequestID:
				default:
				}
			}
		})
		done <- turnResult{text, terr}
	}()

	var reqID string
	select {
	case reqID = <-reqIDCh:
	case <-time.After(5 * time.Second):
		t.Fatal("never received human_input_required")
	}
	if reqID == "" {
		t.Fatal("empty request id")
	}

	// Answer on a second connection, exactly as the TUI does.
	a := dial(t, sock)
	if err := a.AnswerHuman(id, reqID, "yes"); err != nil {
		t.Fatalf("AnswerHuman: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("turn err: %v", res.err)
		}
		if res.text != "done" {
			t.Errorf("turn text = %q, want done", res.text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not complete after answer")
	}
}
