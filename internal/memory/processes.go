package memory

import (
	"database/sql"
	"time"
)

// Process states.
//
// There is no terminal state here, and that is the difference from a job. A
// process is stopped or it is going; `failing` is a going run that keeps
// erroring, kept distinct so an operator can see the difference between "quiet"
// and "broken" without reading a log.
// Process modes (adr/process-sessions.md §2). A live process's instance runs
// until it is stopped and drives its session; a slice process is called per
// trigger, as standing tools always were.
const (
	ProcessLive  = "live"
	ProcessSlice = "slice"
)

const (
	ProcessRunning = "running"
	ProcessStopped = "stopped"
	ProcessFailing = "failing"
)

// Process is one tool that runs indefinitely on its triggers
// (adr/process-sessions.md). Until the process runner lands, every row is what
// was a standing tool (adr/standing-tools.md): a resumable tool run on a cadence.
//
// The split of ownership matters and mirrors what standing agents already do
// (docs/predefined-agents.md): **configuration owns the definition — tool, args,
// trigger — and the runtime owns the run state.** So a standing tool an operator
// stopped stays stopped across a restart, the way a finished standing agent stays
// finished, and editing the file does not silently restart something.
type Process struct {
	ID   string `json:"id"`
	Tool string `json:"tool"`
	Args string `json:"args,omitempty"`
	// IntervalSecs and Schedule are the trigger, mutually exclusive: the cadence
	// between *cycles*. They reuse the parsing standing agents use
	// (docs/scheduling.md), limits included.
	IntervalSecs int    `json:"interval_secs,omitempty"`
	Schedule     string `json:"schedule,omitempty"`

	State string `json:"state"`
	// Cursor is the resume point *within* the current cycle, and resets when a
	// cycle completes — the next cycle starts from the tool's own beginning.
	Cursor string `json:"cursor,omitempty"`
	Calls  int    `json:"calls,omitempty"`
	Cycles int    `json:"cycles,omitempty"`
	// Failures counts *consecutive* failures, so it is the circuit breaker's
	// input and resets on any success.
	Failures   int    `json:"failures,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	LastCallAt string `json:"last_call_at,omitempty"`
	NextAt     string `json:"next_at,omitempty"`
	Generated  bool   `json:"generated,omitempty"`
	// ReportTo, when set, pipes this process into that agent's session: a cycle
	// that produces output wakes it now instead of leaving a note on the human
	// feed. A condition trigger is such a pipe. It is the one way a process may
	// reach an agent, and only because an operator wrote the link in their own
	// config.
	ReportTo string `json:"report_to,omitempty"`

	// Mode is ProcessLive or ProcessSlice.
	Mode string `json:"mode"`
	// SessionID is the session a live process drives; empty for a slice one.
	SessionID string `json:"session_id,omitempty"`
	// Owner marks the process that owns its session and sets its role; an
	// attached process's turns run under the owner's role.
	Owner bool `json:"owner"`
	// Role and Delegates are what the session runs under, when this process
	// owns it.
	Role      string `json:"role,omitempty"`
	Delegates bool   `json:"delegates,omitempty"`
	// GoalID binds the process to a goal: the goal's status decides whether it
	// runs.
	GoalID string `json:"goal_id,omitempty"`
	// StoppedBy says who stopped a stopped process — "operator", "goal",
	// "model", "self", "budget" — which decides who may start it again.
	StoppedBy string `json:"stopped_by,omitempty"`
	StoppedAt string `json:"stopped_at,omitempty"`

	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ProcessUpsertDefinition writes the config-owned half of a process
// and leaves the run state alone.
//
// This is reconciliation, and the columns it does *not* touch are the point:
// state, cursor, calls, failures. Editing nine.toml adjusts what the tool does;
// it does not restart a run an operator stopped, exactly as a standing agent's
// config edit does not reactivate a finished goal.
func (s *Store) ProcessUpsertDefinition(t Process) error {
	args := t.Args
	if args == "" {
		args = "{}"
	}
	mode := t.Mode
	if mode == "" {
		mode = ProcessSlice
	}
	_, err := s.db.Exec(
		`INSERT INTO processes(id, tool, args, interval_secs, schedule, generated, report_to,
		                       mode, session_id, owner, role, delegates, goal_id, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   tool=excluded.tool, args=excluded.args,
		   interval_secs=excluded.interval_secs, schedule=excluded.schedule,
		   report_to=excluded.report_to, mode=excluded.mode, session_id=excluded.session_id,
		   owner=excluded.owner, role=excluded.role, delegates=excluded.delegates,
		   goal_id=excluded.goal_id, updated_at=excluded.updated_at`,
		t.ID, t.Tool, args, t.IntervalSecs, t.Schedule, t.Generated, t.ReportTo,
		mode, t.SessionID, t.Owner, t.Role, t.Delegates, t.GoalID, nowText())
	return err
}

// ProcessGet returns one process, or (…, false, nil).
func (s *Store) ProcessGet(id string) (Process, bool, error) {
	row := s.db.QueryRow(`SELECT `+processColumns+` FROM processes WHERE id = ?`, id)
	t, err := scanProcess(row)
	if err == sql.ErrNoRows {
		return Process{}, false, nil
	}
	if err != nil {
		return Process{}, false, err
	}
	return t, true, nil
}

// ProcessList returns every process, by id.
func (s *Store) ProcessList() ([]Process, error) {
	return s.queryProcesses(`SELECT ` + processColumns + ` FROM processes ORDER BY id`)
}

// ProcessesDue returns the slice processes whose next call is due now. Live
// processes are not called per trigger, so they are never due here.
//
// Only `running` — a stopped run is not due, and a `failing` one is, because
// failing means "still trying, on a backed-off cadence" rather than "given up".
func (s *Store) ProcessesDue() ([]Process, error) {
	return s.queryProcesses(
		`SELECT `+processColumns+`
		 FROM processes
		 WHERE mode = ? AND state IN (?, ?) AND (next_at = '' OR next_at <= ?)
		 ORDER BY id`,
		ProcessSlice, ProcessRunning, ProcessFailing, nowText())
}

// ProcessesLive returns the live processes that should be running: running,
// or failing and due a restart.
func (s *Store) ProcessesLive() ([]Process, error) {
	return s.queryProcesses(
		`SELECT `+processColumns+`
		 FROM processes
		 WHERE mode = ? AND state IN (?, ?)
		 ORDER BY id`,
		ProcessLive, ProcessRunning, ProcessFailing)
}

// ProcessSetNextAt records when a live process's clock next fires.
func (s *Store) ProcessSetNextAt(id string, nextAt time.Time) error {
	_, err := s.db.Exec(`UPDATE processes SET next_at=?, updated_at=? WHERE id=?`,
		writeTime(nextAt), nowText(), id)
	return err
}

// ProcessRecovered clears a live process's failure history once it runs again,
// returning a failing one to running.
func (s *Store) ProcessRecovered(id string) error {
	_, err := s.db.Exec(
		`UPDATE processes SET state=?, failures=0, last_error='', updated_at=?
		 WHERE id=? AND state IN (?, ?)`,
		ProcessRunning, nowText(), id, ProcessRunning, ProcessFailing)
	return err
}

// ProcessAdvance records a call that asked to continue: the cursor it
// returned, and when to call it again. Failures reset — a call that produced a
// continuation worked.
func (s *Store) ProcessAdvance(id, cursor string, nextAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE processes
		 SET cursor=?, calls=calls+1, failures=0, last_error='', state=?,
		     last_call_at=?, next_at=?, updated_at=?
		 WHERE id=? AND state <> ?`,
		cursor, ProcessRunning, nowText(), writeTime(nextAt), nowText(), id, ProcessStopped)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ProcessCompleteCycle records a finished pass: the cursor resets, the cycle
// count moves, and the trigger decides when the next one starts.
func (s *Store) ProcessCompleteCycle(id string, nextAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE processes
		 SET cursor='', calls=calls+1, cycles=cycles+1, failures=0, last_error='',
		     state=?, last_call_at=?, next_at=?, updated_at=?
		 WHERE id=? AND state <> ?`,
		ProcessRunning, nowText(), writeTime(nextAt), nowText(), id, ProcessStopped)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ProcessFail records a failed call: the consecutive count rises, the cursor
// is discarded (it was produced by a call that did not complete), and the caller
// decides the backed-off next_at and whether the breaker has tripped.
func (s *Store) ProcessFail(id, errMsg string, nextAt time.Time, state string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE processes
		 SET cursor='', calls=calls+1, failures=failures+1, last_error=?,
		     state=?, last_call_at=?, next_at=?, updated_at=?
		 WHERE id=? AND state <> ?`,
		errMsg, state, nowText(), writeTime(nextAt), nowText(), id, ProcessStopped)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ProcessSetState stops or starts a run. Starting clears the failure
// history and schedules an immediate first call, so `start` after a fix means
// "try again now" rather than "wait out the backoff".
func (s *Store) ProcessSetState(id, state string) (bool, error) {
	var res sql.Result
	var err error
	if state == ProcessRunning {
		res, err = s.db.Exec(
			`UPDATE processes
			 SET state=?, failures=0, last_error='', cursor='', next_at='',
			     stopped_by='', stopped_at='', updated_at=?
			 WHERE id=?`, state, nowText(), id)
	} else {
		res, err = s.db.Exec(
			`UPDATE processes SET state=?, updated_at=? WHERE id=?`, state, nowText(), id)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ProcessStop stops a process and records who stopped it, which decides who
// may start it again (adr/process-sessions.md §9).
func (s *Store) ProcessStop(id, by string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE processes SET state=?, stopped_by=?, stopped_at=?, updated_at=? WHERE id=?`,
		ProcessStopped, by, nowText(), nowText(), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ProcessesStoppedBy returns the stopped processes a given party stopped.
func (s *Store) ProcessesStoppedBy(by string) ([]Process, error) {
	return s.queryProcesses(
		`SELECT `+processColumns+` FROM processes WHERE state = ? AND stopped_by = ? ORDER BY id`,
		ProcessStopped, by)
}

// ProcessesOfSession returns the processes driving a session: its owner and
// any attached to it.
func (s *Store) ProcessesOfSession(sessionID string) ([]Process, error) {
	return s.queryProcesses(
		`SELECT `+processColumns+` FROM processes WHERE session_id = ? ORDER BY owner DESC, id`,
		sessionID)
}

// ProcessDelete removes a process outright. Used when a generated one
// is withdrawn; a config-declared one that leaves the file is *not* deleted, only
// left unreconciled, matching how removing a standing agent behaves.
func (s *Store) ProcessDelete(id string) error {
	_, err := s.db.Exec(`DELETE FROM processes WHERE id = ?`, id)
	return err
}

const processColumns = `id, tool, args, interval_secs, schedule, state, cursor,
	        calls, cycles, failures, last_error, last_call_at, next_at, generated,
	        report_to, mode, session_id, owner, role, delegates, goal_id, stopped_by, stopped_at,
	        created_at, updated_at`

func scanProcess(row rowScanner) (Process, error) {
	var t Process
	err := row.Scan(&t.ID, &t.Tool, &t.Args, &t.IntervalSecs, &t.Schedule, &t.State,
		&t.Cursor, &t.Calls, &t.Cycles, &t.Failures, &t.LastError, &t.LastCallAt,
		&t.NextAt, &t.Generated, &t.ReportTo, &t.Mode, &t.SessionID, &t.Owner, &t.Role,
		&t.Delegates, &t.GoalID, &t.StoppedBy, &t.StoppedAt, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (s *Store) queryProcesses(query string, args ...any) ([]Process, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Process
	for rows.Next() {
		t, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
