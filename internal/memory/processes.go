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
	ReportTo  string `json:"report_to,omitempty"`
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
	_, err := s.db.Exec(
		`INSERT INTO processes(id, tool, args, interval_secs, schedule, generated, report_to, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   tool=excluded.tool, args=excluded.args,
		   interval_secs=excluded.interval_secs, schedule=excluded.schedule,
		   report_to=excluded.report_to, updated_at=excluded.updated_at`,
		t.ID, t.Tool, args, t.IntervalSecs, t.Schedule, t.Generated, t.ReportTo, nowText())
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

// ProcessesDue returns the runs whose next call is due now.
//
// Only `running` — a stopped run is not due, and a `failing` one is, because
// failing means "still trying, on a backed-off cadence" rather than "given up".
func (s *Store) ProcessesDue() ([]Process, error) {
	return s.queryProcesses(
		`SELECT `+processColumns+`
		 FROM processes
		 WHERE state IN (?, ?) AND (next_at = '' OR next_at <= ?)
		 ORDER BY id`,
		ProcessRunning, ProcessFailing, nowText())
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
			 SET state=?, failures=0, last_error='', cursor='', next_at='', updated_at=?
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

// ProcessDelete removes a process outright. Used when a generated one
// is withdrawn; a config-declared one that leaves the file is *not* deleted, only
// left unreconciled, matching how removing a standing agent behaves.
func (s *Store) ProcessDelete(id string) error {
	_, err := s.db.Exec(`DELETE FROM processes WHERE id = ?`, id)
	return err
}

const processColumns = `id, tool, args, interval_secs, schedule, state, cursor,
	        calls, cycles, failures, last_error, last_call_at, next_at, generated,
	        report_to, created_at, updated_at`

func scanProcess(row rowScanner) (Process, error) {
	var t Process
	err := row.Scan(&t.ID, &t.Tool, &t.Args, &t.IntervalSecs, &t.Schedule, &t.State,
		&t.Cursor, &t.Calls, &t.Cycles, &t.Failures, &t.LastError, &t.LastCallAt,
		&t.NextAt, &t.Generated, &t.ReportTo, &t.CreatedAt, &t.UpdatedAt)
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
