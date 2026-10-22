package runtime

import (
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
)

// The wait stays at the interval until the streak reaches IdleBackoffAfter,
// then doubles with each idle turn up to the cap; an interval above the cap is
// kept.
func TestIdleBackoffDoublesUpToTheCap(t *testing.T) {
	five := 5 * time.Minute
	cases := []struct {
		interval time.Duration
		streak   int
		want     time.Duration
	}{
		{five, 0, five},
		{five, 1, five},
		{five, 2, 10 * time.Minute},
		{five, 3, 20 * time.Minute},
		{five, 4, 40 * time.Minute},
		{five, 7, 5*time.Hour + 20*time.Minute},
		{five, 8, IdleBackoffCap},
		{five, 40, IdleBackoffCap},
		{12 * time.Hour, 5, 12 * time.Hour},
	}
	for _, c := range cases {
		if got := idleBackoff(c.interval, c.streak); got != c.want {
			t.Errorf("idleBackoff(%s, %d) = %s, want %s", c.interval, c.streak, got, c.want)
		}
	}
}

// Only a tool outside lookTools counts as acting; an unknown tool acts.
func TestTurnActivityCountsOnlyToolsThatChangeSomething(t *testing.T) {
	var looked turnActivity
	for _, name := range []string{"goal_get", "read_file", "memory_get", "time"} {
		looked.onTool(name)
	}
	if looked.acted {
		t.Error("a turn that only looked counted as acting")
	}
	for _, name := range []string{"write_file", "web_search", "goal_update_status", "a_tool_added_later"} {
		var a turnActivity
		a.onTool(name)
		if !a.acted {
			t.Errorf("%s did not count as acting", name)
		}
	}
}

// A goal session whose clock turns only look waits longer before each next
// tick from the second one on, and says so in its log; a turn that acts brings
// the interval back.
func TestIdleGoalSessionSlowsItsClockAndActingResetsIt(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{ID: "goal:g1", SessionID: "g1", GoalID: "g1", IntervalSecs: 1, ReportTo: "nobody"})
	if err := store.GoalCreate("g1", "keep up with the news", "", "operator"); err != nil {
		t.Fatal(err)
	}
	sessions.mu.Lock()
	sessions.tools = []string{"goal_get"}
	sessions.mu.Unlock()

	tickUntil(t, r, "two idle turns", func() bool { return len(sessions.seen()) >= 2 })
	eventually(t, "the idle note in the log", func() bool {
		for _, e := range r.log.recent("goal:g1", 0) {
			if e.Outcome == "idle" && strings.Contains(e.Detail, "next tick in 2s") {
				return true
			}
		}
		return false
	})
	p, _, _ := store.ProcessGet("goal:g1")
	next, err := time.Parse(time.RFC3339Nano, p.NextAt)
	if err != nil {
		t.Fatalf("next tick %q: %v", p.NextAt, err)
	}
	if wait := time.Until(next); wait < time.Second {
		t.Errorf("next tick in %s after two idle turns, want the doubled interval", wait)
	}

	sessions.mu.Lock()
	sessions.tools = []string{"goal_get", "web_search"}
	sessions.mu.Unlock()
	n := len(sessions.seen())
	tickUntil(t, r, "a turn that acts", func() bool { return len(sessions.seen()) > n })
	eventually(t, "the streak cleared", func() bool {
		r.liveMu.Lock()
		lp := r.lives["goal:g1"]
		r.liveMu.Unlock()
		lp.mu.Lock()
		defer lp.mu.Unlock()
		return lp.idleStreak == 0
	})
}

// A process that is not bound to a goal keeps its cadence however idle it is.
func TestUnboundProcessKeepsItsCadence(t *testing.T) {
	lp := &liveProc{row: memory.Process{ID: "watch", IntervalSecs: 60}}
	if backsOff(lp) {
		t.Error("an unbound process backs off")
	}
	lp.row.GoalID, lp.row.Schedule = "g1", "0 9 * * *"
	if backsOff(lp) {
		t.Error("a goal session on a cron schedule backs off")
	}
}
