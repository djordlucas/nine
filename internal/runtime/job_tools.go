package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"nine/internal/memory"
	"nine/internal/plugin"
)

// jobWaitPoll is how often job_wait re-reads the registry while blocking. The
// sweeper is the single poller of the plugins themselves; job_wait only watches
// the row it already owns, so a short interval here costs a cheap local read, not
// a plugin round-trip. (A sweeper-signalled channel would remove even that; it is
// a deferred efficiency refinement — behaviour is identical either way.)
const jobWaitPoll = 500 * time.Millisecond

// jobTools implements agent.JobTools against the registry store and the plugin
// manager, scoped to one conversation (ownerID).
type jobTools struct {
	store   *memory.Store
	mgr     *plugin.Manager
	ownerID string
}

func newJobTools(store *memory.Store, mgr *plugin.Manager, ownerID string) *jobTools {
	return &jobTools{store: store, mgr: mgr, ownerID: ownerID}
}

// Wait blocks until the job is terminal, the timeout elapses, or the turn is
// cancelled. A timeout is not a failure: it returns the current progress so the
// model can move on and check back later.
func (jt *jobTools) Wait(ctx context.Context, handle string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		j, ok, err := jt.store.PluginJobGet(handle)
		if err != nil {
			return "", err
		}
		if !ok {
			return fmt.Sprintf("There is no background job with handle %s.", handle), nil
		}
		if memory.PluginJobTerminal(j.State) {
			return renderJob(j), nil
		}
		if !time.Now().Before(deadline) {
			return stillRunning(j), nil
		}
		select {
		case <-ctx.Done():
			return stillRunning(j), nil
		case <-time.After(min(jobWaitPoll, time.Until(deadline))):
		}
	}
}

// Check returns the job's current state and result without waiting.
func (jt *jobTools) Check(_ context.Context, handle string) (string, error) {
	j, ok, err := jt.store.PluginJobGet(handle)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("There is no background job with handle %s.", handle), nil
	}
	return renderJob(j), nil
}

// List reports the conversation's outstanding jobs.
func (jt *jobTools) List(_ context.Context) (string, error) {
	sums, err := jt.store.PluginJobsOutstandingSummary(jt.ownerID)
	if err != nil {
		return "", err
	}
	if len(sums) == 0 {
		return "No outstanding background jobs.", nil
	}
	var b strings.Builder
	b.WriteString("Outstanding background jobs:\n")
	for _, s := range sums {
		b.WriteString(jobSummaryLine(s))
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// Cancel requests best-effort cancellation of a job via its plugin.
func (jt *jobTools) Cancel(ctx context.Context, handle string) (string, error) {
	j, ok, err := jt.store.PluginJobGet(handle)
	if err != nil {
		return "", err
	}
	if !ok {
		return fmt.Sprintf("There is no background job with handle %s to cancel.", handle), nil
	}
	if memory.PluginJobTerminal(j.State) {
		return fmt.Sprintf("Job %s has already finished (%s).", handle, j.State), nil
	}
	p, running := jt.mgr.PluginByName(j.Plugin)
	if !running {
		return fmt.Sprintf("Cannot cancel %s: its plugin %q is no longer running.", handle, j.Plugin), nil
	}
	if err := jt.mgr.JobCancel(ctx, p, j.PluginJobID); err != nil {
		return "", fmt.Errorf("cancel %s: %w", handle, err)
	}
	return fmt.Sprintf("Requested cancellation of %s; it will stop shortly.", handle), nil
}

// renderJob is the model-facing rendering of a job row, terminal or live.
func renderJob(j memory.PluginJob) string {
	switch j.State {
	case string(plugin.JobDone):
		out := j.Output
		if out == "" {
			out = "(no output)"
		}
		return fmt.Sprintf("Job %s (%s) finished.\n%s", j.Handle, j.Tool, out)
	case string(plugin.JobFailed):
		return fmt.Sprintf("Job %s (%s) failed: %s", j.Handle, j.Tool, j.Error)
	case string(plugin.JobCancelled):
		return fmt.Sprintf("Job %s (%s) was cancelled.", j.Handle, j.Tool)
	case "lost":
		return fmt.Sprintf("Job %s (%s) was lost — the daemon restarted while it was running, so its result is unavailable.", j.Handle, j.Tool)
	default:
		return stillRunning(j)
	}
}

func stillRunning(j memory.PluginJob) string {
	s := fmt.Sprintf("Job %s (%s) is still %s.", j.Handle, j.Tool, j.State)
	if j.Progress != "" {
		s += " Progress: " + j.Progress + "."
	}
	return s
}

func jobSummaryLine(s memory.PluginJobSummary) string {
	line := fmt.Sprintf("- %s · %s · %s · %s old", s.Handle, s.Tool, s.State, humanAge(s.AgeSeconds))
	if s.Progress != "" {
		line += " · " + s.Progress
	}
	return line
}

// humanAge renders a whole-second age compactly (e.g. 5s, 3m, 2h).
func humanAge(seconds int) string {
	switch {
	case seconds < 60:
		return fmt.Sprintf("%ds", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%dh", seconds/3600)
	}
}

// jobSurfaceTopN caps how many outstanding jobs the context builder lists, so a
// runaway backlog cannot crowd out the turn.
const jobSurfaceTopN = 5

// jobsEnrichmentFn surfaces the conversation's outstanding jobs as one compact
// line each, so a model that forgot about an in-flight job still sees it in
// context next turn (docs/plugin-capabilities.md §5). Returns "" when there are
// none, so nothing is surfaced in the common case.
func jobsEnrichmentFn(store *memory.Store, ownerID string) func(context.Context, []float32) string {
	return func(_ context.Context, _ []float32) string {
		sums, err := store.PluginJobsOutstandingSummary(ownerID)
		if err != nil || len(sums) == 0 {
			return ""
		}
		var b strings.Builder
		b.WriteString("Background jobs you started that are still running (their results will reach you on a later turn):")
		for i, s := range sums {
			if i >= jobSurfaceTopN {
				fmt.Fprintf(&b, "\n- …and %d more", len(sums)-jobSurfaceTopN)
				break
			}
			b.WriteByte('\n')
			b.WriteString(jobSummaryLine(s))
		}
		return b.String()
	}
}
