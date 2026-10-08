package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/toolvm"
)

func processRow(t *testing.T, store *memory.Store, id string) memory.Process {
	t.Helper()
	p, ok, err := store.ProcessGet(id)
	if err != nil || !ok {
		t.Fatalf("ProcessGet(%s): ok=%v err=%v", id, ok, err)
	}
	return p
}

// A process whose turns reach its budget is paused before the next one: the
// turn is refused, the process is stopped by its budget, its instance closes,
// and the pause reaches the human feed with when it runs again.
func TestBudgetPausesAProcessAndTellsTheFeed(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{
		ID: "digest", SessionID: "digest-session", IntervalSecs: 1, BudgetTurns: 2,
	})
	tickUntil(t, r, "the budget's pause", func() bool {
		return processRow(t, store, "digest").StoppedBy == "budget"
	})

	if n := len(sessions.seen()); n != 2 {
		t.Errorf("turns run = %d, want the 2 the budget covers", n)
	}
	p := processRow(t, store, "digest")
	if p.State != memory.ProcessStopped || p.UsageTurns != 2 {
		t.Errorf("state %q, usage %d turns; want stopped after 2", p.State, p.UsageTurns)
	}
	if feed := feedText(t, store); !strings.Contains(feed, "spent its budget") || !strings.Contains(feed, "2 of 2 turns") {
		t.Errorf("feed lacks the pause:\n%s", feed)
	}
	tickUntil(t, r, "the instance closed", func() bool {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		return r.lives["digest"] == nil
	})
	// relay does not catch E_BUDGET: its throw ends the instance with the
	// pause, not as a failure.
	if p := processRow(t, store, "digest"); p.Failures != 0 || p.LastError != "" {
		t.Errorf("the pause was recorded as a failure: %d failures, %q", p.Failures, p.LastError)
	}
}

// Tokens bound a process as turns do: the turn that takes usage past the
// budget is counted, and the next is refused.
func TestBudgetCountsTokens(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{
		ID: "digest", SessionID: "digest-session", IntervalSecs: 1, BudgetTokens: 100,
	})
	sessions.tokens = 60
	tickUntil(t, r, "the budget's pause", func() bool {
		return processRow(t, store, "digest").StoppedBy == "budget"
	})
	if p := processRow(t, store, "digest"); p.UsageTurns != 2 || p.UsageTokens != 120 {
		t.Errorf("usage = %d turns, %d tokens; want 2 and 120", p.UsageTurns, p.UsageTokens)
	}
}

// A paused process runs again once its day is over — not before — with its
// usage back at zero, and the feed says so.
func TestBudgetResumesWhenItsDayIsOver(t *testing.T) {
	r, store, _ := liveSetup(t, memory.Process{ID: "digest", SessionID: "digest-session", BudgetTurns: 1})
	if err := store.ProcessUsageAdd("digest", 10); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := r.spendTurn("digest", now); !errors.Is(err, toolvm.ErrBudget) {
		t.Fatalf("spendTurn at the limit = %v, want ErrBudget", err)
	}

	r.resumeBudgets(now.Add(time.Hour))
	if p := processRow(t, store, "digest"); p.State != memory.ProcessStopped {
		t.Fatalf("resumed after an hour: state %q", p.State)
	}

	r.resumeBudgets(now.Add(budgetDay + time.Minute))
	p := processRow(t, store, "digest")
	if p.State != memory.ProcessRunning || p.StoppedBy != "" || p.UsageTurns != 0 || p.UsageSince != "" {
		t.Errorf("after the day: %+v; want running, unstopped, usage reset", p)
	}
	if feed := feedText(t, store); !strings.Contains(feed, "running again") {
		t.Errorf("feed lacks the resume:\n%s", feed)
	}
}

// A day that is over starts again at the next turn, so a process that was
// never paused does not carry yesterday's usage.
func TestBudgetDayStartsAgainAtTheNextTurn(t *testing.T) {
	r, store, _ := liveSetup(t, memory.Process{ID: "digest", SessionID: "digest-session", BudgetTurns: 1})
	if err := store.ProcessUsageAdd("digest", 10); err != nil {
		t.Fatal(err)
	}
	if err := r.spendTurn("digest", time.Now().Add(budgetDay+time.Minute)); err != nil {
		t.Fatalf("spendTurn on a new day = %v", err)
	}
	if p := processRow(t, store, "digest"); p.UsageTurns != 0 || p.State != memory.ProcessRunning {
		t.Errorf("usage %d turns, state %q; want a new day, running", p.UsageTurns, p.State)
	}
}

// A process's own budget lowers [processes] budget and never raises it.
func TestBudgetOfAProcessSitsUnderTheCeiling(t *testing.T) {
	r := &StandingRunner{}
	r.SetBudget(config.BudgetConfig{TurnsPerDay: 10})
	for _, tc := range []struct {
		row  memory.Process
		want config.BudgetConfig
	}{
		{memory.Process{}, config.BudgetConfig{TurnsPerDay: 10, TokensPerDay: config.DefaultBudgetTokensPerDay}},
		{memory.Process{BudgetTurns: 3, BudgetTokens: 500}, config.BudgetConfig{TurnsPerDay: 3, TokensPerDay: 500}},
		{memory.Process{BudgetTurns: 50}, config.BudgetConfig{TurnsPerDay: 10, TokensPerDay: config.DefaultBudgetTokensPerDay}},
	} {
		if got := r.budgetOf(tc.row); got != tc.want {
			t.Errorf("budgetOf(%d turns, %d tokens) = %+v, want %+v",
				tc.row.BudgetTurns, tc.row.BudgetTokens, got, tc.want)
		}
	}
}

// What the program sees when its budget refuses a turn is E_BUDGET, so a
// program can tell it from a stop.
func TestBudgetRefusalReachesTheProgramAsEBudget(t *testing.T) {
	r, store, _ := liveSetup(t, memory.Process{ID: "digest", SessionID: "digest-session", BudgetTurns: 1})
	if err := store.ProcessUsageAdd("digest", 0); err != nil {
		t.Fatal(err)
	}
	h := &liveHandler{r: r, lp: &liveProc{row: processRow(t, store, "digest")}}
	if _, err := h.Turn(context.Background(), "tick"); !errors.Is(err, toolvm.ErrBudget) {
		t.Errorf("Turn over budget = %v, want ErrBudget", err)
	}
}
