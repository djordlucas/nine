package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/plugin"
)

func newJobTest(t *testing.T) (*memory.Store, *jobTools) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	return store, newJobTools(store, plugin.NewManager(""), nil, "conv1")
}

func TestJobCheckStates(t *testing.T) {
	store, jt := newJobTest(t)
	ctx := context.Background()

	if out, _ := jt.Check(ctx, "job_missing"); !strings.Contains(out, "no background job") {
		t.Errorf("unknown handle: %q", out)
	}

	_ = store.PluginJobCreate(memory.PluginJob{Handle: "job_r", Plugin: "p", Tool: "scan", PluginJobID: "1", OwnerID: "conv1", State: "running"})
	_ = store.PluginJobUpdateLive("job_r", "running", "42%")
	if out, _ := jt.Check(ctx, "job_r"); !strings.Contains(out, "still running") || !strings.Contains(out, "42%") {
		t.Errorf("running render: %q", out)
	}

	_ = store.PluginJobFinish("job_r", "done", "the answer", "", "")
	if out, _ := jt.Check(ctx, "job_r"); !strings.Contains(out, "finished") || !strings.Contains(out, "the answer") {
		t.Errorf("done render: %q", out)
	}
}

func TestJobWaitReturnsOnTerminalAndTimesOutGracefully(t *testing.T) {
	store, jt := newJobTest(t)
	ctx := context.Background()

	_ = store.PluginJobCreate(memory.PluginJob{Handle: "job_w", Plugin: "p", Tool: "t", PluginJobID: "1", OwnerID: "conv1", State: "running"})

	// A short wait on a still-running job returns its progress, not an error.
	out, err := jt.Wait(ctx, "job_w", 50*time.Millisecond)
	if err != nil {
		t.Fatalf("wait timeout should not error: %v", err)
	}
	if !strings.Contains(out, "still running") {
		t.Errorf("timed-out wait = %q, want still-running", out)
	}

	// Once terminal, wait returns the result immediately.
	_ = store.PluginJobFinish("job_w", "done", "done result", "", "")
	out, err = jt.Wait(ctx, "job_w", 5*time.Second)
	if err != nil || !strings.Contains(out, "done result") {
		t.Errorf("wait on terminal: out=%q err=%v", out, err)
	}
}

func TestJobListAndSurface(t *testing.T) {
	store, jt := newJobTest(t)

	if out, _ := jt.List(context.Background()); !strings.Contains(out, "No outstanding") {
		t.Errorf("empty list: %q", out)
	}

	_ = store.PluginJobCreate(memory.PluginJob{Handle: "job_a", Plugin: "p", Tool: "download", PluginJobID: "1", OwnerID: "conv1", State: "running"})
	out, _ := jt.List(context.Background())
	if !strings.Contains(out, "job_a") || !strings.Contains(out, "download") {
		t.Errorf("list: %q", out)
	}

	// The context surfacing shows the same outstanding job, and nothing when none.
	surf := jobsEnrichmentFn(store, "conv1")(context.Background(), nil)
	if !strings.Contains(surf, "job_a") {
		t.Errorf("surface: %q", surf)
	}
	if empty := jobsEnrichmentFn(store, "other")(context.Background(), nil); empty != "" {
		t.Errorf("surface for owner with no jobs = %q, want empty", empty)
	}
}

// With a waiter registry wired, job_wait wakes on the sweeper's signal rather
// than a poll tick — so it returns well under the poll interval.
func TestJobWaitWakesOnSignal(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	waiters := NewJobWaiters()
	jt := newJobTools(store, plugin.NewManager(""), waiters, "conv1")

	_ = store.PluginJobCreate(memory.PluginJob{Handle: "job_s", Plugin: "p", Tool: "t", PluginJobID: "1", OwnerID: "conv1", State: "running"})

	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = store.PluginJobFinish("job_s", "done", "the result", "", "")
		waiters.signal("job_s")
	}()

	start := time.Now()
	out, err := jt.Wait(context.Background(), "job_s", 5*time.Second)
	elapsed := time.Since(start)
	if err != nil || !strings.Contains(out, "the result") {
		t.Fatalf("wait: out=%q err=%v", out, err)
	}
	if elapsed >= jobWaitPoll {
		t.Errorf("wait took %s (>= poll interval %s); it should have woken on the signal", elapsed, jobWaitPoll)
	}
}

func TestJobWaitersSignalAndCancel(t *testing.T) {
	w := NewJobWaiters()
	ch, cancel := w.register("h")

	select {
	case <-ch:
		t.Fatal("channel closed before signal")
	default:
	}
	w.signal("h")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("signal did not close the waiter")
	}

	// A second waiter that is cancelled is simply removed; signalling is a no-op.
	_, cancel2 := w.register("h")
	cancel2()
	w.signal("h") // must not panic on the removed waiter
	cancel()      // cancelling an already-signalled waiter is safe
}

func TestJobCancelBranches(t *testing.T) {
	store, jt := newJobTest(t)
	ctx := context.Background()

	if out, _ := jt.Cancel(ctx, "nope"); !strings.Contains(out, "no background job") {
		t.Errorf("cancel unknown: %q", out)
	}

	_ = store.PluginJobCreate(memory.PluginJob{Handle: "job_c", Plugin: "ghost", Tool: "t", PluginJobID: "1", OwnerID: "conv1", State: "running"})
	// Plugin is not running in this bare manager → cannot cancel, but not an error.
	if out, err := jt.Cancel(ctx, "job_c"); err != nil || !strings.Contains(out, "no longer running") {
		t.Errorf("cancel plugin-gone: out=%q err=%v", out, err)
	}

	_ = store.PluginJobFinish("job_c", "done", "", "", "")
	if out, _ := jt.Cancel(ctx, "job_c"); !strings.Contains(out, "already finished") {
		t.Errorf("cancel terminal: %q", out)
	}
}
