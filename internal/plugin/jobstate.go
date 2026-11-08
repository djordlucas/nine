package plugin

// JobState is the lifecycle state of a job row. The plugin-side job registry
// (R-PLUG.12's envelope methods) was removed with the envelope at P4; these
// constants stay because the daemon's job store (internal/memory/jobs.go) and
// its sweeper render rows with them, and MCP servers will carry jobs in a
// `_meta` extension on the tools/call result (adr/modular_nine.md §2) that
// reuses these states. Until then no plugin can start a job: the manager's job
// methods fail closed.
type JobState string

const (
	JobQueued    JobState = "queued"
	JobRunning   JobState = "running"
	JobDone      JobState = "done"
	JobFailed    JobState = "failed"
	JobCancelled JobState = "cancelled"
)

// JobStatus is the shape of a plugin job status answer. The manager's job
// methods fail closed while no plugin advertises async_jobs, but the daemon's
// sweeper renders rows with this shape, so it stays.
type JobStatus struct {
	State    JobState `json:"state"`
	Progress string   `json:"progress,omitempty"`
	Output   string   `json:"output,omitempty"`
	Error    string   `json:"error,omitempty"`
}
