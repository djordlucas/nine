package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/toolvm"
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
// tool-output handling (adr/tool-output-spill.md).
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
	// minDelay floors the first call's delay, as it floors every later one.
	minDelay time.Duration
}

func newJobStarter(store *memory.Store, mgr *plugin.Manager, ownerID string, maxJobs int) *jobStarter {
	if maxJobs <= 0 {
		maxJobs = DefaultMaxJobsPerConversation
	}
	return &jobStarter{
		store:    store,
		mgr:      mgr,
		ownerID:  ownerID,
		maxJobs:  maxJobs,
		minDelay: DefaultJobMinDelayMS * time.Millisecond,
	}
}

// StartToolJob records a long-running sandboxed-tool job.
//
// The tool has already done one call's worth of work — that is how it came to
// return a continuation — so the row starts at calls=1 with the cursor it
// produced, and the sweeper takes it from there.
//
// The per-conversation cap is a genuine refusal here, unlike the plugin path.
// A plugin has already started its goroutine by the time its id reaches us, so
// an over-cap plugin job must be admitted and then cancelled; the daemon decides
// when a tool job's next call happens, so it can simply decline. The one call
// already spent is the tool's own doing and is not charged to anything.
func (js *jobStarter) StartToolJob(ctx context.Context, tool string, args json.RawMessage, c *toolvm.Continuation) (string, error) {
	if c == nil {
		return "", fmt.Errorf("tool %q: no continuation to start a job from", tool)
	}
	if n, err := js.store.JobCountOutstanding(js.ownerID); err == nil && n >= js.maxJobs {
		return fmt.Sprintf("Not started: you already have %d background jobs running, the maximum. Wait for one to finish (job_wait/job_check) or cancel one (job_cancel), then try again.", js.maxJobs), nil
	}

	ack := c.Progress
	if ack == "" {
		ack = "started " + tool
	}

	handle := newJobHandle()
	// Args and Ack are separate fields on purpose. Every later call is made with
	// the model's original arguments — a resumable tool is called with the same
	// arguments and a changing cursor, and the cursor is the only thing that
	// moves — while Ack stays the one line a human or the model reads.
	if err := js.store.JobCreate(memory.Job{
		Handle:  handle,
		Backend: memory.JobBackendTool,
		Tool:    tool,
		OwnerID: js.ownerID,
		State:   string(plugin.JobRunning),
		Ack:     ack,
		Args:    string(argsOrEmpty(args)),
		Cursor:  c.Cursor,
	}); err != nil {
		return "", fmt.Errorf("record background job: %w", err)
	}
	next := time.Now().Add(max(time.Duration(c.AfterMS)*time.Millisecond, js.minDelay))
	if _, err := js.store.JobAdvance(handle, c.Cursor, c.Progress, next); err != nil {
		slog.Warn("could not schedule the first call of a tool job", "handle", handle, "err", err)
	}
	return fmt.Sprintf("Started background job %s: %s. It runs detached — its result will reach you on a later turn.", handle, ack), nil
}

// argsOrEmpty normalizes absent arguments to an empty object, so a later call
// re-parses something valid.
func argsOrEmpty(args json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return json.RawMessage(`{}`)
	}
	return args
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
	if n, err := js.store.JobCountOutstanding(js.ownerID); err == nil && n >= js.maxJobs {
		if p, ok := js.mgr.PluginByName(pluginName); ok {
			js.mgr.JobCancel(ctx, p, pluginJobID) //nolint:errcheck // best-effort; we are declining it
		}
		return fmt.Sprintf("Not started: you already have %d background jobs running, the maximum. Wait for one to finish (job_wait/job_check) or cancel one (job_cancel), then try again.", js.maxJobs), nil
	}

	handle := newJobHandle()
	err := js.store.JobCreate(memory.Job{
		Handle:     handle,
		Plugin:     pluginName,
		Tool:       tool,
		BackendRef: pluginJobID,
		OwnerID:    js.ownerID,
		State:      string(plugin.JobRunning),
		Ack:        ack,
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
	// tools runs the tool backend, or nil when no sandboxed-tool host exists.
	tools *ToolJobRunner
}

// RunJobSweeper polls running plugin jobs until ctx is cancelled, completing
// finished ones and expiring over-age ones (maxSeconds, 0 = DefaultJobMaxSeconds).
// interval is the base poll cadence; a job is polled at that rate for its first
// minute, then every jobPollBackoffSeconds. waiters, when non-nil, is signalled
// on each completion so a blocked job_wait wakes at once. Production-only, like
// the spill sweeper. A nil store or manager is a no-op.
func RunJobSweeper(ctx context.Context, store *memory.Store, mgr *plugin.Manager, waiters *JobWaiters, interval time.Duration, maxSeconds int) {
	RunJobSweeperWithTools(ctx, store, mgr, waiters, interval, maxSeconds, nil)
}

// RunJobSweeperWithTools is RunJobSweeper with the tool backend wired in. Both
// backends share one sweeper for the reason the registry is one table: the
// model-facing surface (job_wait/job_check/job_list/job_cancel) is already
// backend-agnostic, and two loops would mean two cadences, two shutdown paths,
// and two places to forget something.
func RunJobSweeperWithTools(ctx context.Context, store *memory.Store, mgr *plugin.Manager, waiters *JobWaiters, interval time.Duration, maxSeconds int, tools *ToolJobRunner) {
	// A nil manager still leaves the tool backend usable: sandboxed tools do not
	// need plugins, and a deployment can reasonably run one without the other.
	if store == nil || (mgr == nil && tools == nil) {
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
		tools:       tools,
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
	jobs, err := s.store.JobsDueForPoll(s.baseSeconds, backoff, jobPollYoungWindow)
	if err != nil {
		slog.Warn("job sweep: list due", "err", err)
		return
	}
	for _, j := range jobs {
		// The age-based backoff above is a *polling* schedule and applies only to
		// the plugin backend. A tool job's next call is due when the tool said it
		// was, which JobsDueForCall answers.
		if j.Backend == memory.JobBackendTool {
			continue
		}
		s.reconcile(ctx, j)
	}

	s.tools.runDue(ctx, s)
}

// expireOverAge fails jobs past their lifetime bound, attempts a best-effort
// cancel of each, and notifies its owner.
func (s *jobSweeper) expireOverAge(ctx context.Context) {
	expired, err := s.store.JobsExpire(s.maxSeconds, fmt.Sprintf("exceeded job_max_seconds (%ds)", s.maxSeconds))
	if err != nil {
		slog.Warn("job sweep: expire", "err", err)
		return
	}
	for _, j := range expired {
		if p, ok := s.pluginFor(j); ok {
			s.mgr.JobCancel(ctx, p, j.BackendRef) //nolint:errcheck // best-effort
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
func (s *jobSweeper) reconcile(ctx context.Context, j memory.Job) {
	p, ok := s.pluginFor(j)
	if !ok {
		// The plugin is gone (reload, crash). Phase 6 marks such rows lost at boot;
		// mid-run we simply leave it for the next tick rather than guess.
		return
	}
	st, err := s.mgr.JobStatus(ctx, p, j.BackendRef)
	if err != nil {
		slog.Warn("job sweep: poll", "handle", j.Handle, "plugin", j.Plugin, "err", err)
		return
	}

	state := string(st.State)
	if !memory.JobTerminal(state) {
		if err := s.store.JobUpdateLive(j.Handle, state, st.Progress); err != nil {
			slog.Warn("job sweep: update live", "handle", j.Handle, "err", err)
		}
		return
	}

	s.finishJob(j, state, st.Output, st.Error)
}

// finishJob records a terminal result and tells the owner, for either backend.
//
// Delivery is pull-only, unchanged: the notification waits for the owner's next
// turn rather than waking anything (adr/reactive-events.md). What differs
// between backends is only who produced the result — a plugin we polled, or a
// tool we called.
func (s *jobSweeper) finishJob(j memory.Job, state, output, errMsg string) {
	inline, spilled := s.storeResult(j, output)
	if err := s.store.JobFinish(j.Handle, state, inline, spilled, errMsg); err != nil {
		slog.Warn("job sweep: finish", "handle", j.Handle, "err", err)
		return
	}
	s.journal(j, state)
	if err := s.store.NotificationCreate(memory.NewID(), j.OwnerID,
		completionMessage(j, state, errMsg), false); err != nil {
		slog.Warn("job sweep: notify", "handle", j.Handle, "err", err)
	}
	s.wake(j.Handle)
	slog.Info("job finished", "handle", j.Handle, "backend", j.Backend,
		"plugin", j.Plugin, "tool", j.Tool, "state", state)
}

// pluginFor resolves a job's plugin, tolerating a daemon with no plugin manager
// at all — a deployment can run sandboxed tools without plugins.
func (s *jobSweeper) pluginFor(j memory.Job) (*plugin.Plugin, bool) {
	if s.mgr == nil {
		return nil, false
	}
	return s.mgr.PluginByName(j.Plugin)
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
	lost, err := store.JobsMarkLost("the daemon restarted while this job was running, so its result is unavailable")
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
	jobs, err := store.JobsRunning()
	if err != nil {
		slog.Warn("shutdown: list running jobs", "err", err)
		return
	}
	for _, j := range jobs {
		if p, ok := mgr.PluginByName(j.Plugin); ok {
			mgr.JobCancel(ctx, p, j.BackendRef) //nolint:errcheck // best-effort on the way down
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
func (s *jobSweeper) storeResult(j memory.Job, output string) (inline, spilled string) {
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
func (s *jobSweeper) journal(j memory.Job, state string) {
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
// completionMessage is what the owner reads on its next turn. It is written from
// the registry's own vocabulary rather than a backend's, so a tool job and a
// plugin job read identically — which is correct, since the difference is an
// implementation detail of where the work ran.
func completionMessage(j memory.Job, state, errMsg string) string {
	switch state {
	case string(plugin.JobDone):
		return fmt.Sprintf("Background job %s (%s) finished. Its result is ready.", j.Handle, j.Tool)
	case string(plugin.JobFailed):
		return fmt.Sprintf("Background job %s (%s) failed: %s", j.Handle, j.Tool, oneLine(errMsg))
	case string(plugin.JobCancelled):
		return fmt.Sprintf("Background job %s (%s) was cancelled.", j.Handle, j.Tool)
	default:
		return fmt.Sprintf("Background job %s (%s) ended: %s", j.Handle, j.Tool, state)
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
