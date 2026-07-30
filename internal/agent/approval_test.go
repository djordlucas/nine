package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
)

func TestApprovalGateAllows(t *testing.T) {
	d := newWithHandler("shell", func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ran", nil
	})
	called := false
	d.SetApproval([]string{"shell"}, func(_ context.Context, name string, _ json.RawMessage) error {
		called = true
		if name != "shell" {
			t.Errorf("gate saw tool %q", name)
		}
		return nil
	})

	res, err := d.Dispatch(context.Background(), "shell", json.RawMessage(`{"command":"ls"}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !called {
		t.Error("approval gate not consulted")
	}
	if res.Output != "ran" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestApprovalGateBlocks(t *testing.T) {
	ran := false
	d := newWithHandler("shell", func(_ context.Context, _ json.RawMessage) (string, error) {
		ran = true
		return "ran", nil
	})
	d.SetApproval([]string{"shell"}, func(_ context.Context, _ string, _ json.RawMessage) error {
		return errors.New("rejected by user")
	})

	_, err := d.Dispatch(context.Background(), "shell", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("Dispatch returned nil err, want rejection")
	}
	if ran {
		t.Error("handler ran despite rejection")
	}
}

func TestApprovalGateUngatedToolUntouched(t *testing.T) {
	d := newWithHandler("echo", func(_ context.Context, _ json.RawMessage) (string, error) {
		return "ok", nil
	})
	d.SetApproval([]string{"shell"}, func(_ context.Context, _ string, _ json.RawMessage) error {
		t.Fatal("gate consulted for an ungated tool")
		return nil
	})

	if _, err := d.Dispatch(context.Background(), "echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
}

func TestRegisterAskHuman(t *testing.T) {
	d := agent.New()
	var gotQ string
	var gotOpts []string
	agent.RegisterAskHuman(d, "agent-1", func(_ context.Context, agentID, q string, opts []string) (string, error) {
		if agentID != "agent-1" {
			t.Errorf("agentID = %q", agentID)
		}
		gotQ, gotOpts = q, opts
		return "the answer", nil
	})

	res, err := d.Dispatch(context.Background(), "ask_human", json.RawMessage(`{"question":"Proceed?","options":["a","b"]}`))
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if gotQ != "Proceed?" || len(gotOpts) != 2 {
		t.Errorf("question=%q options=%v", gotQ, gotOpts)
	}
	if res.Output != "the answer" {
		t.Errorf("output = %q", res.Output)
	}
}

func TestRegisterAskHumanRequiresQuestion(t *testing.T) {
	d := agent.New()
	agent.RegisterAskHuman(d, "agent-1", func(_ context.Context, _, _ string, _ []string) (string, error) {
		t.Fatal("fn called despite empty question")
		return "", nil
	})
	if _, err := d.Dispatch(context.Background(), "ask_human", json.RawMessage(`{"question":"  "}`)); err == nil {
		t.Error("expected error for empty question")
	}
}

// A human's refusal is a decision, not a transient failure: the loop must
// dispatch the gated call once and hand the refusal to the model, rather than
// re-running it (and re-prompting the same person) maxToolRetries+1 times.
// With parallel sub-agent gates (R-HITL.5) the retry version multiplies into a
// barrage of identical questions.
func TestApprovalRejectionIsNotRetried(t *testing.T) {
	for _, tc := range []struct {
		name      string
		gateErr   error
		wantGates int32
	}{
		{"rejection is terminal", &agent.ApprovalError{Err: errors.New("tool echo rejected by user")}, 1},
		{"plain error still retries", errors.New("transient glitch"), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gateCalls, handlerCalls atomic.Int32

			dispatcher := agent.New()
			dispatcher.InjectHandler("echo", func(context.Context, json.RawMessage) (string, error) {
				handlerCalls.Add(1)
				return "ran", nil
			})
			dispatcher.SetApproval([]string{"echo"}, func(context.Context, string, json.RawMessage) error {
				gateCalls.Add(1)
				return tc.gateErr
			})

			provider := sequenceProvider([]llm.Response{{
				StopReason: "tool_use",
				ToolCalls:  []llm.ToolCall{{ID: "c1", Name: "echo", Input: json.RawMessage(`{}`)}},
			}})
			loop := agent.NewLoop(agent.Config{
				SystemCore: "You are a test agent.",
				Priority:   llm.PriorityConversation,
				Tools:      []ninectx.ToolWithVector{echoToolVec()},
			}, newTestBuilder(), llm.NewQueue(provider, 1), dispatcher)

			if _, err := loop.Run(context.Background(), "go"); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := gateCalls.Load(); got != tc.wantGates {
				t.Errorf("approval gate consulted %d times, want %d", got, tc.wantGates)
			}
			if got := handlerCalls.Load(); got != 0 {
				t.Errorf("blocked handler ran %d times, want 0", got)
			}
		})
	}
}
