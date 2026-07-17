package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

func openHITL(t *testing.T, timeout time.Duration) (*runtime.HITL, *memory.Store) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return runtime.NewHITL(store, timeout), store
}

func TestHITLAskAnswered(t *testing.T) {
	h, _ := openHITL(t, time.Minute)

	// Hand emitted messages to the test goroutine over a channel so the read
	// synchronizes with the Ask-goroutine write (no shared variable to race).
	emitCh := make(chan protocol.Msg, 8)
	h.SetEmit(func(_ string, msg protocol.Msg) { emitCh <- msg })

	answer := make(chan struct {
		s   string
		err error
	}, 1)
	go func() {
		s, err := h.Ask(context.Background(), "agent-1", "Proceed?", []string{"yes", "no"})
		answer <- struct {
			s   string
			err error
		}{s, err}
	}()

	// Wait for the question to be emitted, then deliver the answer by request ID.
	var emitted protocol.Msg
	deadline := time.After(2 * time.Second)
	for emitted.Type != "human_input_required" {
		select {
		case emitted = <-emitCh:
		case <-deadline:
			t.Fatal("question never emitted")
		}
	}
	reqID := emitted.RequestID
	if emitted.Question != "Proceed?" || len(emitted.Options) != 2 {
		t.Errorf("emitted = %+v", emitted)
	}

	if !h.Answer(reqID, "yes") {
		t.Fatal("Answer found no waiter")
	}
	res := <-answer
	if res.err != nil {
		t.Fatalf("Ask err: %v", res.err)
	}
	if res.s != "yes" {
		t.Errorf("answer = %q, want yes", res.s)
	}
}

func TestHITLAskTimeout(t *testing.T) {
	h, store := openHITL(t, 20*time.Millisecond)
	h.SetEmit(func(_ string, _ protocol.Msg) {})

	_, err := h.Ask(context.Background(), "agent-1", "Proceed?", nil)
	if err == nil {
		t.Fatal("Ask returned nil err, want timeout")
	}
	// Row is marked timed_out — no longer pending.
	if got, _ := store.HumanRequestGetPending("agent-1"); got != nil {
		t.Errorf("timed-out row still pending: %+v", got)
	}
}

func TestHITLAskCancelled(t *testing.T) {
	h, _ := openHITL(t, time.Minute)
	h.SetEmit(func(_ string, _ protocol.Msg) {})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.Ask(ctx, "agent-1", "Proceed?", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestHITLAnswerNoWaiter(t *testing.T) {
	h, _ := openHITL(t, time.Minute)
	if h.Answer("missing", "x") {
		t.Error("Answer reported a waiter for an unknown request")
	}
}

func TestHITLInteractiveTracking(t *testing.T) {
	h, _ := openHITL(t, time.Minute)
	if ok, _ := h.IsInteractive("a"); ok {
		t.Error("unexpected interactive session")
	}
	if err := h.MarkInteractive("a"); err != nil {
		t.Fatalf("MarkInteractive: %v", err)
	}
	if ok, _ := h.IsInteractive("a"); !ok {
		t.Error("interactive session not recorded")
	}
}
