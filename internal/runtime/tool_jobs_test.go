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

// openStateToolHost is openToolHost with a tool-scoped state grant wired to a
// real store, for a tool that has to observe itself across calls.
func openStateToolHost(t *testing.T, name, manifest, src string, store *memory.Store) *toolvm.Host {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".js"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h, err := toolvm.Open(ctx, toolvm.Config{
		UserDir:    dir,
		Grants:     map[string]toolvm.Grant{name: {State: &toolvm.StateGrant{Scope: toolvm.StateScopeTool}}},
		StateStore: newToolStateStore(store),
	})
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
		tools:   NewToolJobRunner(store, host, 0, 0, 0),
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
	s := &jobSweeper{store: store, waiters: NewJobWaiters(), tools: NewToolJobRunner(store, host, 3, 0, 0)}

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
	s := &jobSweeper{store: store, waiters: NewJobWaiters(), tools: NewToolJobRunner(store, host, 0, 0, 0)}
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

// The hazard a worker pool introduces that a sequential loop could not have: two
// calls of the *same* job running at once against the same cursor.
//
// runDue waits for its batch, and the sweeper's ticker drops ticks while a sweep
// is in progress, so overlap is impossible — this asserts that rather than
// trusting it. The tool records the highest concurrency it ever observes for its
// own handle.
func TestToolJobIsNeverCalledTwiceAtOnce(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	// The tool records overlap in its own durable state: increment on entry,
	// decrement on exit, remember the peak.
	host := openStateToolHost(t, "overlap", `
name = "overlap"
kind = "js"
entrypoint = "./overlap.js"
description = "Detect concurrent calls of one job."
resumable = true

[capabilities]
state = true
`, `
import { again } from "nine:job";
import { get, set } from "nine:state";
export default function (args, job) {
  const live = Number(get("live") ?? 0) + 1;
  set("live", String(live));
  const peak = Math.max(live, Number(get("peak") ?? 0));
  set("peak", String(peak));
  // Burn a little wall clock so an overlapping call would be observed.
  const until = Date.now() + 30;
  while (Date.now() < until) { /* spin */ }
  set("live", String(live - 1));
  if (job.call >= 4) return "peak=" + peak;
  return again({ cursor: String(job.call), afterMs: 0 });
}
`, store)

	s := &jobSweeper{store: store, waiters: NewJobWaiters(),
		tools: NewToolJobRunner(store, host, 0, 0, 8)}

	js := newJobStarter(store, nil, "agent-1", 0)
	if _, err := js.StartToolJob(context.Background(), "overlap",
		json.RawMessage(`{}`), &toolvm.Continuation{Cursor: "0"}); err != nil {
		t.Fatal(err)
	}
	jobs, _ := store.JobsResumable()
	handle := jobs[0].Handle

	for range 8 {
		j, _, _ := store.JobGet(handle)
		if memory.JobTerminal(j.State) {
			break
		}
		if _, err := store.JobAdvance(handle, j.Cursor, j.Progress, time.Now()); err != nil {
			t.Fatal(err)
		}
		s.tools.runDue(context.Background(), s)
	}

	final, _, _ := store.JobGet(handle)
	if final.State != "done" {
		t.Fatalf("job ended %q: %s", final.State, final.Error)
	}
	if final.Output != "peak=1" {
		t.Fatalf("output = %q; two calls of one job overlapped", final.Output)
	}
}

// The pool runs distinct jobs concurrently — the point of having one.
func TestToolJobsRunConcurrentlyAcrossJobs(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := openToolHost(t, "slow", `
name = "slow"
kind = "js"
entrypoint = "./slow.js"
description = "Take a moment, then finish."
resumable = true
`, `
export default function () {
  const until = Date.now() + 60;
  while (Date.now() < until) { /* spin */ }
  return "done";
}
`)
	s := &jobSweeper{store: store, waiters: NewJobWaiters(),
		tools: NewToolJobRunner(store, host, 0, 0, 4)}

	js := newJobStarter(store, nil, "agent-1", 0)
	for range 4 {
		if _, err := js.StartToolJob(context.Background(), "slow",
			json.RawMessage(`{}`), &toolvm.Continuation{Cursor: "0"}); err != nil {
			t.Fatal(err)
		}
	}
	jobs, _ := store.JobsResumable()
	if len(jobs) != 4 {
		t.Fatalf("expected 4 jobs, got %d", len(jobs))
	}
	for _, j := range jobs {
		if _, err := store.JobAdvance(j.Handle, "", "", time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	s.tools.runDue(context.Background(), s)
	elapsed := time.Since(start)

	for _, j := range jobs {
		got, _, _ := store.JobGet(j.Handle)
		if got.State != "done" {
			t.Fatalf("job %s ended %q: %s", j.Handle, got.State, got.Error)
		}
	}
	// Four ~60ms calls sequentially would be ~240ms. A generous bound: this is
	// asserting "concurrent", not a precise speedup.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("four jobs took %v with 4 workers; they ran sequentially", elapsed)
	}
}

// The daemon-wide cap bounds the machine where the per-conversation cap bounds
// one agent. It refuses rather than admitting-and-cancelling, because the daemon
// decides when a tool job's first call happens.
func TestDaemonWideJobCapRefuses(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	// Two conversations, each under the per-conversation cap, together over the
	// daemon-wide one.
	for i, owner := range []string{"agent-a", "agent-b"} {
		js := newJobStarterWithTotal(store, nil, owner, 8, 3)
		for n := range 2 {
			out, err := js.StartToolJob(context.Background(), "t",
				json.RawMessage(`{}`), &toolvm.Continuation{Cursor: "0"})
			if err != nil {
				t.Fatal(err)
			}
			// The 4th job overall crosses the total of 3.
			last := i == 1 && n == 1
			if last && !strings.Contains(out, "daemon-wide maximum") {
				t.Fatalf("job %d/%s was admitted past the daemon-wide cap: %q", n, owner, out)
			}
			if !last && strings.Contains(out, "maximum") {
				t.Fatalf("job %d/%s was refused early: %q", n, owner, out)
			}
		}
	}
	n, err := store.JobsCountRunning()
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("%d jobs were recorded, want the cap of 3", n)
	}
}
