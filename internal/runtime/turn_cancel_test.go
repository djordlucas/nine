package runtime_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/protocol"
)

// A cancelled turn ends as "turn cancelled"; the session stays, and its next
// turn runs.
func TestTurnCancel(t *testing.T) {
	var calls atomic.Int32
	provider := llm.ProviderFunc(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done() // the first turn's model call never answers
			return llm.Response{}, ctx.Err()
		}
		return finalResp("second answer"), nil
	})
	_, sock := operatorHarness(t, provider)
	id, err := dial(t, sock).NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	if ran, err := dial(t, sock).CancelTurn(id); err != nil || ran {
		t.Fatalf("CancelTurn(idle) = %v, %v; want false", ran, err)
	}
	if _, err := dial(t, sock).CancelTurn("no-such-session"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("CancelTurn(unknown) err = %v, want not found", err)
	}

	type result struct {
		outcome protocol.TurnOutcome
		detail  string
	}
	first := make(chan result, 1)
	go func() {
		o, d, err := dial(t, sock).SubmitTurn(id, "first", false)
		if err != nil {
			d = err.Error()
		}
		first <- result{o, d}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ran, err := dial(t, sock).CancelTurn(id); err != nil || !ran {
		t.Fatalf("CancelTurn(running) = %v, %v; want true", ran, err)
	}
	select {
	case r := <-first:
		if r.outcome != protocol.TurnFailed || r.detail != "turn cancelled" {
			t.Errorf("cancelled turn = (%v, %q), want failed with \"turn cancelled\"", r.outcome, r.detail)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cancelled turn never ended")
	}

	reply, err := dial(t, sock).Turn(id, "second")
	if err != nil || reply != "second answer" {
		t.Errorf("turn after the cancel = %q, %v; want the session to carry on", reply, err)
	}
}
