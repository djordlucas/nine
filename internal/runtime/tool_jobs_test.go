package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/toolvm"
)

// writeResumableTool drops a resumable tool into dir and opens a host over it.
func openToolHost(t *testing.T, name, manifest, src string) *toolvm.Host {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".js"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := toolvm.Open(ctx, toolvm.Config{UserDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.Load(ctx, nil)
	if h.Get(name) == nil {
		t.Fatalf("tool %q did not load: %+v", name, h.Status())
	}
	return h
}

const batcherManifest = `
name = "batcher"
kind = "js"
entrypoint = "./batcher.js"
description = "Count down in bounded batches."
resumable = true
`

const batcherSrc = `
import { again } from "nine:job";
export default function (args, job) {
  const n = Number(job?.cursor ?? args.from);
  if (n <= 0) return "reached zero on call " + job.call;
  return again({ cursor: String(n - 1), progress: n + " left", afterMs: 1 });
}
`

// newTestSweeper wires a sweeper with only the tool backend — no plugin manager,
// which is itself worth covering: a deployment can run sandboxed tools without
// plugins at all.
func newTestSweeper(store *memory.Store, host *toolvm.Host) *jobSweeper {
	return &jobSweeper{
		store:   store,
		waiters: NewJobWaiters(),
		tools:   NewToolJobRunner(store, host, 0, 0),
	}
}

func startToolJob(t *testing.T, store *memory.Store, tool, args, cursor string) string {
	t.Helper()
	js := newJobStarter(store, nil, "agent-1", 0)
	out, err := js.StartToolJob(context.Background(), tool, json.RawMessage(args),
		&toolvm.Continuation{Cursor: cursor, Progress: "starting", AfterMS: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "job_") {
		t.Fatalf("StartToolJob returned %q, want an observation naming a handle", out)
	}
	jobs, err := store.JobsResumable()
	if err != nil || len(jobs) != 1 {
		t.Fatalf("JobsResumable = %d jobs, err %v; want the one just started", len(jobs), err)
	}
	return jobs[0].Handle
}

// The driver's central claim: repeated calls carry the cursor forward until the
// tool returns a result, and the job then completes with that result.
func TestToolJobRunsToCompletion(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := openToolHost(t, "batcher", batcherManifest, batcherSrc)
	s := newTestSweeper(store, host)
	handle := startToolJob(t, store, "batcher", `{"from":3}`, "3")

	// Each sweep makes at most one call per job, so this loop is also asserting
	// that progress is per-tick rather than a hidden inner loop.
	for range 10 {
		s.tools.runDue(context.Background(), s)
		j, _, err := store.JobGet(handle)
		if err != nil {
			t.Fatal(err)
		}
		if memory.JobTerminal(j.State) {
			if j.State != "done" {
				t.Fatalf("job ended %q: %s", j.State, j.Error)
			}
			if !strings.Contains(j.Output, "reached zero") {
				t.Fatalf("output = %q, want the tool's final result", j.Output)
			}
			return
		}
		// next_at is in the future by the delay floor, so let it come due.
		time.Sleep(2 * time.Millisecond)
		if _, err := store.JobAdvance(handle, j.Cursor, j.Progress, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the job never finished")
}

// Completion posts a notification to the owner, which is how a result reaches a
// turn that never waited for it. Delivery stays pull-only: nothing is woken.
func TestToolJobNotifiesItsOwnerOnCompletion(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := openToolHost(t, "batcher", batcherManifest, batcherSrc)
	s := newTestSweeper(store, host)
	handle := startToolJob(t, store, "batcher", `{"from":0}`, "0")

	if _, err := store.JobAdvance(handle, "0", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	s.tools.runDue(context.Background(), s)

	notes, err := store.NotificationListPending("agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) == 0 {
		t.Fatal("a finished tool job left no notification for its owner")
	}
	if !strings.Contains(notes[0].Message, handle) {
		t.Fatalf("notification %q does not name the handle %s", notes[0].Message, handle)
	}
}

// A tool that always asks to continue must not run forever. The cap is checked
// before a call is spent, so the job fails without one last call.
func TestToolJobStopsAtTheCallCap(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := openToolHost(t, "forever", `
name = "forever"
kind = "js"
entrypoint = "./forever.js"
description = "Never finish."
resumable = true
`, `
import { again } from "nine:job";
export default function (args, job) { return again({ cursor: String(job.call), afterMs: 0 }); }
`)
	s := &jobSweeper{store: store, waiters: NewJobWaiters(), tools: NewToolJobRunner(store, host, 3, 0)}

	js := newJobStarter(store, nil, "agent-1", 0)
	if _, err := js.StartToolJob(context.Background(), "forever",
		json.RawMessage(`{}`), &toolvm.Continuation{Cursor: "0"}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := store.JobsResumable()
	handle := jobs[0].Handle

	for range 12 {
		if _, err := store.JobAdvance(handle, "", "", time.Now()); err != nil {
			break
		}
		s.tools.runDue(context.Background(), s)
	}

	j, _, err := store.JobGet(handle)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != "failed" {
		t.Fatalf("state = %q, want failed — an unbounded tool ran past its cap", j.State)
	}
	if !strings.Contains(j.Error, "job_max_calls") {
		t.Fatalf("error = %q, want it to name the bound that stopped it", j.Error)
	}
}

// Cancelling is exact: the row goes terminal and no further call is made. A call
// already in flight cannot resurrect it, which JobAdvance enforces.
func TestCancelledToolJobIsNotCalledAgain(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := openToolHost(t, "batcher", batcherManifest, batcherSrc)
	s := newTestSweeper(store, host)
	handle := startToolJob(t, store, "batcher", `{"from":5}`, "5")

	jt := newJobTools(store, nil, s.waiters, "agent-1")
	msg, err := jt.Cancel(context.Background(), handle)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "Cancelled") {
		t.Fatalf("cancel said %q", msg)
	}

	before, _, _ := store.JobGet(handle)
	s.tools.runDue(context.Background(), s)
	after, _, _ := store.JobGet(handle)

	if after.Calls != before.Calls {
		t.Fatalf("a cancelled job was called again (%d -> %d)", before.Calls, after.Calls)
	}
	if after.State != "cancelled" {
		t.Fatalf("state = %q, want cancelled", after.State)
	}
}

// The behaviour a plugin job cannot have: a tool job outlives the daemon,
// because its entire live state is the row.
func TestToolJobsAreResumedNotLostAtBoot(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.JobCreate(memory.Job{
		Handle: "job_tool", Backend: memory.JobBackendTool, Tool: "batcher",
		OwnerID: "agent-1", State: "running", Ack: `{"from":2}`, Cursor: "2",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.JobCreate(memory.Job{
		Handle: "job_plug", Backend: memory.JobBackendPlugin, Plugin: "p", Tool: "download",
		OwnerID: "agent-1", State: "running", BackendRef: "pj-1",
	}); err != nil {
		t.Fatal(err)
	}

	MarkOrphanedJobsLost(store)

	tool, _, _ := store.JobGet("job_tool")
	if tool.State != "running" {
		t.Errorf("tool job was marked %q at boot; it should resume", tool.State)
	}
	plug, _, _ := store.JobGet("job_plug")
	if plug.State != "lost" {
		t.Errorf("plugin job state = %q, want lost — its process is gone", plug.State)
	}
	if n := ResumeToolJobs(store); n != 1 {
		t.Errorf("ResumeToolJobs reported %d, want the 1 tool job", n)
	}
}
