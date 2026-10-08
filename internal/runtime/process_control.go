package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"nine/internal/memory"
)

// The roster and control of processes (adr/process-sessions.md §9): what a
// model's process tools and the operator's `nine process` both read and act
// through. The difference between the two is who acts, which the start rule
// turns on.

// Who acts on a process. A stop records it, and it decides who may start the
// process again.
const (
	ByOperator = "operator"
	ByModel    = "model"
)

// ProcessStatus is one process as the roster reports it.
type ProcessStatus struct {
	ID        string `json:"id"`
	Tool      string `json:"tool"`
	Mode      string `json:"mode"`
	State     string `json:"state"`
	StoppedBy string `json:"stopped_by,omitempty"`
	StoppedAt string `json:"stopped_at,omitempty"`
	Trigger   string `json:"trigger"`
	// Session is the session a live process drives; Attached marks one that
	// runs in another process's session.
	Session  string `json:"session,omitempty"`
	Attached bool   `json:"attached,omitempty"`
	Goal     string `json:"goal,omitempty"`
	ReportTo string `json:"report_to,omitempty"`
	Role     string `json:"role,omitempty"`

	Budget ProcessBudgetStatus `json:"budget"`

	Calls      int    `json:"calls,omitempty"`
	Cycles     int    `json:"cycles,omitempty"`
	Failures   int    `json:"failures,omitempty"`
	LastError  string `json:"last_error,omitempty"`
	LastCallAt string `json:"last_call_at,omitempty"`
	NextAt     string `json:"next_at,omitempty"`
	Generated  bool   `json:"generated,omitempty"`

	Recent []StandingLogEntry `json:"recent,omitempty"`
}

// ProcessBudgetStatus is a process's budget and what its day has used.
type ProcessBudgetStatus struct {
	Turns        int    `json:"turns"`
	TurnsPerDay  int    `json:"turns_per_day"`
	Tokens       int    `json:"tokens"`
	TokensPerDay int    `json:"tokens_per_day"`
	ResetsAt     string `json:"resets_at,omitempty"`
}

// SetMaxRunning sets [processes] max_running, which a start counts against.
// <= 0 is the default.
func (r *StandingRunner) SetMaxRunning(n int) {
	if r != nil {
		r.maxRunning = n
	}
}

func (r *StandingRunner) statusOf(p memory.Process, recent []StandingLogEntry) ProcessStatus {
	lim := r.budgetOf(p)
	b := ProcessBudgetStatus{
		Turns: p.UsageTurns, TurnsPerDay: lim.TurnsPerDay,
		Tokens: p.UsageTokens, TokensPerDay: lim.TokensPerDay,
	}
	if at, ok := budgetResetAt(p); ok {
		b.ResetsAt = at.UTC().Format(time.RFC3339)
	}
	return ProcessStatus{
		ID: p.ID, Tool: p.Tool, Mode: p.Mode, State: p.State,
		StoppedBy: p.StoppedBy, StoppedAt: p.StoppedAt, Trigger: triggerText(p),
		Session: p.SessionID, Attached: p.SessionID != "" && !p.Owner,
		Goal: p.GoalID, ReportTo: p.ReportTo, Role: p.Role, Budget: b,
		Calls: p.Calls, Cycles: p.Cycles, Failures: p.Failures, LastError: p.LastError,
		LastCallAt: p.LastCallAt, NextAt: p.NextAt, Generated: p.Generated,
		Recent: recent,
	}
}

// Processes returns every process, by id.
func (r *StandingRunner) Processes() ([]ProcessStatus, error) {
	if r == nil {
		return nil, nil
	}
	rows, err := r.store.ProcessList()
	if err != nil {
		return nil, err
	}
	out := make([]ProcessStatus, 0, len(rows))
	for _, p := range rows {
		out = append(out, r.statusOf(p, nil))
	}
	return out, nil
}

// ProcessOf returns one process with up to n recent activity entries.
func (r *StandingRunner) ProcessOf(id string, n int) (ProcessStatus, bool, error) {
	if r == nil {
		return ProcessStatus{}, false, nil
	}
	p, ok, err := r.store.ProcessGet(id)
	if err != nil || !ok {
		return ProcessStatus{}, ok, err
	}
	return r.statusOf(p, r.log.recent(id, n)), true, nil
}

// ErrUnknownProcess is the error for an id no process has.
var ErrUnknownProcess = errors.New("no such process")

func (r *StandingRunner) mustGet(id string) (memory.Process, error) {
	if r == nil {
		return memory.Process{}, fmt.Errorf("processes are not enabled here")
	}
	p, ok, err := r.store.ProcessGet(id)
	if err != nil {
		return memory.Process{}, err
	}
	if !ok {
		return memory.Process{}, fmt.Errorf("%w: %q (process_list shows them)", ErrUnknownProcess, id)
	}
	return p, nil
}

// StartProcess starts a stopped process on behalf of by. The operator may start
// any process. A model may start any stopped process except one the operator
// stopped, one its goal stopped, or one its budget paused until the budget
// resets (adr/process-sessions.md §9). Either way a start counts against
// max_running.
func (r *StandingRunner) StartProcess(id, by string) error {
	p, err := r.mustGet(id)
	if err != nil {
		return err
	}
	if p.State == memory.ProcessRunning {
		return fmt.Errorf("process %s is already running", id)
	}
	if p.State == memory.ProcessStopped && by == ByModel {
		if err := modelMayStart(p, time.Now()); err != nil {
			return err
		}
	}
	if p.State == memory.ProcessStopped {
		limit := r.maxRunning
		if limit <= 0 {
			limit = DefaultMaxRunning
		}
		n, err := r.store.ProcessesRunning()
		if err != nil {
			return err
		}
		if n >= limit {
			// "Stop one first" here led models to stop another process they had
			// been told to leave running. Which process to give up is the
			// person's call, so the message sends the model back to them.
			return fmt.Errorf("process %s not started: %d processes are running, the maximum on this instance ([processes] max_running). Report this to the person rather than stopping another process unless they asked you to", id, n)
		}
	}
	// Starting a failing process means "try again now": its failures clear and
	// it runs at once rather than waiting out its backoff.
	if _, err := r.store.ProcessSetState(id, memory.ProcessRunning); err != nil {
		return err
	}
	if p.StoppedBy == "budget" {
		if err := r.store.ProcessUsageReset(id); err != nil {
			slog.Warn("process start: reset its budget day", "id", id, "err", err)
		}
	}
	journalTransition(r.store, id, evStandingStarted, map[string]any{"by": by})
	r.log.add(id, "started", "by "+by)
	slog.Info("process started", "id", id, "by", by)
	r.Wake()
	return nil
}

// modelMayStart applies the start rule's refusals, each naming its reason.
func modelMayStart(p memory.Process, now time.Time) error {
	switch p.StoppedBy {
	case ByOperator:
		return fmt.Errorf("process %s was stopped by the operator%s; only the operator can start it", p.ID, onDate(p.StoppedAt))
	case "goal":
		return fmt.Errorf("process %s was stopped by its goal %s, whose status decides whether it runs; reactivate the goal (goal_update_status) instead", p.ID, p.GoalID)
	case "budget":
		if at, ok := budgetResetAt(p); ok && at.After(now) {
			return fmt.Errorf("process %s is paused by its budget until %s, when it runs again by itself", p.ID, at.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

func onDate(at string) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return ""
	}
	return " on " + t.UTC().Format("2006-01-02")
}

// StopProcess stops a process on behalf of by, which records who stopped it.
// Its instance closes on the next pass.
func (r *StandingRunner) StopProcess(id, by string) error {
	p, err := r.mustGet(id)
	if err != nil {
		return err
	}
	if p.State == memory.ProcessStopped {
		return fmt.Errorf("process %s is already stopped (by %s)", id, p.StoppedBy)
	}
	if _, err := r.store.ProcessStop(id, by); err != nil {
		return err
	}
	journalTransition(r.store, id, evStandingStopped, map[string]any{"by": by})
	r.log.add(id, "stopped", "by "+by)
	slog.Info("process stopped", "id", id, "by", by)
	r.Wake()
	return nil
}

// SendProcess gives a live process a message as its next trigger, labelled
// with its sender. Unlike a pipe, a send the process cannot take now is
// refused rather than sent to the human feed: the sender is a model that can
// try again.
//
// from names the sender in the label: "conversation <id>", "the operator".
func (r *StandingRunner) SendProcess(id, text, from string) error {
	p, err := r.mustGet(id)
	if err != nil {
		return err
	}
	switch {
	case p.Mode != memory.ProcessLive:
		return fmt.Errorf("process %s is a slice process, called on its trigger with no session to message", id)
	case p.State == memory.ProcessStopped:
		return fmt.Errorf("process %s is stopped; start it first", id)
	}
	handled, delivered := r.Deliver(p.SessionID, fmt.Sprintf("[From %s: %s]", from, text), from)
	switch {
	case !handled:
		return fmt.Errorf("process %s is not running yet; try again in a moment", id)
	case !delivered:
		return fmt.Errorf("process %s is busy with a trigger and did not take the message; send it again later", id)
	}
	return nil
}

// modelProcesses is the process tools' view: every action is a model's.
type modelProcesses struct{ r *StandingRunner }

func (m modelProcesses) ProcessList() (any, error) { return m.r.Processes() }

func (m modelProcesses) ProcessShow(id string) (any, error) {
	st, ok, err := m.r.ProcessOf(id, 10)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: %q (process_list shows them)", ErrUnknownProcess, id)
	}
	return st, nil
}

func (m modelProcesses) ProcessSend(id, text, agentID string) error {
	return m.r.SendProcess(id, text, "conversation "+agentID)
}

func (m modelProcesses) ProcessStart(id string) error { return m.r.StartProcess(id, ByModel) }

func (m modelProcesses) ProcessStop(id string) error { return m.r.StopProcess(id, ByModel) }
