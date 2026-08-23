package memory

import (
	"database/sql"
	"time"
)

// Job is one row of the job registry: a piece of long-running work the daemon
// tracks across turns and restarts (docs/plugin-capabilities.md §5,
// adr/durable-and-long-running-tools.md §4.3).
//
// Handle is the stable, model-facing id and is the only one the model ever sees.
// Backend decides what the sweeper does with the row:
//
//   - JobBackendPlugin — the work is a goroutine inside a plugin process. The
//     sweeper POLLS it at (Plugin, BackendRef). It cannot survive a restart,
//     because the process that held it is gone.
//   - JobBackendTool — the work is a sandboxed tool called repeatedly. The
//     sweeper IS the executor: it makes the next call, passing back the Cursor
//     the tool last returned. The whole live state is this row, so the job
//     resumes after a restart rather than being lost.
//
// OwnerID is the conversation to notify on completion.
type Job struct {
	Handle     string `json:"handle"`
	Backend    string `json:"backend"`
	Plugin     string `json:"plugin"`
	Tool       string `json:"tool"`
	BackendRef string `json:"backend_ref"`
	OwnerID    string `json:"owner_id"`
	State      string `json:"state"`
	// Ack is the one-line acknowledgement a human or the model reads — never
	// machine input. Args is the tool backend's separate field for the arguments
	// every later call is made with; conflating the two would mean anything that
	// renders Ack (reasonably) dumps raw JSON at the model.
	Ack        string `json:"ack,omitempty"`
	Args       string `json:"args,omitempty"`
	Progress   string `json:"progress,omitempty"`
	Output     string `json:"output,omitempty"`
	SpillPath  string `json:"spill_path,omitempty"`
	Error      string `json:"error,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	// Cursor is the tool backend's resume point: whatever the tool returned in
	// its last `continue` envelope, handed back verbatim on the next call. Opaque
	// to the daemon — it is the tool's own state, not something Nine interprets.
	Cursor string `json:"cursor,omitempty"`
	// Calls counts how many times a tool job has been called, bounding a tool
	// that returns `continue` forever.
	Calls int `json:"calls,omitempty"`
	// NextAt is the earliest the tool backend should make the next call — the
	// delay the tool asked for, clamped to the configured floor. Stored as an
	// instant rather than a duration so it does not depend on when the sweeper
	// happens to look.
	NextAt string `json:"next_at,omitempty"`
}

// Job backends. A row's backend decides whether the sweeper polls it or runs it.
const (
	JobBackendPlugin = "plugin"
	JobBackendTool   = "tool"
)

// terminalJobStates are the states a job never leaves; the sweeper stops polling
// once a job reaches one.
var terminalJobStates = map[string]bool{
	"done": true, "failed": true, "cancelled": true, "lost": true,
}

// JobTerminal reports whether state is terminal (no further polling).
func JobTerminal(state string) bool { return terminalJobStates[state] }

// JobCreate records a newly started job.
func (s *Store) JobCreate(j Job) error {
	backend := j.Backend
	if backend == "" {
		backend = JobBackendPlugin
	}
	_, err := s.db.Exec(
		`INSERT INTO jobs(handle, backend, plugin, tool, backend_ref, owner_id, state, ack, args, cursor)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		j.Handle, backend, j.Plugin, j.Tool, j.BackendRef, j.OwnerID, j.State, j.Ack, j.Args, j.Cursor)
	return err
}

// JobGet returns the job with handle, or (…, false, nil) if none exists.
func (s *Store) JobGet(handle string) (Job, bool, error) {
	row := s.db.QueryRow(
		`SELECT `+jobColumns+`
		 FROM jobs WHERE handle = ?`, handle)
	j, err := scanJob(row)
	if err == sql.ErrNoRows {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return j, true, nil
}

// JobsRunning returns every job still being polled (non-terminal), across
// all owners — the sweeper's work list.
func (s *Store) JobsRunning() ([]Job, error) {
	return s.queryJobs(
		`SELECT ` + jobColumns + `
		 FROM jobs WHERE state NOT IN ('done','failed','cancelled','lost')
		 ORDER BY created_at`)
}

// JobSummary is a compact view of an outstanding job for the context
// builder and job_list: enough to name it and show how long it has been running,
// without its (possibly large) result. AgeSeconds is computed at query time.
type JobSummary struct {
	Handle     string `json:"handle"`
	Tool       string `json:"tool"`
	State      string `json:"state"`
	Progress   string `json:"progress,omitempty"`
	AgeSeconds int    `json:"age_seconds"`
}

// JobsDueForPoll returns the non-terminal jobs the sweeper should poll
// now, applying an age-based backoff (docs/plugin-capabilities.md §5): a job
// younger than youngWindowSeconds is due every baseSeconds, an older one every
// backoffSeconds. updated_at is the last-poll time (each poll refreshes it), so a
// job not yet due is simply skipped this tick.
func (s *Store) JobsDueForPoll(baseSeconds, backoffSeconds, youngWindowSeconds int) ([]Job, error) {
	// The backoff is expressed as three cutoff instants computed here rather than
	// as interval arithmetic in SQL. The CASE now picks between two literal
	// cutoffs instead of two intervals, which keeps the whole predicate a plain
	// string comparison against an indexable column. The semantics are unchanged.
	now := time.Now()
	cutoff := func(sec int) string { return writeTime(now.Add(-time.Duration(sec) * time.Second)) }
	return s.queryJobs(
		`SELECT `+jobColumns+`
		 FROM jobs
		 WHERE backend = 'plugin' AND state NOT IN ('done','failed','cancelled','lost')
		   AND updated_at < (CASE WHEN created_at > ? THEN ? ELSE ? END)
		 ORDER BY created_at`,
		cutoff(youngWindowSeconds), cutoff(baseSeconds), cutoff(backoffSeconds))
}

// JobsOutstandingSummary returns ownerID's non-terminal jobs, oldest
// first — what the context builder surfaces and job_list reports.
func (s *Store) JobsOutstandingSummary(ownerID string) ([]JobSummary, error) {
	rows, err := s.db.Query(
		`SELECT handle, tool, state, progress, created_at
		 FROM jobs
		 WHERE owner_id = ? AND state NOT IN ('done','failed','cancelled','lost')
		 ORDER BY created_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// AgeSeconds is derived here rather than in SQL, from a single `now` for the
	// whole result set — so two jobs created a microsecond apart cannot report
	// ages that disagree about their ordering. Clamped at 0 against clock skew.
	now := time.Now()
	var out []JobSummary
	for rows.Next() {
		var (
			s         JobSummary
			createdAt string
		)
		if err := rows.Scan(&s.Handle, &s.Tool, &s.State, &s.Progress, &createdAt); err != nil {
			return nil, err
		}
		if t := parseStoredTime(createdAt); !t.IsZero() {
			if age := int(now.Sub(t).Seconds()); age > 0 {
				s.AgeSeconds = age
			}
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// JobUpdateLive updates a still-running job's state and progress. It never
// touches a terminal row (a late poll cannot resurrect a finished job).
func (s *Store) JobUpdateLive(handle, state, progress string) error {
	_, err := s.db.Exec(
		`UPDATE jobs SET state=?, progress=?, updated_at=?
		 WHERE handle=? AND state NOT IN ('done','failed','cancelled','lost')`,
		state, progress, nowText(), handle)
	return err
}

// JobFinish records a terminal state and its result. output/spillPath/errMsg
// are the caller's already cap-or-spilled values. It is idempotent-safe: a row
// already terminal is left unchanged.
func (s *Store) JobFinish(handle, state, output, spillPath, errMsg string) error {
	// One timestamp bound twice, so finished_at and updated_at are exactly equal
	// as a single now() within a statement guaranteed.
	ts := nowText()
	_, err := s.db.Exec(
		`UPDATE jobs
		 SET state=?, output=?, spill_path=?, error=?, finished_at=?, updated_at=?
		 WHERE handle=? AND state NOT IN ('done','failed','cancelled','lost')`,
		state, output, spillPath, errMsg, ts, ts, handle)
	return err
}

// JobCountOutstanding returns how many non-terminal jobs ownerID holds —
// the admission check for max_jobs_per_conversation.
func (s *Store) JobCountOutstanding(ownerID string) (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM jobs
		 WHERE owner_id = ? AND state NOT IN ('done','failed','cancelled','lost')`,
		ownerID).Scan(&n)
	return n, err
}

// JobsMarkLost marks every still-running PLUGIN job lost with a reason and
// returns the affected rows. Run at boot: such a job lives inside a plugin
// process, so any row still running belongs to a plugin the previous daemon left
// behind and can no longer be reached (docs/plugin-capabilities.md §5).
//
// Tool jobs are deliberately excluded. Their whole live state is the cursor on
// the row, so there is nothing unreachable about them — they resume
// (JobsResumable). `lost` is a plugin-backend state.
func (s *Store) JobsMarkLost(reason string) ([]Job, error) {
	ts := nowText()
	return s.execReturning(
		`UPDATE jobs SET state='lost', error=?, finished_at=?, updated_at=?
		 WHERE backend = 'plugin' AND state NOT IN ('done','failed','cancelled','lost')
		 RETURNING `+jobColumns,
		reason, ts, ts)
}

// JobsExpire marks every non-terminal job older than maxSeconds failed with
// reason and returns the affected rows, so the sweeper can attempt a cancel and
// notify the owner. maxSeconds <= 0 disables it (returns no rows).
func (s *Store) JobsExpire(maxSeconds int, reason string) ([]Job, error) {
	if maxSeconds <= 0 {
		return nil, nil
	}
	ts := nowText()
	return s.execReturning(
		`UPDATE jobs SET state='failed', error=?, finished_at=?, updated_at=?
		 WHERE state NOT IN ('done','failed','cancelled','lost')
		   AND created_at < ?
		 RETURNING `+jobColumns,
		reason, ts, ts, writeTime(time.Now().Add(-time.Duration(maxSeconds)*time.Second)))
}

// jobColumns is the full-row column list. It is one constant rather than seven
// copies so a new column cannot be added to the scan and forgotten in a query —
// the failure mode there is a silent zero value, not an error.
const jobColumns = `handle, backend, plugin, tool, backend_ref, owner_id, state, ack, args,
	        progress, output, spill_path, error, cursor, calls, next_at,
	        created_at, updated_at, finished_at`

type rowScanner interface{ Scan(...any) error }

func scanJob(row rowScanner) (Job, error) {
	var (
		j          Job
		finishedAt sql.NullString
	)
	err := row.Scan(&j.Handle, &j.Backend, &j.Plugin, &j.Tool, &j.BackendRef, &j.OwnerID,
		&j.State, &j.Ack, &j.Args, &j.Progress, &j.Output, &j.SpillPath, &j.Error,
		&j.Cursor, &j.Calls, &j.NextAt,
		&j.CreatedAt, &j.UpdatedAt, &finishedAt)
	if err != nil {
		return Job{}, err
	}
	j.FinishedAt = finishedAt.String
	return j, nil
}

// execReturning runs an UPDATE … RETURNING and collects every row.
//
// Unlike queryJobs it never abandons the sql.Rows partway through. SQLite
// applies a RETURNING statement's changes incrementally as rows are stepped, so
// returning early on a scan error would leave the update half-applied — where a
// server-side engine would have completed it before streaming anything. Drain
// first, report after.
func (s *Store) execReturning(query string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		jobs     []Job
		firstErr error
	)
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return jobs, err
	}
	return jobs, firstErr
}

func (s *Store) queryJobs(query string, args ...any) ([]Job, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// JobAdvance records one completed call of a tool job: the cursor it returned,
// the progress line it reported, and the call it just spent.
//
// It refuses a terminal row for the same reason JobUpdateLive does — a call that
// finishes after a cancel must not resurrect the job — and it reports whether it
// took, so the driver can tell "cancelled underneath me" from "advanced".
func (s *Store) JobAdvance(handle, cursor, progress string, nextAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE jobs SET cursor=?, progress=?, calls=calls+1, next_at=?, updated_at=?
		 WHERE handle=? AND state NOT IN ('done','failed','cancelled','lost')`,
		cursor, progress, writeTime(nextAt), nowText(), handle)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// JobsDueForCall returns the tool jobs whose next call is due now, oldest first.
//
// Unlike JobsDueForPoll this applies no backoff, and the difference is not an
// oversight. For a plugin the sweeper polls work someone else is doing, so
// asking less often as a job ages costs nothing; for a tool the call IS the
// work, and backing it off would simply make the job slower than the tool asked
// to be.
func (s *Store) JobsDueForCall() ([]Job, error) {
	return s.queryJobs(
		`SELECT `+jobColumns+`
		 FROM jobs
		 WHERE backend = ? AND state NOT IN ('done','failed','cancelled','lost')
		   AND (next_at = '' OR next_at <= ?)
		 ORDER BY created_at`, JobBackendTool, nowText())
}

// JobsResumable returns the non-terminal tool jobs, oldest first.
//
// This is the tool backend's answer to JobsMarkLost. A plugin job cannot survive
// a restart — it lived as a goroutine in a process this daemon did not start — but
// a tool job's entire live state is its cursor, in this row, so the sweeper simply
// makes the next call. Nothing here is marked lost.
func (s *Store) JobsResumable() ([]Job, error) {
	return s.queryJobs(
		`SELECT `+jobColumns+`
		 FROM jobs
		 WHERE backend = ? AND state NOT IN ('done','failed','cancelled','lost')
		 ORDER BY created_at`, JobBackendTool)
}

// JobsCountRunning returns how many non-terminal jobs exist across every owner.
//
// The daemon-wide admission check, distinct from JobCountOutstanding's per-owner
// one. It matters more since the tool backend landed: a plugin job is work
// someone else's process performs, but a tool job is work *this* daemon does, so
// the total is a real resource and not just a tidiness concern.
func (s *Store) JobsCountRunning() (int, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT count(*) FROM jobs WHERE state NOT IN ('done','failed','cancelled','lost')`).Scan(&n)
	return n, err
}
