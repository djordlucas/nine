package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"nine/internal/memory"
	"nine/internal/plugin"
)

// DefaultJobPollSeconds is how often the sweeper polls running plugin jobs when
// [plugins].job_poll_seconds is unset.
const DefaultJobPollSeconds = 2

// jobInlineCap is how many characters of a job's terminal output are kept inline
// in the registry row; a larger result is spilled to the file store, mirroring
// tool-output handling (docs/tool-output-spill.md).
const jobInlineCap = 8192

// newJobHandle returns a short, stable, model-facing job id (job_<8 hex>).
func newJobHandle() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "job_" + hex.EncodeToString(b[:])
}

// jobStarter implements agent.JobStarter: it records a plugin job keyed to the
// owning conversation and returns the observation the model sees.
type jobStarter struct {
	store   *memory.Store
	ownerID string
}

func newJobStarter(store *memory.Store, ownerID string) *jobStarter {
	return &jobStarter{store: store, ownerID: ownerID}
}

// StartJob writes the registry row and returns a one-line observation naming the
// handle. The job is recorded as running; the sweeper reconciles its true state
// (queued/running/done…) on the next poll.
func (js *jobStarter) StartJob(_ context.Context, pluginName, tool, pluginJobID, ack string) (string, error) {
	handle := newJobHandle()
	err := js.store.PluginJobCreate(memory.PluginJob{
		Handle:      handle,
		Plugin:      pluginName,
		Tool:        tool,
		PluginJobID: pluginJobID,
		OwnerID:     js.ownerID,
		State:       string(plugin.JobRunning),
		Ack:         ack,
	})
	if err != nil {
		return "", fmt.Errorf("record background job: %w", err)
	}
	return fmt.Sprintf("Started background job %s: %s. It runs detached — its result will reach you on a later turn.", handle, ack), nil
}

// --- the sweeper ---

// jobSweeper polls the plugins for the state of every running registry job and,
// on completion, records the result and notifies the owning conversation
// (docs/plugin-capabilities.md §5). It is a single daemon-level component, not a
// goroutine per job.
type jobSweeper struct {
	store *memory.Store
	mgr   *plugin.Manager
}

// RunJobSweeper polls running plugin jobs on interval until ctx is cancelled,
// completing finished ones. Production-only, like the spill sweeper: the eval
// harness never starts one. A nil store or manager is a no-op.
func RunJobSweeper(ctx context.Context, store *memory.Store, mgr *plugin.Manager, interval time.Duration) {
	if store == nil || mgr == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultJobPollSeconds * time.Second
	}
	s := &jobSweeper{store: store, mgr: mgr}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce polls every running job once and reconciles the registry.
func (s *jobSweeper) sweepOnce(ctx context.Context) {
	jobs, err := s.store.PluginJobsRunning()
	if err != nil {
		slog.Warn("job sweep: list running", "err", err)
		return
	}
	for _, j := range jobs {
		s.reconcile(ctx, j)
	}
}

// reconcile polls one job and updates its row: live jobs get their state and
// progress refreshed; a terminal job is finished (cap-or-spill), and its owner
// is notified so the next turn learns of it even if nothing waited.
func (s *jobSweeper) reconcile(ctx context.Context, j memory.PluginJob) {
	p, ok := s.mgr.PluginByName(j.Plugin)
	if !ok {
		// The plugin is gone (reload, crash). Phase 6 marks such rows lost at boot;
		// mid-run we simply leave it for the next tick rather than guess.
		return
	}
	st, err := s.mgr.JobStatus(ctx, p, j.PluginJobID)
	if err != nil {
		slog.Warn("job sweep: poll", "handle", j.Handle, "plugin", j.Plugin, "err", err)
		return
	}

	state := string(st.State)
	if !memory.PluginJobTerminal(state) {
		if err := s.store.PluginJobUpdateLive(j.Handle, state, st.Progress); err != nil {
			slog.Warn("job sweep: update live", "handle", j.Handle, "err", err)
		}
		return
	}

	inline, spilled := s.storeResult(j, st.Output)
	if err := s.store.PluginJobFinish(j.Handle, state, inline, spilled, st.Error); err != nil {
		slog.Warn("job sweep: finish", "handle", j.Handle, "err", err)
		return
	}
	s.journal(j, state)
	if err := s.store.NotificationCreate(memory.NewID(), j.OwnerID, completionMessage(j, st), false); err != nil {
		slog.Warn("job sweep: notify", "handle", j.Handle, "err", err)
	}
	slog.Info("plugin job finished", "handle", j.Handle, "plugin", j.Plugin, "state", state)
}

// storeResult caps or spills a job's terminal output, mirroring the tool-output
// path: a small result stays inline in the row, a large one goes to the file
// store with a short preview kept inline.
func (s *jobSweeper) storeResult(j memory.PluginJob, output string) (inline, spilled string) {
	if utf8.RuneCountInString(output) <= jobInlineCap {
		return output, ""
	}
	p := spillPath(j.OwnerID, "job-"+j.Tool)
	if err := s.store.FileStore(p, output); err != nil {
		slog.Warn("job sweep: spill", "handle", j.Handle, "err", err)
		return clipRunes(output, jobInlineCap) + "\n[output truncated]", ""
	}
	return clipRunes(output, jobInlineCap) + "\n[full output at " + p + "]", p
}

// journal records a terminal-job event on the owner's execution journal.
// Best-effort: journaling must never take the sweeper down.
func (s *jobSweeper) journal(j memory.PluginJob, state string) {
	err := s.store.SessionEventsAppend([]memory.SessionEvent{{
		AgentID: j.OwnerID,
		SpanID:  "job-" + j.Handle,
		Type:    "plugin_job_" + state,
		Payload: fmt.Appendf(nil, `{"handle":%q,"plugin":%q,"tool":%q}`, j.Handle, j.Plugin, j.Tool),
	}})
	if err != nil {
		slog.Warn("job sweep: journal", "handle", j.Handle, "err", err)
	}
}

// completionMessage is the one-line notification the owner reads next turn.
func completionMessage(j memory.PluginJob, st plugin.JobStatus) string {
	switch st.State {
	case plugin.JobDone:
		return fmt.Sprintf("Background job %s (%s) finished. Its result is ready.", j.Handle, j.Tool)
	case plugin.JobFailed:
		return fmt.Sprintf("Background job %s (%s) failed: %s", j.Handle, j.Tool, oneLine(st.Error))
	case plugin.JobCancelled:
		return fmt.Sprintf("Background job %s (%s) was cancelled.", j.Handle, j.Tool)
	default:
		return fmt.Sprintf("Background job %s (%s) ended: %s", j.Handle, j.Tool, st.State)
	}
}

// oneLine keeps a message to its first line, so a multi-line plugin error does
// not blow up the notification.
func oneLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}

// clipRunes returns the first n runes of s.
func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}
