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

// DefaultJobMaxSeconds bounds a single job's lifetime when
// [plugins].job_max_seconds is unset: one hour.
const DefaultJobMaxSeconds = 3600

// Job-poll backoff (docs/plugin-capabilities.md §5): a job is polled every
// base interval for its first minute, then every jobPollBackoffSeconds, so a
// long-running job stops being hammered once it is clearly slow.
const (
	jobPollBackoffSeconds = 30
	jobPollYoungWindow    = 60
)

// DefaultMaxJobsPerConversation caps a conversation's outstanding jobs when
// [plugins].max_jobs_per_conversation is unset.
const DefaultMaxJobsPerConversation = 8

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
	mgr     *plugin.Manager
	ownerID string
	maxJobs int
}

func newJobStarter(store *memory.Store, mgr *plugin.Manager, ownerID string, maxJobs int) *jobStarter {
	if maxJobs <= 0 {
		maxJobs = DefaultMaxJobsPerConversation
	}
	return &jobStarter{store: store, mgr: mgr, ownerID: ownerID, maxJobs: maxJobs}
}

// StartJob writes the registry row and returns a one-line observation naming the
// handle. The job is recorded as running; the sweeper reconciles its true state
// (queued/running/done…) on the next poll.
//
// The per-conversation cap is enforced here, on admission: the plugin has already
// started the goroutine by the time its id reaches us, so an over-cap job is
// cancelled rather than refused, and no row is written
// (docs/plugin-capabilities.md §5).
func (js *jobStarter) StartJob(ctx context.Context, pluginName, tool, pluginJobID, ack string) (string, error) {
	if n, err := js.store.PluginJobCountOutstanding(js.ownerID); err == nil && n >= js.maxJobs {
		if p, ok := js.mgr.PluginByName(pluginName); ok {
			js.mgr.JobCancel(ctx, p, pluginJobID) //nolint:errcheck // best-effort; we are declining it
		}
		return fmt.Sprintf("Not started: you already have %d background jobs running, the maximum. Wait for one to finish (job_wait/job_check) or cancel one (job_cancel), then try again.", js.maxJobs), nil
	}

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
	store       *memory.Store
	mgr         *plugin.Manager
	waiters     *JobWaiters
	maxSeconds  int
	baseSeconds int
}

// RunJobSweeper polls running plugin jobs until ctx is cancelled, completing
// finished ones and expiring over-age ones (maxSeconds, 0 = DefaultJobMaxSeconds).
// interval is the base poll cadence; a job is polled at that rate for its first
// minute, then every jobPollBackoffSeconds. waiters, when non-nil, is signalled
// on each completion so a blocked job_wait wakes at once. Production-only, like
// the spill sweeper. A nil store or manager is a no-op.
func RunJobSweeper(ctx context.Context, store *memory.Store, mgr *plugin.Manager, waiters *JobWaiters, interval time.Duration, maxSeconds int) {
	if store == nil || mgr == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultJobPollSeconds * time.Second
	}
	if maxSeconds <= 0 {
		maxSeconds = DefaultJobMaxSeconds
	}
	s := &jobSweeper{
		store:       store,
		mgr:         mgr,
		waiters:     waiters,
		maxSeconds:  maxSeconds,
		baseSeconds: max(int(interval/time.Second), 1),
	}
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

// sweepOnce expires over-age jobs, then polls the running jobs that are due under
// the backoff and reconciles the registry.
func (s *jobSweeper) sweepOnce(ctx context.Context) {
	s.expireOverAge(ctx)

	// A job older than the base cadence never wants a shorter backoff, so the
	// backoff is at least the base — an operator raising job_poll_seconds past 30s
	// simply polls everything at that rate rather than inverting young and old.
	backoff := max(jobPollBackoffSeconds, s.baseSeconds)
	jobs, err := s.store.PluginJobsDueForPoll(s.baseSeconds, backoff, jobPollYoungWindow)
	if err != nil {
		slog.Warn("job sweep: list due", "err", err)
		return
	}
	for _, j := range jobs {
		s.reconcile(ctx, j)
	}
}

// expireOverAge fails jobs past their lifetime bound, attempts a best-effort
// cancel of each, and notifies its owner.
func (s *jobSweeper) expireOverAge(ctx context.Context) {
	expired, err := s.store.PluginJobsExpire(s.maxSeconds, fmt.Sprintf("exceeded job_max_seconds (%ds)", s.maxSeconds))
	if err != nil {
		slog.Warn("job sweep: expire", "err", err)
		return
	}
	for _, j := range expired {
		if p, ok := s.mgr.PluginByName(j.Plugin); ok {
			s.mgr.JobCancel(ctx, p, j.PluginJobID) //nolint:errcheck // best-effort
		}
		msg := fmt.Sprintf("Background job %s (%s) was stopped after exceeding its %ds time limit.", j.Handle, j.Tool, s.maxSeconds)
		if err := s.store.NotificationCreate(memory.NewID(), j.OwnerID, msg, false); err != nil {
			slog.Warn("job sweep: expire notify", "handle", j.Handle, "err", err)
		}
		s.wake(j.Handle)
		slog.Info("plugin job expired", "handle", j.Handle, "plugin", j.Plugin)
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
	s.wake(j.Handle)
	slog.Info("plugin job finished", "handle", j.Handle, "plugin", j.Plugin, "state", state)
}

// MarkOrphanedJobsLost marks every still-running job lost at boot and notifies
// each owner. A job runs as a goroutine inside a plugin process, and this daemon
// spawns fresh plugins on fresh sockets, so any row still running belongs to a
// plugin the previous daemon left behind and is unreachable
// (docs/plugin-capabilities.md §5). Returns the number marked. Best-effort.
func MarkOrphanedJobsLost(store *memory.Store) int {
	if store == nil {
		return 0
	}
	lost, err := store.PluginJobsMarkLost("the daemon restarted while this job was running, so its result is unavailable")
	if err != nil {
		slog.Warn("mark orphaned jobs lost", "err", err)
		return 0
	}
	for _, j := range lost {
		msg := fmt.Sprintf("Background job %s (%s) was lost when the daemon restarted; its result is unavailable. You may start it again if you still need it.", j.Handle, j.Tool)
		if err := store.NotificationCreate(memory.NewID(), j.OwnerID, msg, false); err != nil {
			slog.Warn("mark lost: notify", "handle", j.Handle, "err", err)
		}
	}
	if len(lost) > 0 {
		slog.Info("marked orphaned plugin jobs lost", "count", len(lost))
	}
	return len(lost)
}

// ShutdownJobs cancels every running plugin job before the daemon stops its
// plugins, so a graceful shutdown asks jobs to stop rather than orphaning them.
// Best-effort and bounded by ctx. Callers run Manager.StopAll afterwards.
func ShutdownJobs(ctx context.Context, store *memory.Store, mgr *plugin.Manager) {
	if store == nil || mgr == nil {
		return
	}
	jobs, err := store.PluginJobsRunning()
	if err != nil {
		slog.Warn("shutdown: list running jobs", "err", err)
		return
	}
	for _, j := range jobs {
		if p, ok := mgr.PluginByName(j.Plugin); ok {
			mgr.JobCancel(ctx, p, j.PluginJobID) //nolint:errcheck // best-effort on the way down
		}
	}
}

// wake signals any job_wait blocked on this handle, when a waiter registry is
// wired. A no-op otherwise (the waiter falls back to polling).
func (s *jobSweeper) wake(handle string) {
	if s.waiters != nil {
		s.waiters.signal(handle)
	}
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
