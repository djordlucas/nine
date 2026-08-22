package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
)

// toolJobRunner is the tool backend of the job registry.
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
type toolJobRunner struct {
	store    *memory.Store
	host     *toolvm.Host
	maxCalls int
	minDelay time.Duration
}

// NewToolJobRunner builds the tool backend for the job sweeper, or nil when
// there is no sandboxed-tool host — which is what keeps the whole thing additive
// for a deployment with [tools] unset.
func NewToolJobRunner(store *memory.Store, host *toolvm.Host, maxCalls, minDelayMS int) *toolJobRunner {
	if store == nil || host == nil {
		return nil
	}
	return newToolJobRunner(store, host, maxCalls, minDelayMS)
}

func newToolJobRunner(store *memory.Store, host *toolvm.Host, maxCalls, minDelayMS int) *toolJobRunner {
	if maxCalls <= 0 {
		maxCalls = DefaultJobMaxCalls
	}
	if minDelayMS <= 0 {
		minDelayMS = DefaultJobMinDelayMS
	}
	return &toolJobRunner{
		store:    store,
		host:     host,
		maxCalls: maxCalls,
		minDelay: time.Duration(minDelayMS) * time.Millisecond,
	}
}

// runDue makes one call for every tool job that is due, and reconciles the row.
func (r *toolJobRunner) runDue(ctx context.Context, s *jobSweeper) {
	if r == nil || r.host == nil {
		return
	}
	jobs, err := r.store.JobsDueForCall()
	if err != nil {
		slog.Warn("tool job sweep: list due", "err", err)
		return
	}
	for _, j := range jobs {
		r.runOnce(ctx, s, j)
	}
}

// runOnce makes a single call of one job and writes back what happened.
func (r *toolJobRunner) runOnce(ctx context.Context, s *jobSweeper, j memory.Job) {
	// The call budget is checked before spending one, so a job that has reached
	// its cap fails without a final extra call.
	if j.Calls >= r.maxCalls {
		s.finishJob(j, "failed", "", fmt.Sprintf(
			"exceeded job_max_calls (%d): the tool asked to continue every time and never returned a result",
			r.maxCalls))
		return
	}

	// The turn that started this job is long gone, so there is no conversation on
	// the context — a conversation-scoped `state` grant is refused here by design
	// (R-TVM.18), and a resumable tool that wants to remember across calls uses
	// its cursor, or a tool-scoped grant.
	out, err := r.host.CallJob(ctx, j.Tool, json.RawMessage(j.Ack), toolvm.JobContext{
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
