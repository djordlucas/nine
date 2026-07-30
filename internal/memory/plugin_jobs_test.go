package memory_test

import (
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func TestPluginJobLifecycle(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	job := memory.PluginJob{
		Handle: "job_abc", Plugin: "scanner", Tool: "scan", PluginJobID: "j1",
		OwnerID: "conv1", State: "running", Ack: "started scan",
	}
	if err := store.PluginJobCreate(job); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, ok, err := store.PluginJobGet("job_abc")
	if err != nil || !ok {
		t.Fatalf("get: ok=%v err=%v", ok, err)
	}
	if got.Plugin != "scanner" || got.PluginJobID != "j1" || got.Ack != "started scan" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Running list includes it; a live update refreshes state and progress.
	if err := store.PluginJobUpdateLive("job_abc", "running", "42%"); err != nil {
		t.Fatalf("update live: %v", err)
	}
	running, err := store.PluginJobsRunning()
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 1 || running[0].Progress != "42%" {
		t.Errorf("running = %+v, want one job at 42%%", running)
	}

	// Finishing drops it from the running/outstanding lists.
	if err := store.PluginJobFinish("job_abc", "done", "the result", "", ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if running, _ := store.PluginJobsRunning(); len(running) != 0 {
		t.Errorf("finished job still in running list: %+v", running)
	}
	got, _, _ = store.PluginJobGet("job_abc")
	if got.State != "done" || got.Output != "the result" || got.FinishedAt == "" {
		t.Errorf("finished row = %+v, want done with output and finished_at", got)
	}
}

func TestPluginJobFinishIsTerminalGuarded(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "j", Plugin: "p", Tool: "t", PluginJobID: "1", OwnerID: "o", State: "running"})
	if err := store.PluginJobFinish("j", "done", "first", "", ""); err != nil {
		t.Fatal(err)
	}
	// A late poll must not overwrite a terminal row.
	if err := store.PluginJobUpdateLive("j", "running", "late"); err != nil {
		t.Fatal(err)
	}
	if err := store.PluginJobFinish("j", "failed", "second", "", "boom"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.PluginJobGet("j")
	if got.State != "done" || got.Output != "first" || got.Progress == "late" {
		t.Errorf("terminal row was mutated: %+v", got)
	}
}

func TestPluginJobCountAndMarkLost(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "r1", Plugin: "p", Tool: "t", PluginJobID: "1", OwnerID: "me", State: "running"})
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "r2", Plugin: "p", Tool: "t", PluginJobID: "2", OwnerID: "me", State: "running"})
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "d1", Plugin: "p", Tool: "t", PluginJobID: "3", OwnerID: "me", State: "running"})
	_ = store.PluginJobFinish("d1", "done", "", "", "")

	if n, _ := store.PluginJobCountOutstanding("me"); n != 2 {
		t.Errorf("outstanding count = %d, want 2", n)
	}

	lost, err := store.PluginJobsMarkLost("daemon restarted")
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 2 {
		t.Fatalf("marked %d lost, want 2 (the running ones only)", len(lost))
	}
	for _, j := range lost {
		if j.State != "lost" || j.Error == "" {
			t.Errorf("lost row %s = %+v, want state lost with a reason", j.Handle, j)
		}
	}
	// The done job is untouched; nothing is outstanding anymore.
	if n, _ := store.PluginJobCountOutstanding("me"); n != 0 {
		t.Errorf("outstanding after mark-lost = %d, want 0", n)
	}
	if got, _, _ := store.PluginJobGet("d1"); got.State != "done" {
		t.Errorf("done job mutated to %q", got.State)
	}
}

func TestPluginJobsDueForPoll(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "j", Plugin: "p", Tool: "t", PluginJobID: "1", OwnerID: "me", State: "running"})

	// Just created (updated_at = now): not yet due under a 1s base cadence.
	if due, _ := store.PluginJobsDueForPoll(1, 30, 60); len(due) != 0 {
		t.Errorf("fresh job due immediately: %d rows", len(due))
	}

	time.Sleep(1100 * time.Millisecond)

	// Aged past the base cadence: now due.
	if due, _ := store.PluginJobsDueForPoll(1, 30, 60); len(due) != 1 {
		t.Errorf("aged job not due: %d rows, want 1", len(due))
	}
	// A poll refreshes updated_at, so it is not due again right away.
	_ = store.PluginJobUpdateLive("j", "running", "")
	if due, _ := store.PluginJobsDueForPoll(1, 30, 60); len(due) != 0 {
		t.Errorf("just-polled job due again immediately: %d rows", len(due))
	}
}

func TestPluginJobsExpireDisabled(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "j", Plugin: "p", Tool: "t", PluginJobID: "1", OwnerID: "me", State: "running"})
	// maxSeconds <= 0 disables expiry; a fresh job is well within any positive age.
	if rows, _ := store.PluginJobsExpire(0, "x"); len(rows) != 0 {
		t.Errorf("expire disabled returned %d rows, want 0", len(rows))
	}
	if rows, _ := store.PluginJobsExpire(3600, "x"); len(rows) != 0 {
		t.Errorf("fresh job expired under a 1h bound: %d rows", len(rows))
	}
}

func TestPluginJobsOutstandingSummary(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "a", Plugin: "p", Tool: "scan", PluginJobID: "1", OwnerID: "me", State: "running"})
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "b", Plugin: "p", Tool: "t", PluginJobID: "2", OwnerID: "me", State: "running"})
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "c", Plugin: "p", Tool: "t", PluginJobID: "3", OwnerID: "other", State: "running"})
	_ = store.PluginJobFinish("b", "done", "", "", "")

	out, err := store.PluginJobsOutstandingSummary("me")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Handle != "a" || out[0].Tool != "scan" {
		t.Errorf("outstanding for me = %+v, want only the running job a", out)
	}
	if out[0].AgeSeconds < 0 {
		t.Errorf("age = %d, want >= 0", out[0].AgeSeconds)
	}
}
