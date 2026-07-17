package llm_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/llm"
)

func TestQueueMaxConcurrent1(t *testing.T) {
	var inflight, maxSeen atomic.Int64

	prov := llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			old := maxSeen.Load()
			if n <= old || maxSeen.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		return llm.Response{Text: "ok"}, nil
	})

	q := llm.NewQueue(prov, 1)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.Submit(context.Background(), llm.PriorityBackground, llm.Request{}) //nolint:errcheck
		}()
	}
	wg.Wait()

	if got := maxSeen.Load(); got != 1 {
		t.Errorf("max concurrent = %d, want 1", got)
	}
}

func TestQueueMaxConcurrent3(t *testing.T) {
	var inflight, maxSeen atomic.Int64
	release := make(chan struct{})

	prov := llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			old := maxSeen.Load()
			if n <= old || maxSeen.CompareAndSwap(old, n) {
				break
			}
		}
		<-release
		return llm.Response{Text: "ok"}, nil
	})

	q := llm.NewQueue(prov, 3)
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.Submit(context.Background(), llm.PriorityBackground, llm.Request{}) //nolint:errcheck
		}()
	}

	// Give the first 3 time to fill their slots.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := maxSeen.Load(); got < 3 {
		t.Errorf("max concurrent = %d, want >= 3", got)
	}
	if got := maxSeen.Load(); got > 3 {
		t.Errorf("max concurrent = %d, exceeded limit of 3", got)
	}
}

func TestQueuePriority(t *testing.T) {
	// With max_concurrent=1:
	//   1. Request 0 (p3) starts immediately and blocks inside the provider.
	//   2. Requests 1–4 (p3) and request 99 (p1) are queued while 0 is running.
	//   3. When 0 finishes the slot opens — 99 (p1) must run next.

	var (
		mu    sync.Mutex
		order []int
	)

	callN := 0
	firstStarted := make(chan struct{})
	firstUnblock := make(chan struct{})

	prov := llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		mu.Lock()
		isFirst := callN == 0
		callN++
		mu.Unlock()

		if isFirst {
			close(firstStarted) // signal that slot is occupied
			<-firstUnblock      // wait to be released
		}

		mu.Lock()
		order = append(order, req.MaxTokens)
		mu.Unlock()
		return llm.Response{Text: "ok"}, nil
	})

	q := llm.NewQueue(prov, 1)
	var wg sync.WaitGroup

	// Request 0 fills the slot.
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Submit(context.Background(), llm.PriorityBackground, llm.Request{MaxTokens: 0}) //nolint:errcheck
	}()
	<-firstStarted // slot is occupied; heap is empty

	// Enqueue 4 low-priority and 1 high-priority request.
	for i := 1; i <= 4; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.Submit(context.Background(), llm.PriorityBackground, llm.Request{MaxTokens: i}) //nolint:errcheck
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Submit(context.Background(), llm.PrioritySupervisor, llm.Request{MaxTokens: 99}) //nolint:errcheck
	}()

	// Allow goroutines to block inside Submit (add themselves to the heap).
	time.Sleep(30 * time.Millisecond)

	// Release request 0 → slot opens → highest-priority pending request runs.
	close(firstUnblock)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 6 {
		t.Fatalf("expected 6 executions, got %d: %v", len(order), order)
	}
	if order[0] != 0 {
		t.Errorf("first = %d, want 0", order[0])
	}
	if order[1] != 99 {
		t.Errorf("second = %d, want 99 (priority-1 request)\nfull order: %v", order[1], order)
	}
}

func TestQueueContextCancellation(t *testing.T) {
	// A cancelled context must cause Submit to return immediately.
	block := make(chan struct{})
	prov := llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		<-block
		return llm.Response{Text: "ok"}, nil
	})

	q := llm.NewQueue(prov, 1)

	// Fill the slot.
	go q.Submit(context.Background(), llm.PriorityBackground, llm.Request{}) //nolint:errcheck
	time.Sleep(10 * time.Millisecond)

	// Submit with an already-cancelled context — must return promptly.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := q.Submit(ctx, llm.PriorityBackground, llm.Request{})
	if time.Since(start) > 500*time.Millisecond {
		t.Error("Submit with cancelled ctx took too long")
	}
	if err == nil {
		t.Error("expected error for cancelled ctx, got nil")
	}
	close(block)
}

type fakeThinkingAwareProvider struct {
	supportsThinking bool
}

func (p *fakeThinkingAwareProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{}, nil
}

func (p *fakeThinkingAwareProvider) SupportsThinking(ctx context.Context) bool {
	return p.supportsThinking
}

func TestQueueSupportsThinking(t *testing.T) {
	// test case 1 - supports thinking
	provWithThinking := &fakeThinkingAwareProvider{supportsThinking: true}
	q := llm.NewQueue(provWithThinking, 1)

	if !q.SupportsThinking(context.Background()) {
		t.Error("expected true, got false")
	}

	// test case 2 - does not support thinking
	provWithoutThinking := &fakeThinkingAwareProvider{supportsThinking: false}
	q2 := llm.NewQueue(provWithoutThinking, 1)

	if q2.SupportsThinking(context.Background()) {
		t.Error("expected false, got true")
	}

	// test case 3 - provider does not implement ThinkingAware
	provNotThinkingAware := llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		return llm.Response{}, nil
	})
	q3 := llm.NewQueue(provNotThinkingAware, 1)

	if q3.SupportsThinking(context.Background()) {
		t.Error("expected false, got true")
	}
}

func TestQueueThinkingUnsupported(t *testing.T) {
	// Aware + supports → not unsupported.
	q := llm.NewQueue(&fakeThinkingAwareProvider{supportsThinking: true}, 1)
	if q.ThinkingUnsupported(context.Background()) {
		t.Error("aware+supports: ThinkingUnsupported = true, want false")
	}

	// Aware + does not support → known unsupported (the analysis-pass trigger).
	q2 := llm.NewQueue(&fakeThinkingAwareProvider{supportsThinking: false}, 1)
	if !q2.ThinkingUnsupported(context.Background()) {
		t.Error("aware+unsupported: ThinkingUnsupported = false, want true")
	}

	// Not ThinkingAware → unknown, NOT unsupported (must not trigger the fallback).
	q3 := llm.NewQueue(llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		return llm.Response{}, nil
	}), 1)
	if q3.ThinkingUnsupported(context.Background()) {
		t.Error("not-aware: ThinkingUnsupported = true, want false (unknown ≠ unsupported)")
	}
}

func TestQueueWaitCallbacks(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	prov := llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return llm.Response{Text: "ok"}, nil
	})
	q := llm.NewQueue(prov, 1)

	// First request takes the only slot: it never waits, so it sees neither callback.
	var firstQueued, firstDequeued atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Submit(context.Background(), llm.PriorityConversation, llm.Request{ //nolint:errcheck
			OnQueued:   func() { firstQueued.Add(1) },
			OnDequeued: func() { firstDequeued.Add(1) },
		})
	}()
	<-started // the slot is taken

	// Second request has to park, so it sees OnQueued now and OnDequeued once
	// the first finishes.
	queued := make(chan struct{})
	dequeued := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Submit(context.Background(), llm.PriorityConversation, llm.Request{ //nolint:errcheck
			OnQueued:   func() { close(queued) },
			OnDequeued: func() { close(dequeued) },
		})
	}()

	select {
	case <-queued:
	case <-time.After(time.Second):
		t.Fatal("parked request: OnQueued never fired")
	}
	select {
	case <-dequeued:
		t.Fatal("OnDequeued fired while the slot was still busy")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case <-dequeued:
	case <-time.After(time.Second):
		t.Fatal("parked request: OnDequeued never fired after a slot opened")
	}
	wg.Wait()

	if got := firstQueued.Load(); got != 0 {
		t.Errorf("request that ran immediately: OnQueued fired %d times, want 0", got)
	}
	if got := firstDequeued.Load(); got != 0 {
		t.Errorf("request that ran immediately: OnDequeued fired %d times, want 0", got)
	}
}

func TestQueueNoDequeueCallbackAfterCancel(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})

	prov := llm.ProviderFunc(func(ctx context.Context, req llm.Request) (llm.Response, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return llm.Response{Text: "ok"}, nil
	})
	q := llm.NewQueue(prov, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Submit(context.Background(), llm.PriorityConversation, llm.Request{}) //nolint:errcheck
	}()
	<-started

	// A parked request whose caller gives up must not report a queue wait later:
	// by then the callback belongs to a turn that is already over.
	ctx, cancel := context.WithCancel(context.Background())
	var dequeued atomic.Int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		q.Submit(ctx, llm.PriorityConversation, llm.Request{ //nolint:errcheck
			OnDequeued: func() { dequeued.Add(1) },
		})
	}()
	time.Sleep(20 * time.Millisecond) // let it park
	cancel()

	close(release)
	wg.Wait()
	time.Sleep(20 * time.Millisecond) // give a stray callback time to land

	if got := dequeued.Load(); got != 0 {
		t.Errorf("cancelled request: OnDequeued fired %d times, want 0", got)
	}
}

func TestQueueDepthReportsPendingAndInflight(t *testing.T) {
	release := make(chan struct{})
	var started atomic.Int64
	prov := llm.ProviderFunc(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		started.Add(1)
		<-release // hold the slot until the test releases it
		return llm.Response{Text: "ok"}, nil
	})

	q := llm.NewQueue(prov, 2)

	// Before any work: empty and reporting the configured slot limit.
	if p, inf, mx := q.Depth(); p != 0 || inf != 0 || mx != 2 {
		t.Fatalf("idle Depth = (%d,%d,%d), want (0,0,2)", p, inf, mx)
	}

	const total = 5
	var wg sync.WaitGroup
	for range total {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.Submit(context.Background(), llm.PriorityBackground, llm.Request{}) //nolint:errcheck
		}()
	}

	// Wait until both slots are filled and the rest have parked.
	deadline := time.Now().Add(2 * time.Second)
	for {
		pending, inflight, _ := q.Depth()
		if inflight == 2 && pending == total-2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue never reached steady state: last Depth = (%d,%d)", pending, inflight)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := started.Load(); got != 2 {
		t.Errorf("started = %d, want 2 (only inflight requests run)", got)
	}

	close(release)
	wg.Wait()

	// Drained.
	if p, inf, _ := q.Depth(); p != 0 || inf != 0 {
		t.Errorf("drained Depth = (%d,%d), want (0,0)", p, inf)
	}
}
