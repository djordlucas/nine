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

// The per-conversation cap is enforced on admission: an over-cap start writes no
// row and tells the model, rather than accumulating jobs.
func TestJobStarterAdmissionCap(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "a", Plugin: "p", Tool: "t", PluginJobID: "1", OwnerID: "conv1", State: "running"})
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "b", Plugin: "p", Tool: "t", PluginJobID: "2", OwnerID: "conv1", State: "running"})

	js := newJobStarter(store, plugin.NewManager(""), "conv1", 2)
	out, err := js.StartJob(context.Background(), "p", "t", "3", "started")
	if err != nil {
		t.Fatalf("over-cap start should not error: %v", err)
	}
	if !strings.Contains(out, "maximum") {
		t.Errorf("over-cap message = %q, want it to mention the maximum", out)
	}
	// No new row was written.
	if n, _ := store.PluginJobCountOutstanding("conv1"); n != 2 {
		t.Errorf("outstanding = %d, want it to stay at 2 (the new job declined)", n)
	}

	// Under the cap, a start records a row and returns a handle observation.
	_ = store.PluginJobFinish("a", "done", "", "", "")
	out, err = js.StartJob(context.Background(), "p", "t", "4", "started")
	if err != nil || !strings.Contains(out, "job_") {
		t.Errorf("under-cap start: out=%q err=%v", out, err)
	}
}

// At boot every still-running row is marked lost and its owner notified.
func TestMarkOrphanedJobsLost(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "orphan", Plugin: "p", Tool: "scan", PluginJobID: "1", OwnerID: "conv1", State: "running"})

	if n := MarkOrphanedJobsLost(store); n != 1 {
		t.Fatalf("marked %d lost, want 1", n)
	}
	if got, _, _ := store.PluginJobGet("orphan"); got.State != "lost" {
		t.Errorf("state = %q, want lost", got.State)
	}
	ns, _ := store.NotificationListPending("conv1")
	if len(ns) != 1 || !strings.Contains(ns[0].Message, "orphan") {
		t.Errorf("owner not notified of the lost job: %+v", ns)
	}
}

// The sweeper fails a job past its lifetime bound and notifies the owner.
func TestSweeperExpiresOverAge(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	_ = store.PluginJobCreate(memory.PluginJob{Handle: "old", Plugin: "ghost", Tool: "t", PluginJobID: "1", OwnerID: "conv1", State: "running"})

	// Let it age past a 1-second bound.
	time.Sleep(1100 * time.Millisecond)

	sw := &jobSweeper{store: store, mgr: plugin.NewManager(""), maxSeconds: 1}
	sw.expireOverAge(context.Background())

	got, _, _ := store.PluginJobGet("old")
	if got.State != "failed" || got.Error == "" {
		t.Errorf("over-age job = %+v, want failed with a reason", got)
	}
	ns, _ := store.NotificationListPending("conv1")
	if len(ns) != 1 || !strings.Contains(ns[0].Message, "time limit") {
		t.Errorf("owner not notified of expiry: %+v", ns)
	}
}
