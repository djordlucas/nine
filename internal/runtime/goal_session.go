package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"nine/internal/memory"
)

// PursueIdleInterval is how often a "pursue" session's idle scheduler wakes
// to assess and act on its goal (docs/goal-sessions.md).
const PursueIdleInterval = 5 * time.Minute

// DefaultMaxGoalSessions is the concurrent "pursue" session cap used when
// DaemonConfig.MaxGoalSessions is unset or non-positive (docs/goal-sessions.md
// "Resource bounds").
const DefaultMaxGoalSessions = 10

// SetMaxGoalSessions configures the concurrent "pursue" session cap. A value
// <= 0 falls back to DefaultMaxGoalSessions. Must be called before
// SpawnGoalSession is reachable (i.e. before AgentBuilder.SetGoalSessionSpawnFn
// is wired up).
func (d *Daemon) SetMaxGoalSessions(n int) {
	d.maxGoalSessions = n
}

// SpawnGoalSession starts a background "pursue" session for goalID (see
// docs/goal-sessions.md). It is idempotent — if a session for goalID is
// already running, it returns (true, nil) without creating another. If the
// daemon is at its MaxGoalSessions cap, it returns (false, nil); the goal
// itself is still recorded by the caller (goal_create) either way.
func (d *Daemon) SpawnGoalSession(_ context.Context, goalID string) (bool, error) {
	// No explicit role: a plain pursue shell runs the default pursue role.
	plan, err := newIdleCapablePlan(goalID, "pursue", "", PursueIdleInterval)
	if err != nil {
		return false, err
	}
	return d.spawnPursueSession(goalID, plan)
}

// SpawnStandingSession starts (or ensures running) the pursue-shell session for
// a pre-defined standing agent (docs/predefined-agents.md). It mirrors
// SpawnGoalSession but seeds the plan with the configured work role, delegation
// opt-in, and wake trigger so the session runs under a narrowed role while
// keeping the pursue shell. aspects are additional stages the session carries
// alongside the pursue shell, each with its own wake cadence. The trigger is a
// cron schedule when schedule is non-empty, otherwise the fixed interval (interval <= 0 with no schedule uses
// PursueIdleInterval). Idempotent — a no-op if the session is already running;
// otherwise it (re)writes the plan, so config edits to role/delegates/trigger
// take effect on the next boot.
func (d *Daemon) SpawnStandingSession(_ context.Context, goalID, role string, delegates bool, interval time.Duration, schedule string, aspects []AspectDecl) (bool, error) {
	if interval <= 0 && schedule == "" {
		interval = PursueIdleInterval
	}
	plan, err := newStandingPursuePlan(goalID, role, delegates, interval, schedule, aspects)
	if err != nil {
		return false, err
	}
	return d.spawnPursueSession(goalID, plan)
}

// TeardownStandingSession stops a standing agent's pursue session and
// deactivates its session plan so it is never resumed again (subtractive
// reconciliation — docs/predefined-agents.md §7 v3). It stops the running
// worker if one exists and flips the plan (and its stages) out of "active",
// which makes planNeedsResume return false on every later boot. Idempotent: a
// no-op if the session isn't running and its plan is already inactive. The
// caller is responsible for archiving the goal itself, and for only calling
// this on config-origin goals.
func (d *Daemon) TeardownStandingSession(_ context.Context, goalID string) error {
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

	if d.plans == nil {
		return nil
	}
	plan, err := d.plans.SessionPlanGet(goalID)
	if err != nil {
		return fmt.Errorf("load session plan %s: %w", goalID, err)
	}
	if plan == nil || plan.Status != "active" {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	plan.Status = "archived"
	for i := range plan.Aspects {
		plan.Aspects[i].Status = "done"
		plan.Aspects[i].UpdatedAt = now
	}
	plan.UpdatedAt = now
	if err := d.plans.SessionPlanSave(plan); err != nil {
		return fmt.Errorf("deactivate session plan %s: %w", goalID, err)
	}
	return nil
}

// spawnPursueSession registers a background pursue-shell worker for goalID from
// the given seeded plan. It is the shared core of SpawnGoalSession and
// SpawnStandingSession: idempotent for an already-running session, bounded by
// MaxGoalSessions, and it persists the plan so ResumeSessions revives it on a
// later boot.
func (d *Daemon) spawnPursueSession(goalID string, plan *memory.SessionPlan) (bool, error) {
	d.mu.RLock()
	_, running := d.sessions[goalID]
	d.mu.RUnlock()
	if running {
		return true, nil
	}

	if d.plans == nil {
		return false, fmt.Errorf("pursue sessions require a configured plan store")
	}

	limit := d.maxGoalSessions
	if limit <= 0 {
		limit = DefaultMaxGoalSessions
	}
	if d.activeGoalSessionCount() >= limit {
		return false, nil
	}

	if err := d.plans.SessionPlanSave(plan); err != nil {
		return false, fmt.Errorf("create pursue session plan %s: %w", goalID, err)
	}

	var data []byte
	if d.ckpt != nil {
		data, _, _ = d.ckpt.Load(goalID) //nolint:errcheck // best-effort; makeAgentWorker handles a missing checkpoint
	}
	r := d.makeAgentWorker(goalID, data, false)
	d.mu.Lock()
	d.sessions[goalID] = r
	d.mu.Unlock()
	slog.Info("pursue session spawned", "goal_id", goalID)
	return true, nil
}

// activeGoalSessionCount returns the number of currently-running sessions
// whose plan has an active "pursue" stage (docs/session-plans.md
// "MaxGoalSessions counting").
func (d *Daemon) activeGoalSessionCount() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	n := 0
	for _, w := range d.sessions {
		if w.plan == nil {
			continue
		}
		for _, st := range w.plan.plan.Aspects {
			if st.Kind == "pursue" && st.Status == "active" {
				n++
				break
			}
		}
	}
	return n
}
