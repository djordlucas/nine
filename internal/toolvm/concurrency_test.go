package toolvm

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

// The bound exists because memory_mb is per call and nothing multiplied it: the
// number of simultaneous instances was whatever the turns in flight asked for.
func TestConcurrentCallsAreBounded(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir(), MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck

	first, err := h.acquire(ctx, "a")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := h.acquire(ctx, "b")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	// Both slots are held, so a third caller waits rather than instantiating.
	waiting := make(chan error, 1)
	go func() {
		release, err := h.acquire(ctx, "c")
		if err == nil {
			release()
		}
		waiting <- err
	}()

	select {
	case err := <-waiting:
		t.Fatalf("a third call ran while both slots were held (err = %v)", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Releasing one lets exactly one waiter through.
	first()
	select {
	case err := <-waiting:
		if err != nil {
			t.Errorf("the waiter failed once a slot freed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("releasing a slot did not admit the waiting call")
	}
	second()
}

// A queued call must not wait forever, and the failure has to say it was
// queueing — a tool that never got a slot looks exactly like a tool that is slow
// from every other vantage point.
func TestAQueuedCallGivesUpWithTheCallerContext(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir(), MaxConcurrent: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck

	release, err := h.acquire(ctx, "holder")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	deadline, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := h.acquire(deadline, "queued"); err == nil {
		t.Fatal("a queued call was admitted while the only slot was held")
	} else if !strings.Contains(err.Error(), "max_concurrent") {
		t.Errorf("error = %q, want it to name the bound the operator can raise", err)
	}
}

// The regression that would be worst: a slot not given back wedges the host for
// every later call, and would show up as tools mysteriously hanging rather than
// as anything pointing here.
func TestSlotsAreReturnedAfterEveryCall(t *testing.T) {
	ctx := context.Background()
	h, err := Open(ctx, Config{UserDir: t.TempDir(), MaxConcurrent: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer h.Close(ctx) //nolint:errcheck
	h.LoadShipped(ctx, nil)

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Half of these fail on purpose: a slot has to come back from the
			// error paths too, not only from the successful one.
			h.Call(ctx, "time", json.RawMessage(`{}`))         //nolint:errcheck
			h.Call(ctx, "no_such_tool", json.RawMessage(`{}`)) //nolint:errcheck
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("concurrent calls wedged — a slot was not returned")
	}

	if len(h.sem) != 0 {
		t.Errorf("%d slots still held after every call returned", len(h.sem))
	}
}
