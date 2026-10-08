package runtime_test

import (
	"context"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/runtime"
)

// goalDaemon is a daemon with a process store and no socket: goal sessions
// are written as processes, and these tests read the store.
func goalDaemon(t *testing.T) (*runtime.Daemon, *memory.Store) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	d := runtime.New("", nil, nil)
	d.ConfigureProcesses(store, nil)
	return d, store
}

func TestSpawnGoalSessionRequiresAProcessStore(t *testing.T) {
	d := runtime.New("", nil, nil)
	if _, err := d.SpawnGoalSession(context.Background(), "goal-1"); err == nil {
		t.Fatal("SpawnGoalSession without a process store: want error, got nil")
	}
}

// A goal session is the goal's pursue process: live, owning the session whose
// id is the goal's, bound to the goal, waking every PursueIdleInterval.
// Spawning twice keeps one process.
func TestSpawnGoalSessionWritesItsProcessOnce(t *testing.T) {
	d, store := goalDaemon(t)
	for i := 0; i < 2; i++ {
		spawned, err := d.SpawnGoalSession(context.Background(), "goal-1")
		if err != nil || !spawned {
			t.Fatalf("SpawnGoalSession #%d = %v, %v", i+1, spawned, err)
		}
	}
	procs, err := store.ProcessesOfSession("goal-1")
	if err != nil || len(procs) != 1 {
		t.Fatalf("processes = %+v, %v; want exactly one", procs, err)
	}
	p := procs[0]
	if p.Tool != "pursue" || p.Mode != memory.ProcessLive || !p.Owner || p.GoalID != "goal-1" ||
		p.Role != runtime.PursueRole || p.IntervalSecs != int(runtime.PursueIdleInterval.Seconds()) {
		t.Errorf("process = %+v", p)
	}
}

// A standing agent's process carries its work role, delegation opt-in and
// trigger; a cron schedule replaces the interval.
func TestSpawnStandingSessionWritesRoleAndTrigger(t *testing.T) {
	d, store := goalDaemon(t)
	if _, err := d.SpawnStandingSession(context.Background(), "sec-watch", "monitor", true, 0, "0 9 * * 1-5", config.BudgetConfig{}); err != nil {
		t.Fatal(err)
	}
	p, found, err := store.ProcessGet("goal:sec-watch")
	if err != nil || !found {
		t.Fatalf("ProcessGet = %v, %v", found, err)
	}
	if p.Role != "monitor" || !p.Delegates || p.Schedule != "0 9 * * 1-5" || p.IntervalSecs != 0 {
		t.Errorf("process = %+v", p)
	}
}

// Teardown removes a standing agent's processes, so nothing starts it again.
func TestTeardownStandingSessionRemovesItsProcesses(t *testing.T) {
	d, store := goalDaemon(t)
	if _, err := d.SpawnStandingSession(context.Background(), "sec-watch", "monitor", false, time.Hour, "", config.BudgetConfig{}); err != nil {
		t.Fatal(err)
	}
	if err := d.TeardownStandingSession(context.Background(), "sec-watch"); err != nil {
		t.Fatal(err)
	}
	if procs, _ := store.ProcessesOfSession("sec-watch"); len(procs) != 0 {
		t.Errorf("processes after teardown = %+v", procs)
	}
	// Idempotent.
	if err := d.TeardownStandingSession(context.Background(), "sec-watch"); err != nil {
		t.Errorf("second teardown: %v", err)
	}
}

// At the MaxGoalSessions cap a new goal gets no process; an existing one is
// still spawnable.
func TestSpawnGoalSessionRespectsMaxGoalSessions(t *testing.T) {
	d, _ := goalDaemon(t)
	d.SetMaxRunning(1)

	if spawned, err := d.SpawnGoalSession(context.Background(), "goal-1"); err != nil || !spawned {
		t.Fatalf("SpawnGoalSession(goal-1) = (%v, %v), want (true, nil)", spawned, err)
	}
	if spawned, err := d.SpawnGoalSession(context.Background(), "goal-2"); err != nil || spawned {
		t.Errorf("SpawnGoalSession(goal-2) = (%v, %v), want (false, nil) at the cap", spawned, err)
	}
	if spawned, err := d.SpawnGoalSession(context.Background(), "goal-1"); err != nil || !spawned {
		t.Errorf("re-spawning goal-1 at the cap = (%v, %v), want (true, nil)", spawned, err)
	}
	if n := d.RunningProcessCountForTest(); n != 1 {
		t.Errorf("active goal sessions = %d, want 1", n)
	}
}

// Self-reflection is a process reconciled from configuration, in both
// directions: on writes it, off stops it, on again restarts it.
func TestReconcileSelfReflection(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.ReconcileSelfReflection(store, 2*time.Minute); err != nil {
		t.Fatal(err)
	}
	p, found, _ := store.ProcessGet(runtime.SelfReflectionAgentID)
	if !found || p.Tool != "reflect" || p.Role != runtime.ReflectionRole || p.IntervalSecs != 120 ||
		p.SessionID != runtime.SelfReflectionAgentID || p.State != memory.ProcessRunning {
		t.Fatalf("process = %+v (found %v)", p, found)
	}

	if err := runtime.ReconcileSelfReflection(store, 0); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := store.ProcessGet(runtime.SelfReflectionAgentID); p.State != memory.ProcessStopped {
		t.Errorf("turned off: state = %q, want stopped", p.State)
	}

	if err := runtime.ReconcileSelfReflection(store, time.Hour); err != nil {
		t.Fatal(err)
	}
	p, _, _ = store.ProcessGet(runtime.SelfReflectionAgentID)
	if p.State != memory.ProcessRunning || p.IntervalSecs != 3600 {
		t.Errorf("turned on again: %+v, want running at the new cadence", p)
	}
}
