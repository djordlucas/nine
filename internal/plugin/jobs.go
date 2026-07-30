package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// JobState is the lifecycle state of a plugin job (docs/plugin-capabilities.md
// §5). queued and running are live; done, failed, and cancelled are terminal.
type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobDone      JobState = "done"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

func (s JobState) terminal() bool {
	return s == JobDone || s == JobFailed || s == JobCancelled
}

// DefaultJobTTL is how long a terminal job (and its per-job directory) is
// retained before eviction, so a long-lived plugin does not accumulate finished
// jobs. The daemon polls well inside this window (docs/plugin-capabilities.md §5).
const DefaultJobTTL = time.Hour

// Job is the detached work a JobHandler returns: an immediate acknowledgement
// plus the function to run in the background.
type Job struct {
	// Ack is a one-line acknowledgement the model sees verbatim in place of a
	// result, e.g. "started download of ubuntu-24.04.iso".
	Ack string
	// Run performs the work and returns its output or an error. Its context is
	// detached from the HTTP request that started the job — it is cancelled only
	// by plugin.job_cancel or process exit, never by the reply being written.
	// Use JobDir(ctx) for scratch space and SetProgress(ctx, …) to report progress.
	Run func(ctx context.Context) (string, error)
}

// JobHandler starts a job for a tool call. It validates its arguments
// synchronously — a bad argument is an ordinary error the model gets at once,
// not a job that fails a moment later — and returns the Job to run detached.
type JobHandler func(ctx context.Context, args json.RawMessage) (Job, error)

// JobStatus is the plugin.job_status result. An unknown job id answers with
// state failed and a clear Error rather than an RPC error, so a daemon that lost
// track of an id cannot wedge.
type JobStatus struct {
	State    JobState `json:"state"`
	Progress string   `json:"progress,omitempty"`
	Output   string   `json:"output,omitempty"`
	Error    string   `json:"error,omitempty"`
}

type jobEntry struct {
	id       string
	state    JobState
	progress string
	output   string
	errMsg   string
	dir      string // <cache>/jobs/<id>; "" when no plugin cache dir is configured
	cancel   context.CancelFunc
	finished time.Time
}

// Jobs is the plugin-side job registry: it allocates ids, runs jobs through a
// worker pool that honours the plugin's max_concurrent, answers status and
// cancel queries, and evicts terminal jobs after a TTL. Create it with NewJobs
// and pass it to Serve via WithJobs; Serve wires in the concurrency cap and the
// cache dir.
type Jobs struct {
	mu     sync.Mutex
	jobs   map[string]*jobEntry
	sem    chan struct{} // worker-slot semaphore; nil means unbounded
	cache  string        // NINE_PLUGIN_CACHE_DIR, for per-job directories
	ttl    time.Duration
	nextID uint64
}

// NewJobs creates an empty job registry with the default terminal-job TTL.
func NewJobs() *Jobs {
	return &Jobs{jobs: make(map[string]*jobEntry), ttl: DefaultJobTTL}
}

// configure installs the concurrency cap and cache dir and starts background
// eviction. Serve calls it once, so the registry re-enforces max_concurrent for
// jobs — the daemon's cap is on connections and a job start returns instantly,
// so only the plugin can hold that line (docs/plugin-capabilities.md §5).
func (j *Jobs) configure(maxConcurrent int, cacheDir string) {
	j.mu.Lock()
	if maxConcurrent > 0 {
		j.sem = make(chan struct{}, maxConcurrent)
	}
	j.cache = cacheDir
	j.mu.Unlock()
	go j.evictLoop()
}

// start registers job, launches it, and returns its plugin-local id. The id is
// opaque to the model; the daemon maps it to a stable handle.
func (j *Jobs) start(job Job) string {
	ctx, cancel := context.WithCancel(context.Background())

	j.mu.Lock()
	j.nextID++
	id := fmt.Sprintf("j%d", j.nextID)
	e := &jobEntry{id: id, state: JobQueued, cancel: cancel}
	if j.cache != "" {
		e.dir = filepath.Join(j.cache, "jobs", id)
	}
	j.jobs[id] = e
	j.evictLocked(time.Now())
	j.mu.Unlock()

	go j.run(ctx, e, job.Run)
	return id
}

// run drives one job: acquire a worker slot (staying queued until one frees),
// then run detached, recording the terminal state and result.
func (j *Jobs) run(ctx context.Context, e *jobEntry, fn func(context.Context) (string, error)) {
	if j.sem != nil {
		select {
		case j.sem <- struct{}{}:
			defer func() { <-j.sem }()
		case <-ctx.Done():
			j.finish(e, JobCancelled, "", "cancelled before start")
			return
		}
	}
	if ctx.Err() != nil { // cancelled after acquiring the slot but before running
		j.finish(e, JobCancelled, "", "cancelled before start")
		return
	}

	j.setState(e, JobRunning)
	out, err := fn(withJobRun(ctx, j, e))
	switch {
	case ctx.Err() != nil:
		// A cancel that landed while running wins over whatever fn returned.
		j.finish(e, JobCancelled, out, "cancelled")
	case err != nil:
		j.finish(e, JobFailed, "", err.Error())
	default:
		j.finish(e, JobDone, out, "")
	}
}

// status returns the current state of id, or a failed status for an unknown id.
func (j *Jobs) status(id string) JobStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.evictLocked(time.Now())
	e, ok := j.jobs[id]
	if !ok {
		return JobStatus{State: JobFailed, Error: "unknown job id: " + id}
	}
	return JobStatus{State: e.state, Progress: e.progress, Output: e.output, Error: e.errMsg}
}

// cancel best-effort stops id and reports whether it was known. A queued job
// ends without running; a running job is cancelled via its context.
func (j *Jobs) cancel(id string) bool {
	j.mu.Lock()
	e, ok := j.jobs[id]
	j.mu.Unlock()
	if !ok {
		return false
	}
	e.cancel()
	return true
}

func (j *Jobs) setState(e *jobEntry, s JobState) {
	j.mu.Lock()
	e.state = s
	j.mu.Unlock()
}

func (j *Jobs) finish(e *jobEntry, s JobState, out, errMsg string) {
	j.mu.Lock()
	e.state = s
	e.output = out
	e.errMsg = errMsg
	e.finished = time.Now()
	j.mu.Unlock()
}

func (j *Jobs) setProgress(e *jobEntry, p string) {
	j.mu.Lock()
	e.progress = p
	j.mu.Unlock()
}

// evictLoop periodically drops expired terminal jobs even if nothing polls them.
func (j *Jobs) evictLoop() {
	iv := min(j.ttl/4, time.Minute)
	iv = max(iv, time.Second)
	t := time.NewTicker(iv)
	defer t.Stop()
	for range t.C {
		j.mu.Lock()
		j.evictLocked(time.Now())
		j.mu.Unlock()
	}
}

// evictLocked removes terminal jobs older than the TTL along with their per-job
// directory. Caller holds j.mu.
func (j *Jobs) evictLocked(now time.Time) {
	for id, e := range j.jobs {
		if e.state.terminal() && !e.finished.IsZero() && now.Sub(e.finished) > j.ttl {
			if e.dir != "" {
				os.RemoveAll(e.dir) //nolint:errcheck // best-effort
			}
			delete(j.jobs, id)
		}
	}
}

// --- per-job context helpers ---

type jobCtxKey struct{}

type jobRun struct {
	j *Jobs
	e *jobEntry
}

func withJobRun(ctx context.Context, j *Jobs, e *jobEntry) context.Context {
	return context.WithValue(ctx, jobCtxKey{}, &jobRun{j: j, e: e})
}

func jobRunFromContext(ctx context.Context) (*jobRun, bool) {
	r, ok := ctx.Value(jobCtxKey{}).(*jobRun)
	return r, ok
}

// JobDir returns a scratch directory unique to the running job, created on first
// call, under the plugin cache dir (<cache>/jobs/<id>). Two concurrent jobs
// never share one, so a handler need not namespace its own temp files. It errors
// when called outside a job's Run or when the plugin has no cache dir.
func JobDir(ctx context.Context) (string, error) {
	r, ok := jobRunFromContext(ctx)
	if !ok {
		return "", fmt.Errorf("JobDir called outside a job's Run")
	}
	if r.e.dir == "" {
		return "", fmt.Errorf("no plugin cache dir configured; cannot allocate a job dir")
	}
	if err := os.MkdirAll(r.e.dir, 0o700); err != nil {
		return "", err
	}
	return r.e.dir, nil
}

// SetProgress records a free-text one-line progress note for the running job
// (e.g. "41% · 1.2 GB/2.9 GB"), surfaced to the model via job_status. It is a
// no-op when called outside a job's Run.
func SetProgress(ctx context.Context, progress string) {
	if r, ok := jobRunFromContext(ctx); ok {
		r.j.setProgress(r.e, progress)
	}
}
