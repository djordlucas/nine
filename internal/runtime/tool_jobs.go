package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"nine/internal/memory"
	"nine/internal/toolvm"
)

// Bounds on a long-running tool job, overridable in `[tools]`.
const (
	// DefaultJobMaxCalls bounds how many calls one job may spend. At the default
	// delay floor that is a few minutes of continuous work, and at a one-minute
	// cadence it is half a day — the point is that the number is finite, because
	// each call is individually legal and only the total is not.
	DefaultJobMaxCalls = 720

	// DefaultJobMinDelayMS floors the delay a tool may ask for. Without it a tool
	// returning after_ms = 0 forever would occupy the sweeper indefinitely while
	// never breaking a single per-call rule.
	DefaultJobMinDelayMS = 250

	// DefaultJobWorkers bounds concurrent tool-job calls. Each one is a wasm
	// instantiation holding up to [tools] memory_mb — 16 MiB by default — so four
	// is a memory budget as much as a parallelism choice.
	DefaultJobWorkers = 4
)

// ToolJobRunner is the tool backend of the job registry.
//
// The asymmetry with the plugin backend is the whole of it: for a plugin the
// sweeper POLLS work the plugin is already doing, and for a tool the sweeper IS
// the executor. That has three consequences worth stating, because each is a
// behaviour difference an operator will notice:
//
//   - The delay is the tool's request, not a poll backoff. Backing a tool job off
//     as it ages would just make it slower than it asked to be.
//   - A job resumes after a restart. Its whole live state is the cursor on the
//     row, so there is nothing unreachable about it the way an orphaned plugin
//     goroutine is unreachable.
//   - Cancellation is exact. Stopping means not making the next call; there is
//     nothing to ask nicely.
type ToolJobRunner struct {
	store    *memory.Store
	host     *toolvm.Host
	maxCalls int
	minDelay time.Duration
	workers  int
}

// NewToolJobRunner builds the tool backend for the job sweeper, or nil when
// there is no sandboxed-tool host — which is what keeps the whole thing additive
// for a deployment with [tools] unset.
func NewToolJobRunner(store *memory.Store, host *toolvm.Host, maxCalls, minDelayMS, workers int) *ToolJobRunner {
	if store == nil || host == nil {
		return nil
	}
	return newToolJobRunner(store, host, maxCalls, minDelayMS, workers)
}

func newToolJobRunner(store *memory.Store, host *toolvm.Host, maxCalls, minDelayMS, workers int) *ToolJobRunner {
	if maxCalls <= 0 {
		maxCalls = DefaultJobMaxCalls
	}
	if minDelayMS <= 0 {
		minDelayMS = DefaultJobMinDelayMS
	}
	if workers <= 0 {
		workers = DefaultJobWorkers
	}
	return &ToolJobRunner{
		store:    store,
		host:     host,
		maxCalls: maxCalls,
		minDelay: time.Duration(minDelayMS) * time.Millisecond,
		workers:  workers,
	}
}

// runDue makes one call for every tool job that is due, up to `workers` at a
// time, and reconciles each row.
//
// **It waits for the whole batch before returning, and that is load-bearing.**
// The sweeper's ticker drops ticks while a sweep is in progress, so waiting is
// what guarantees a job is never called twice concurrently: within one batch
// JobsDueForCall returns each handle once, and across batches the wait prevents
// overlap. Firing these off and returning would let a slow call still be running
// when the next tick found the same row due — its next_at has not moved yet — and
// two calls would run against the same cursor.
//
// The pool exists because the tool backend *executes* rather than polls. A
// sequential loop was right when a sweep issued HTTP requests; with fifty due
// jobs it would put the slowest tool's deadline in front of everyone else.
func (r *ToolJobRunner) runDue(ctx context.Context, s *jobSweeper) {
	if r == nil || r.host == nil {
		return
	}
	jobs, err := r.store.JobsDueForCall()
	if err != nil {
		slog.Warn("tool job sweep: list due", "err", err)
		return
	}
	if len(jobs) == 0 {
		return
	}

	sem := make(chan struct{}, r.workers)
	var wg sync.WaitGroup
	for _, j := range jobs {
		select {
		case <-ctx.Done():
			// Shutting down: stop handing out work, but let what is running finish
			// its own deadline rather than abandoning half-written rows.
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(j memory.Job) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runOnce(ctx, s, j)
		}(j)
	}
	wg.Wait()
}

// runOnce makes a single call of one job and writes back what happened.
func (r *ToolJobRunner) runOnce(ctx context.Context, s *jobSweeper, j memory.Job) {
	// The call budget is checked before spending one, so a job that has reached
	// its cap fails without a final extra call.
	if j.Calls >= r.maxCalls {
		s.finishJob(j, "failed", "", fmt.Sprintf(
			"exceeded job_max_calls (%d): the tool asked to continue every time and never returned a result",
			r.maxCalls))
		return
	}

	// The conversation this job belongs to, so a conversation-scoped `state`
	// grant resolves the same way on call 40 as it did on call 1.
	//
	// Without this a tool granted state at conversation scope works inside the
	// turn that started it and then dies on its second call, because the sweeper
	// has no turn and stateScopeFor refuses rather than falling back (R-TVM.18).
	// owner_id *is* the conversation id — the registry has treated the agent id
	// and the conversation id as one value since plugin jobs landed.
	callCtx := toolvm.WithStateScope(ctx, j.OwnerID)

	out, err := r.host.CallJob(callCtx, j.Tool, json.RawMessage(j.Args), toolvm.JobContext{
		Cursor: j.Cursor,
		Call:   j.Calls + 1,
	})
	if err != nil {
		// The tool's own failure is terminal for the job: a resumable tool that
		// wants to survive a transient error catches it and returns again().
		s.finishJob(j, "failed", "", err.Error())
		return
	}

	if out.Continue == nil {
		text := out.Text
		if out.Bytes != nil {
			// A job's result reaches its owner as a notification, which is text.
			// Bytes would need a spill path the notification cannot carry, so this
			// is refused rather than silently base64'd into someone's context.
			s.finishJob(j, "failed", "", fmt.Sprintf(
				"tool %q finished with %d bytes of binary output, which a background job cannot deliver",
				j.Tool, len(out.Bytes)))
			return
		}
		s.finishJob(j, "done", text, "")
		return
	}

	next := time.Now().Add(max(time.Duration(out.Continue.AfterMS)*time.Millisecond, r.minDelay))
	took, err := r.store.JobAdvance(j.Handle, out.Continue.Cursor, out.Continue.Progress, next)
	if err != nil {
		slog.Warn("tool job sweep: advance", "handle", j.Handle, "err", err)
		return
	}
	if !took {
		// Cancelled (or expired) while this call was in flight. Dropping the
		// result is correct: the row is terminal and must not be resurrected.
		slog.Info("tool job call discarded; the job is no longer running", "handle", j.Handle)
	}
}

// ResumeToolJobs is the tool backend's counterpart to MarkOrphanedJobsLost.
//
// resumeAtBoot logs the tool jobs this daemon is picking up where the previous
// one left off.
//
// There is no work to do beyond saying so: a tool job's live state is entirely
// its row, so the sweeper's ordinary due-check finds it on the first tick. This
// is the thing the plugin backend cannot do — MarkOrphanedJobsLost exists there
// precisely because a plugin's goroutine died with the process that held it.
func ResumeToolJobs(store *memory.Store) int {
	if store == nil {
		return 0
	}
	jobs, err := store.JobsResumable()
	if err != nil {
		slog.Warn("could not list resumable tool jobs", "err", err)
		return 0
	}
	for _, j := range jobs {
		slog.Info("resuming tool job", "handle", j.Handle, "tool", j.Tool,
			"calls", j.Calls, "owner", j.OwnerID)
	}
	return len(jobs)
}
