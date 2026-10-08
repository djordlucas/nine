package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"nine/internal/memory"
)

// Goal sessions and standing agents are process sessions (adr/process-sessions.md
// §3): the shipped `pursue` process, bound to the goal, owning a session whose id
// is the goal's. Spawning one writes its process; the process runner starts it.

// PursueIdleInterval is how often a goal session's pursue process wakes to
// assess and act on its goal (docs/goal-sessions.md).
const PursueIdleInterval = 5 * time.Minute

// DefaultMaxRunning is the cap on running processes when the daemon is given
// none: [processes] max_running's default.
const DefaultMaxRunning = 14

// ProcessBackend is what the daemon reads and writes about processes. The
// concrete implementation is *memory.Store.
type ProcessBackend interface {
	ProcessUpsertDefinition(p memory.Process) error
	ProcessGet(id string) (memory.Process, bool, error)
	ProcessList() ([]memory.Process, error)
	ProcessSetState(id, state string) (bool, error)
	ProcessStop(id, by string) (bool, error)
	ProcessDelete(id string) error
	ProcessesOfSession(sessionID string) ([]memory.Process, error)
	ProcessesRunning() (int, error)
	GoalUpdateStatus(id, status string) error
}

// ConfigureProcesses gives the daemon its process store, and wake, which asks
// the process runner for a pass now rather than at its next tick.
func (d *Daemon) ConfigureProcesses(store ProcessBackend, wake func()) {
	d.procs = store
	d.procWake = wake
}

// SetMaxRunning configures [processes] max_running, the cap a new goal
// session counts against. A value <= 0 falls back to DefaultMaxRunning.
func (d *Daemon) SetMaxRunning(n int) {
	d.maxRunning = n
}

// goalProcessID names the pursue process of a goal's session.
func goalProcessID(goalID string) string { return "goal:" + goalID }

// SpawnGoalSession starts the pursue process for goalID (docs/goal-sessions.md).
// It is idempotent: a goal that already has its process returns (true, nil). At
// the max_running cap it returns (false, nil); the goal itself is still
// recorded by the caller (goal_create) either way.
func (d *Daemon) SpawnGoalSession(_ context.Context, goalID string) (bool, error) {
	return d.spawnGoalProcess(memory.Process{
		ID: goalProcessID(goalID), Tool: "pursue", Mode: memory.ProcessLive,
		SessionID: goalID, Owner: true, Role: PursueRole, GoalID: goalID,
		IntervalSecs: int(PursueIdleInterval.Seconds()),
	}, nil)
}

// SpawnStandingSession writes the pursue process for a pre-defined standing
// agent (docs/predefined-agents.md): the configured work role, delegation
// opt-in and wake trigger. The trigger is a cron schedule when schedule is
// non-empty, otherwise the fixed interval (PursueIdleInterval when neither is
// set). Configuration owns the definition, so a config edit takes effect on the
// next pass; the run state is left alone.
func (d *Daemon) SpawnStandingSession(_ context.Context, goalID, role string, delegates bool, interval time.Duration, schedule string) (bool, error) {
	if interval <= 0 && schedule == "" {
		interval = PursueIdleInterval
	}
	if role == "" {
		role = PursueRole
	}
	owner := memory.Process{
		ID: goalProcessID(goalID), Tool: "pursue", Mode: memory.ProcessLive,
		SessionID: goalID, Owner: true, Role: role, Delegates: delegates, GoalID: goalID,
		Schedule: schedule,
	}
	if schedule == "" {
		owner.IntervalSecs = int(interval.Seconds())
	}
	return d.spawnGoalProcess(owner, nil)
}

// spawnGoalProcess writes a goal session's processes, bounded by the
// max_running cap, and asks the runner to start them.
func (d *Daemon) spawnGoalProcess(owner memory.Process, attached []memory.Process) (bool, error) {
	if d.procs == nil {
		return false, fmt.Errorf("goal sessions require a configured process store")
	}
	existing, found, err := d.procs.ProcessGet(owner.ID)
	if err != nil {
		return false, err
	}
	if !found {
		limit := d.maxRunning
		if limit <= 0 {
			limit = DefaultMaxRunning
		}
		n, err := d.procs.ProcessesRunning()
		if err != nil {
			return false, err
		}
		if n >= limit {
			return false, nil
		}
	}
	all := append([]memory.Process{owner}, attached...)
	for _, p := range all {
		if err := d.procs.ProcessUpsertDefinition(p); err != nil {
			return false, fmt.Errorf("write process %s: %w", p.ID, err)
		}
	}
	// Spawning is what makes the goal's session run, as it always was: a
	// process its goal or an earlier spawn left stopped runs again.
	if found && existing.State == memory.ProcessStopped {
		for _, p := range all {
			if _, err := d.procs.ProcessSetState(p.ID, memory.ProcessRunning); err != nil {
				return false, err
			}
		}
	}
	slog.Info("goal session spawned", "goal_id", owner.GoalID, "role", owner.Role)
	if d.procWake != nil {
		d.procWake()
	}
	return true, nil
}

// TeardownStandingSession removes a standing agent's processes, so it never
// runs again, and stops its session (subtractive reconciliation —
// adr/predefined-agents-design.md §7 v3). Idempotent. The caller archives the
// goal, and only calls this for config-origin goals.
func (d *Daemon) TeardownStandingSession(_ context.Context, goalID string) error {
	if d.procs != nil {
		procs, err := d.procs.ProcessesOfSession(goalID)
		if err != nil {
			return fmt.Errorf("list processes of %s: %w", goalID, err)
		}
		for _, p := range procs {
			if err := d.procs.ProcessDelete(p.ID); err != nil {
				return fmt.Errorf("remove process %s: %w", p.ID, err)
			}
		}
	}
	d.mu.Lock()
	w, running := d.sessions[goalID]
	if running {
		delete(d.sessions, goalID)
	}
	d.mu.Unlock()
	if running {
		w.stop()
		slog.Info("standing session stopped", "goal_id", goalID)
	}
	if d.procWake != nil {
		d.procWake()
	}
	return nil
}
