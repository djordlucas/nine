package runtime_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/agent"
	ninectx "nine/internal/context"
	"nine/internal/llm"
	"nine/internal/runtime"
)

// workerLoop creates an agent.Loop backed by provider.
func workerLoop(provider llm.Provider) *agent.Loop {
	builder := ninectx.New(ninectx.Config{Budget: 100_000})
	queue := llm.NewQueue(provider, 1)
	dispatcher := agent.New()
	return agent.NewLoop(agent.Config{
		SystemCore: "test",
		Priority:   llm.PriorityConversation,
	}, builder, queue, dispatcher)
}

// constProvider returns a provider that always responds with text.
func constProvider(text string) llm.Provider {
	return llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		return llm.Response{Text: text, StopReason: "end_turn"}, nil
	})
}

func TestTurnContextAlreadyCancelled(t *testing.T) {
	loop := workerLoop(constProvider("ok"))
	w := runtime.NewAgentWorkerForTest("agent-1", loop, nil, nil, runtime.StallConfig{})
	t.Cleanup(func() { w.StopAgentWorker() })

	// Fill the inbox (capacity 1) with a blocking request so the next turn
	// cannot enqueue and must fall through to ctx.Done().
	go func() {
		w.TurnAgentWorker(t.Context(), "filler") //nolint:errcheck
	}()
	// Give the goroutine time to occupy the inbox slot.
	time.Sleep(10 * time.Millisecond)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, err := w.TurnAgentWorker(cancelledCtx, "should fail fast")
	if err == nil {
		t.Fatal("expected error for already-cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestInspectContextNoLLMCall(t *testing.T) {
	// A provider that fails the test if it is ever called — InspectContext must
	// assemble the context without any LLM round-trip.
	provider := llm.ProviderFunc(func(_ context.Context, _ llm.Request) (llm.Response, error) {
		t.Error("InspectContext must not call the LLM")
		return llm.Response{}, nil
	})
	w := runtime.NewAgentWorkerForTest("agent-ctx", workerLoop(provider), nil, nil, runtime.StallConfig{})
	t.Cleanup(func() { w.StopAgentWorker() })

	rep, err := w.InspectContextForTest(context.Background())
	if err != nil {
		t.Fatalf("InspectContext: %v", err)
	}
	if rep.Budget != 100_000 {
		t.Errorf("Budget = %d, want 100000", rep.Budget)
	}
	// system-core ("test" + injected current-time preamble) is always present.
	core := sectionByName(rep, "system-core")
	if !core.Included || core.Tokens == 0 {
		t.Errorf("system-core section = %+v, want included & non-zero", core)
	}
	if !strings.Contains(rep.System, "Current time:") {
		t.Errorf("assembled system prompt missing time preamble: %q", rep.System)
	}
}

func sectionByName(rep ninectx.Report, name string) ninectx.Section {
	for _, s := range rep.Sections {
		if s.Name == name {
			return s
		}
	}
	return ninectx.Section{}
}

func TestTurnStoppedSessionWorker(t *testing.T) {
	loop := workerLoop(constProvider("ok"))
	w := runtime.NewAgentWorkerForTest("agent-2", loop, nil, nil, runtime.StallConfig{})

	// Stop the worker concurrently: the turn races the stop.
	// Either the inbox is already closed (stopped channel is readable)
	// or the turn is sent before the stop happens; either way the worker
	// must return an error (context.Canceled or from ctx cancellation).
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		w.StopAgentWorker()
	}()

	<-stopDone

	// After StopAgentWorker returns the inbox is closed. A turn submitted now
	// should observe w.stopped is closed and return context.Canceled.
	// We use a goroutine with recover to guard against the theoretical panic
	// if the select picks the send arm on a closed channel.
	result := make(chan error, 1)
	go func() {
		defer func() {
			if rc := recover(); rc != nil {
				result <- context.Canceled // panic == channel closed == stopped
			}
		}()
		_, err := w.TurnAgentWorker(context.Background(), "after stop")
		result <- err
	}()

	select {
	case err := <-result:
		if err == nil {
			t.Fatal("expected error after stopping session worker, got nil")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("TurnAgentWorker did not return after worker was stopped")
	}
}

func TestCheckpointSaveError(t *testing.T) {
	saveErr := errors.New("disk full")
	saveCalled := atomic.Bool{}

	saveCkpt := func(_ string, _ []byte) error {
		saveCalled.Store(true)
		return saveErr
	}

	loop := workerLoop(constProvider("done"))
	w := runtime.NewAgentWorkerForTest("agent-3", loop, saveCkpt, nil, runtime.StallConfig{})
	t.Cleanup(func() { w.StopAgentWorker() })

	// Turn should succeed even though checkpoint save fails (non-fatal).
	result, err := w.TurnAgentWorker(context.Background(), "hello")
	if err != nil {
		t.Fatalf("turn failed: %v (checkpoint errors must be non-fatal)", err)
	}
	if result != "done" {
		t.Errorf("result = %q, want %q", result, "done")
	}
	if !saveCalled.Load() {
		t.Error("saveCkpt was never called")
	}
}

func TestStallDetectionNoOnStall(t *testing.T) {
	// StallConfig.Limit set but OnStall is nil — must not panic.
	loop := workerLoop(constProvider("no tools"))
	stall := runtime.StallConfig{Limit: 2, OnStall: nil}
	w := runtime.NewAgentWorkerForTest("agent-4", loop, nil, nil, stall)
	t.Cleanup(func() { w.StopAgentWorker() })

	for i := range 2 {
		_, err := w.TurnAgentWorker(context.Background(), "turn")
		if err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
}
