package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
)

// controlSetup is a runner over a store holding one stopped live process.
func controlSetup(t *testing.T, row memory.Process, stoppedBy string) (*StandingRunner, *memory.Store) {
	t.Helper()
	r, store, _ := liveSetup(t, row)
	if stoppedBy != "" {
		if _, err := store.ProcessStop(row.ID, stoppedBy); err != nil {
			t.Fatal(err)
		}
	}
	return r, store
}

// The start rule (adr/process-sessions.md §9): a model may start a stopped
// process unless the operator, its goal, or its budget stopped it; each
// refusal names its reason. The operator may start any.
func TestStartRule(t *testing.T) {
	for _, tc := range []struct {
		stoppedBy string
		modelOK   bool
		reason    string
	}{
		{ByOperator, false, "only the operator"},
		{"goal", false, "reactivate the goal"},
		{"budget", false, "paused by its budget"},
		{ByModel, true, ""},
		{"self", true, ""},
	} {
		t.Run(tc.stoppedBy, func(t *testing.T) {
			r, store := controlSetup(t, memory.Process{ID: "p", SessionID: "p", GoalID: "g"}, tc.stoppedBy)
			if tc.stoppedBy == "budget" {
				if err := store.ProcessUsageAdd("p", 1); err != nil {
					t.Fatal(err)
				}
			}
			err := r.StartProcess("p", ByModel)
			if tc.modelOK {
				if err != nil {
					t.Fatalf("a model's start refused: %v", err)
				}
				if p := processRow(t, store, "p"); p.State != memory.ProcessRunning || p.StoppedBy != "" {
					t.Errorf("after start: state %q, stopped_by %q", p.State, p.StoppedBy)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("a model's start = %v, want a refusal naming %q", err, tc.reason)
			}
			if err := r.StartProcess("p", ByOperator); err != nil {
				t.Fatalf("the operator's start refused: %v", err)
			}
		})
	}
}

// A process its budget paused may be started by a model once its day is
// over, and the start begins a new day.
func TestStartAfterTheBudgetDay(t *testing.T) {
	r, store := controlSetup(t, memory.Process{ID: "p", SessionID: "p"}, "budget")
	if err := store.ProcessUsageAdd("p", 1); err != nil {
		t.Fatal(err)
	}
	p := processRow(t, store, "p")
	if err := modelMayStart(p, time.Now().Add(budgetDay+time.Minute)); err != nil {
		t.Fatalf("a start after the day was refused: %v", err)
	}
	if err := store.ProcessUsageReset("p"); err != nil { // the day is over
		t.Fatal(err)
	}
	if err := r.StartProcess("p", ByModel); err != nil {
		t.Fatal(err)
	}
}

// A start counts against max_running; a running or unknown process is
// refused with its reason.
func TestStartRefusals(t *testing.T) {
	r, store := controlSetup(t, memory.Process{ID: "p", SessionID: "p"}, ByModel)
	if err := store.ProcessUpsertDefinition(memory.Process{ID: "other", Tool: "x", IntervalSecs: 60}); err != nil {
		t.Fatal(err)
	}
	r.SetMaxRunning(1)
	if err := r.StartProcess("p", ByModel); err == nil || !strings.Contains(err.Error(), "max_running") {
		t.Errorf("start at the cap = %v, want max_running to refuse", err)
	}
	r.SetMaxRunning(0)
	if err := r.StartProcess("p", ByModel); err != nil {
		t.Fatal(err)
	}
	if err := r.StartProcess("p", ByModel); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Errorf("starting a running process = %v", err)
	}
	if err := r.StartProcess("nobody", ByModel); !errors.Is(err, ErrUnknownProcess) {
		t.Errorf("starting an unknown process = %v", err)
	}
}

// A stop records who stopped the process, which the start rule reads.
func TestStopRecordsWhoStopped(t *testing.T) {
	r, store := controlSetup(t, memory.Process{ID: "p", SessionID: "p"}, "")
	if err := r.StopProcess("p", ByModel); err != nil {
		t.Fatal(err)
	}
	if p := processRow(t, store, "p"); p.State != memory.ProcessStopped || p.StoppedBy != ByModel {
		t.Errorf("after a model's stop: state %q, stopped_by %q", p.State, p.StoppedBy)
	}
	if err := r.StopProcess("p", ByModel); err == nil {
		t.Error("stopping a stopped process was accepted")
	}
}

// A send reaches a live process waiting for work, labelled with its sender;
// one that cannot take it now is refused so the model can try again.
func TestSendProcess(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{ID: "agent", SessionID: "agent-session"})
	if err := r.SendProcess("agent", "hello", "conv-1"); err == nil || !strings.Contains(err.Error(), "not running yet") {
		t.Errorf("send before the start = %v", err)
	}
	r.tickLive(context.Background())
	eventually(t, "the process waiting in next()", func() bool {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		lp := r.lives["agent"]
		if lp == nil {
			return false
		}
		lp.mu.Lock()
		defer lp.mu.Unlock()
		return lp.waiting
	})
	if err := r.SendProcess("agent", "check the notes", "conv-1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the turn", func() bool { return len(sessions.seen()) == 1 })
	if got := sessions.seen()[0]; got != "agent-session|condition|[From conversation conv-1: check the notes]" {
		t.Errorf("turn = %q", got)
	}

	if err := store.ProcessUpsertDefinition(memory.Process{ID: "slice", Tool: "x", IntervalSecs: 60}); err != nil {
		t.Fatal(err)
	}
	if err := r.SendProcess("slice", "x", "conv-1"); err == nil || !strings.Contains(err.Error(), "slice process") {
		t.Errorf("send to a slice process = %v", err)
	}
}

// The roster reports each process's budget against its limits.
func TestProcessStatusReportsTheBudget(t *testing.T) {
	r, store := controlSetup(t, memory.Process{ID: "p", SessionID: "p", BudgetTurns: 5}, "")
	if err := store.ProcessUsageAdd("p", 1200); err != nil {
		t.Fatal(err)
	}
	st, ok, err := r.ProcessOf("p", 10)
	if err != nil || !ok {
		t.Fatalf("ProcessOf: ok=%v err=%v", ok, err)
	}
	if st.Budget.Turns != 1 || st.Budget.TurnsPerDay != 5 || st.Budget.Tokens != 1200 || st.Budget.ResetsAt == "" {
		t.Errorf("budget = %+v", st.Budget)
	}
	if st.Mode != memory.ProcessLive || st.Session != "p" || st.Attached {
		t.Errorf("status = %+v", st)
	}
}
