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

// The interaction between durable state and long-running work, which is where
// the two features meet and where each is individually correct.
//
// A conversation-scoped `state` grant must resolve the same way on call 40 as on
// call 1. Call 1 happens inside the turn that started the job, where the
// conversation is on the context; every later call happens in the sweeper, which
// has no turn. Without the owner being carried across, such a tool works exactly
// once and then dies — the failure this test exists to prevent.
func TestConversationScopedStateSurvivesLaterJobCalls(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acc.toml"), []byte(`
name = "acc"
kind = "js"
entrypoint = "./acc.js"
description = "Count across calls in conversation-scoped state."
resumable = true

[capabilities]
state = true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acc.js"), []byte(`
import { again } from "nine:job";
import { get, set } from "nine:state";
export default function () {
  const n = Number(get("n") ?? 0) + 1;
  set("n", String(n));
  if (n >= 3) return "counted to " + n;
  return again({ cursor: String(n), afterMs: 0 });
}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	host, err := toolvm.Open(ctx, toolvm.Config{
		UserDir:    dir,
		Grants:     map[string]toolvm.Grant{"acc": {State: &toolvm.StateGrant{Scope: toolvm.StateScopeConversation}}},
		StateStore: newToolStateStore(store),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { host.Close(context.Background()) }) //nolint:errcheck
	host.Load(ctx, nil)
	if host.Get("acc") == nil {
		t.Fatalf("tool did not load: %+v", host.Status())
	}

	// Call 1: inside the turn, exactly as the dispatcher would run it.
	turnCtx := toolvm.WithStateScope(ctx, "agent-1")
	out, err := host.CallOutput(turnCtx, "acc", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("in-turn call: %v", err)
	}
	if out.Continue == nil {
		t.Fatal("the tool did not ask to continue")
	}

	js := newJobStarter(store, nil, "agent-1", 0)
	if _, err := js.StartToolJob(turnCtx, "acc", json.RawMessage(`{}`), out.Continue); err != nil {
		t.Fatal(err)
	}
	jobs, _ := store.JobsResumable()
	handle := jobs[0].Handle

	// Calls 2+: the sweeper, with no turn of its own.
	s := &jobSweeper{store: store, waiters: NewJobWaiters(), tools: NewToolJobRunner(store, host, 0, 0)}
	for range 6 {
		j, _, err := store.JobGet(handle)
		if err != nil {
			t.Fatal(err)
		}
		if memory.JobTerminal(j.State) {
			break
		}
		if _, err := store.JobAdvance(handle, j.Cursor, j.Progress, time.Now()); err != nil {
			t.Fatal(err)
		}
		s.tools.runDue(ctx, s)
	}

	final, _, _ := store.JobGet(handle)
	if final.State == "failed" {
		t.Fatalf("the job died after the turn ended: %s", final.Error)
	}
	if final.State != "done" {
		t.Fatalf("state = %q, want done", final.State)
	}
	if !strings.Contains(final.Output, "counted to 3") {
		t.Fatalf("output = %q; the count did not accumulate across calls", final.Output)
	}
}

// Args and Ack are separate fields. Ack is the one line a human reads; Args is
// machine input. Conflating them means anything that renders Ack — a reasonable
// thing to do — dumps raw JSON at the model.
func TestToolJobKeepsArgsOutOfTheAck(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	js := newJobStarter(store, nil, "agent-1", 0)
	if _, err := js.StartToolJob(context.Background(), "batcher",
		json.RawMessage(`{"total":9}`),
		&toolvm.Continuation{Cursor: "0", Progress: "0/9"}); err != nil {
		t.Fatal(err)
	}

	jobs, _ := store.JobsResumable()
	j := jobs[0]
	if strings.Contains(j.Ack, "{") {
		t.Errorf("ack = %q, want a human-readable line rather than arguments", j.Ack)
	}
	if j.Args != `{"total":9}` {
		t.Errorf("args = %q, want the model's original arguments", j.Args)
	}
}
