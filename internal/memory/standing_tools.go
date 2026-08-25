package memory

import (
	"database/sql"
	"time"
)

// Standing-run states.
//
// There is no terminal state here, and that is the difference from a job. A
// standing run is stopped or it is going; `failing` is a going run that keeps
// erroring, kept distinct so an operator can see the difference between "quiet"
// and "broken" without reading a log.
const (
	StandingRunning = "running"
	StandingStopped = "stopped"
	StandingFailing = "failing"
)

// StandingTool is one indefinitely-running tool (adr/standing-tools.md).
//
// The split of ownership matters and mirrors what standing agents already do
// (docs/predefined-agents.md): **configuration owns the definition — tool, args,
// trigger — and the runtime owns the run state.** So a standing tool an operator
// stopped stays stopped across a restart, the way a finished standing agent stays
// finished, and editing the file does not silently restart something.
type StandingTool struct {
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
	// WakeAgent, when set, makes this run a *condition trigger*: a cycle that
	// produces output wakes that agent now instead of leaving a note on the human
	// feed. It is the one way a standing tool may reach an agent, and only
	// because an operator wrote the link in their own config.
	WakeAgent string `json:"wake_agent,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// StandingToolUpsertDefinition writes the config-owned half of a standing tool
// and leaves the run state alone.
//
// This is reconciliation, and the columns it does *not* touch are the point:
// state, cursor, calls, failures. Editing nine.toml adjusts what the tool does;
// it does not restart a run an operator stopped, exactly as a standing agent's
// config edit does not reactivate a finished goal.
func (s *Store) StandingToolUpsertDefinition(t StandingTool) error {
	args := t.Args
	if args == "" {
		args = "{}"
	}
	_, err := s.db.Exec(
		`INSERT INTO standing_tools(id, tool, args, interval_secs, schedule, generated, wake_agent, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   tool=excluded.tool, args=excluded.args,
		   interval_secs=excluded.interval_secs, schedule=excluded.schedule,
		   wake_agent=excluded.wake_agent, updated_at=excluded.updated_at`,
		t.ID, t.Tool, args, t.IntervalSecs, t.Schedule, t.Generated, t.WakeAgent, nowText())
	return err
}

// StandingToolGet returns one standing tool, or (…, false, nil).
func (s *Store) StandingToolGet(id string) (StandingTool, bool, error) {
	row := s.db.QueryRow(`SELECT `+standingColumns+` FROM standing_tools WHERE id = ?`, id)
	t, err := scanStandingTool(row)
	if err == sql.ErrNoRows {
		return StandingTool{}, false, nil
	}
	if err != nil {
		return StandingTool{}, false, err
	}
	return t, true, nil
}

// StandingToolList returns every standing tool, by id.
func (s *Store) StandingToolList() ([]StandingTool, error) {
	return s.queryStandingTools(`SELECT ` + standingColumns + ` FROM standing_tools ORDER BY id`)
}

// StandingToolsDue returns the runs whose next call is due now.
//
// Only `running` — a stopped run is not due, and a `failing` one is, because
// failing means "still trying, on a backed-off cadence" rather than "given up".
func (s *Store) StandingToolsDue() ([]StandingTool, error) {
	return s.queryStandingTools(
		`SELECT `+standingColumns+`
		 FROM standing_tools
		 WHERE state IN (?, ?) AND (next_at = '' OR next_at <= ?)
		 ORDER BY id`,
		StandingRunning, StandingFailing, nowText())
}

// StandingToolAdvance records a call that asked to continue: the cursor it
// returned, and when to call it again. Failures reset — a call that produced a
// continuation worked.
func (s *Store) StandingToolAdvance(id, cursor string, nextAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE standing_tools
		 SET cursor=?, calls=calls+1, failures=0, last_error='', state=?,
		     last_call_at=?, next_at=?, updated_at=?
		 WHERE id=? AND state <> ?`,
		cursor, StandingRunning, nowText(), writeTime(nextAt), nowText(), id, StandingStopped)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StandingToolCompleteCycle records a finished pass: the cursor resets, the cycle
// count moves, and the trigger decides when the next one starts.
func (s *Store) StandingToolCompleteCycle(id string, nextAt time.Time) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE standing_tools
		 SET cursor='', calls=calls+1, cycles=cycles+1, failures=0, last_error='',
		     state=?, last_call_at=?, next_at=?, updated_at=?
		 WHERE id=? AND state <> ?`,
		StandingRunning, nowText(), writeTime(nextAt), nowText(), id, StandingStopped)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StandingToolFail records a failed call: the consecutive count rises, the cursor
// is discarded (it was produced by a call that did not complete), and the caller
// decides the backed-off next_at and whether the breaker has tripped.
func (s *Store) StandingToolFail(id, errMsg string, nextAt time.Time, state string) (bool, error) {
	res, err := s.db.Exec(
		`UPDATE standing_tools
		 SET cursor='', calls=calls+1, failures=failures+1, last_error=?,
		     state=?, last_call_at=?, next_at=?, updated_at=?
		 WHERE id=? AND state <> ?`,
		errMsg, state, nowText(), writeTime(nextAt), nowText(), id, StandingStopped)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StandingToolSetState stops or starts a run. Starting clears the failure
// history and schedules an immediate first call, so `start` after a fix means
// "try again now" rather than "wait out the backoff".
func (s *Store) StandingToolSetState(id, state string) (bool, error) {
	var res sql.Result
	var err error
	if state == StandingRunning {
		res, err = s.db.Exec(
			`UPDATE standing_tools
			 SET state=?, failures=0, last_error='', cursor='', next_at='', updated_at=?
			 WHERE id=?`, state, nowText(), id)
	} else {
		res, err = s.db.Exec(
			`UPDATE standing_tools SET state=?, updated_at=? WHERE id=?`, state, nowText(), id)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StandingToolDelete removes a standing tool outright. Used when a generated one
// is withdrawn; a config-declared one that leaves the file is *not* deleted, only
// left unreconciled, matching how removing a standing agent behaves.
func (s *Store) StandingToolDelete(id string) error {
	_, err := s.db.Exec(`DELETE FROM standing_tools WHERE id = ?`, id)
	return err
}

const standingColumns = `id, tool, args, interval_secs, schedule, state, cursor,
	        calls, cycles, failures, last_error, last_call_at, next_at, generated,
	        wake_agent, created_at, updated_at`

func scanStandingTool(row rowScanner) (StandingTool, error) {
	var t StandingTool
	err := row.Scan(&t.ID, &t.Tool, &t.Args, &t.IntervalSecs, &t.Schedule, &t.State,
		&t.Cursor, &t.Calls, &t.Cycles, &t.Failures, &t.LastError, &t.LastCallAt,
		&t.NextAt, &t.Generated, &t.WakeAgent, &t.CreatedAt, &t.UpdatedAt)
	return t, err
}

func (s *Store) queryStandingTools(query string, args ...any) ([]StandingTool, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StandingTool
	for rows.Next() {
		t, err := scanStandingTool(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
