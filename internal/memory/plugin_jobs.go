package memory

import "database/sql"

// PluginJob is one row of the plugin-job registry (docs/plugin-capabilities.md
// §5): a piece of long-running plugin work the daemon tracks across turns and
// restarts. Handle is the stable, model-facing id; (Plugin, PluginJobID) is
// where the sweeper polls; OwnerID is the conversation to notify on completion.
type PluginJob struct {
	Handle      string `json:"handle"`
	Plugin      string `json:"plugin"`
	Tool        string `json:"tool"`
	PluginJobID string `json:"plugin_job_id"`
	OwnerID     string `json:"owner_id"`
	State       string `json:"state"`
	Ack         string `json:"ack,omitempty"`
	Progress    string `json:"progress,omitempty"`
	Output      string `json:"output,omitempty"`
	SpillPath   string `json:"spill_path,omitempty"`
	Error       string `json:"error,omitempty"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
	FinishedAt  string `json:"finished_at,omitempty"`
}

// terminalJobStates are the states a job never leaves; the sweeper stops polling
// once a job reaches one.
var terminalJobStates = map[string]bool{
	"done": true, "failed": true, "cancelled": true, "lost": true,
}

// PluginJobTerminal reports whether state is terminal (no further polling).
func PluginJobTerminal(state string) bool { return terminalJobStates[state] }

// PluginJobCreate records a newly started job.
func (s *Store) PluginJobCreate(j PluginJob) error {
	_, err := s.db.Exec(
		`INSERT INTO plugin_jobs(handle, plugin, tool, plugin_job_id, owner_id, state, ack)
		 VALUES(?,?,?,?,?,?,?)`,
		j.Handle, j.Plugin, j.Tool, j.PluginJobID, j.OwnerID, j.State, j.Ack)
	return err
}

// PluginJobGet returns the job with handle, or (…, false, nil) if none exists.
func (s *Store) PluginJobGet(handle string) (PluginJob, bool, error) {
	row := s.db.QueryRow(
		`SELECT handle, plugin, tool, plugin_job_id, owner_id, state, ack, progress,
		        output, spill_path, error, created_at, updated_at, finished_at
		 FROM plugin_jobs WHERE handle = ?`, handle)
	j, err := scanPluginJob(row)
	if err == sql.ErrNoRows {
		return PluginJob{}, false, nil
	}
	if err != nil {
		return PluginJob{}, false, err
	}
	return j, true, nil
}

// PluginJobsRunning returns every job still being polled (non-terminal), across
// all owners — the sweeper's work list.
func (s *Store) PluginJobsRunning() ([]PluginJob, error) {
	return s.queryPluginJobs(
		`SELECT handle, plugin, tool, plugin_job_id, owner_id, state, ack, progress,
		        output, spill_path, error, created_at, updated_at, finished_at
		 FROM plugin_jobs WHERE state NOT IN ('done','failed','cancelled','lost')
		 ORDER BY created_at`)
}

// PluginJobSummary is a compact view of an outstanding job for the context
// builder and job_list: enough to name it and show how long it has been running,
// without its (possibly large) result. AgeSeconds is computed at query time.
type PluginJobSummary struct {
	Handle     string `json:"handle"`
	Tool       string `json:"tool"`
	State      string `json:"state"`
	Progress   string `json:"progress,omitempty"`
	AgeSeconds int    `json:"age_seconds"`
}

// PluginJobsOutstandingSummary returns ownerID's non-terminal jobs, oldest
// first — what the context builder surfaces and job_list reports.
func (s *Store) PluginJobsOutstandingSummary(ownerID string) ([]PluginJobSummary, error) {
	rows, err := s.db.Query(
		`SELECT handle, tool, state, progress,
		        extract(epoch FROM now() - created_at)::int
		 FROM plugin_jobs
		 WHERE owner_id = ? AND state NOT IN ('done','failed','cancelled','lost')
		 ORDER BY created_at`, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PluginJobSummary
	for rows.Next() {
		var s PluginJobSummary
		if err := rows.Scan(&s.Handle, &s.Tool, &s.State, &s.Progress, &s.AgeSeconds); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// PluginJobUpdateLive updates a still-running job's state and progress. It never
// touches a terminal row (a late poll cannot resurrect a finished job).
func (s *Store) PluginJobUpdateLive(handle, state, progress string) error {
	_, err := s.db.Exec(
		`UPDATE plugin_jobs SET state=?, progress=?, updated_at=now()
		 WHERE handle=? AND state NOT IN ('done','failed','cancelled','lost')`,
		state, progress, handle)
	return err
}

// PluginJobFinish records a terminal state and its result. output/spillPath/errMsg
// are the caller's already cap-or-spilled values. It is idempotent-safe: a row
// already terminal is left unchanged.
func (s *Store) PluginJobFinish(handle, state, output, spillPath, errMsg string) error {
	_, err := s.db.Exec(
		`UPDATE plugin_jobs
		 SET state=?, output=?, spill_path=?, error=?, finished_at=now(), updated_at=now()
		 WHERE handle=? AND state NOT IN ('done','failed','cancelled','lost')`,
		state, output, spillPath, errMsg, handle)
	return err
}

type rowScanner interface{ Scan(...any) error }

func scanPluginJob(row rowScanner) (PluginJob, error) {
	var (
		j          PluginJob
		finishedAt sql.NullString
	)
	err := row.Scan(&j.Handle, &j.Plugin, &j.Tool, &j.PluginJobID, &j.OwnerID, &j.State,
		&j.Ack, &j.Progress, &j.Output, &j.SpillPath, &j.Error,
		&j.CreatedAt, &j.UpdatedAt, &finishedAt)
	if err != nil {
		return PluginJob{}, err
	}
	j.FinishedAt = finishedAt.String
	return j, nil
}

func (s *Store) queryPluginJobs(query string, args ...any) ([]PluginJob, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []PluginJob
	for rows.Next() {
		j, err := scanPluginJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}
