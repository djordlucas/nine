package memory_test

import (
	"testing"

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
