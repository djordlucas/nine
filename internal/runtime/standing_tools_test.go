package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// fakeCaller stands in for the wired ProcessToolCaller (core dispatcher for Go
// builtins, plugin manager for MCP plugins): one synchronous call per tick,
// behavior injected per test.
type fakeCaller struct {
	calls int
	fn    func(args string) (string, error)
}

func (f *fakeCaller) CallProcessTool(_ context.Context, _ string, args json.RawMessage) (string, error) {
	f.calls++
	return f.fn(string(args))
}

// watcherFn is the standing-tool shape: speaks only when it has something to
// say, so a quiet run posts nothing to the human feed.
func watcherFn(args string) (string, error) {
	var a struct {
		Quiet bool `json:"quiet"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	if a.Quiet {
		return "", nil
	}
	return "found something", nil
}

func brokenFn(string) (string, error) { return "", errors.New("upstream is down") }

// predicateFn is the condition-trigger shape: silent unless it finds something.
func predicateFn(args string) (string, error) {
	var a struct {
		Found string `json:"found"`
	}
	_ = json.Unmarshal([]byte(args), &a)
	if a.Found == "" {
		return "", nil
	}
	return "found: " + a.Found, nil
}

func newStandingRunner(t *testing.T, store *memory.Store, fn func(args string) (string, error)) (*StandingRunner, *fakeCaller) {
	t.Helper()
	c := &fakeCaller{fn: fn}
	r := NewStandingRunner(store, 1, 2)
	r.SetToolCaller(c)
	return r, c
}

func declare(t *testing.T, store *memory.Store, id, tool string, interval int) {
	t.Helper()
	if err := store.ProcessUpsertDefinition(memory.Process{
		ID: id, Tool: tool, Args: `{}`, IntervalSecs: interval,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessSetState(id, memory.ProcessRunning); err != nil {
		t.Fatal(err)
	}
}

// One call completes a cycle: the cycle count moves, the cursor resets, and
// the next cycle waits for the trigger rather than firing at once.
func TestStandingToolCompletesACycleAndReschedules(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r, c := newStandingRunner(t, store, watcherFn)
	declare(t, store, "w1", "watcher", 3600)

	r.runDue(context.Background())
	if c.calls != 1 {
		t.Fatalf("calls = %d, want 1", c.calls)
	}
	got, _, _ := store.ProcessGet("w1")
	if got.Cycles != 1 {
		t.Fatalf("cycles = %d, want 1", got.Cycles)
	}
	if got.Cursor != "" {
		t.Fatalf("cursor = %q after a completed cycle, want it reset", got.Cursor)
	}
	// The next cycle waits for the trigger, not for the tool.
	next := time.Until(mustTime(t, got.NextAt))
	if next < 30*time.Minute {
		t.Fatalf("next cycle in %v, want the hour-long interval", next)
	}
}

// A cycle that produces output posts to the human feed; one that returns nothing
// stays silent. That convention is what stops a ten-second watcher becoming a
// notification storm.
func TestStandingToolReportsOnlyWhenItHasSomethingToSay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      string
		wantNotes int
	}{
		{"speaks when it finds something", `{}`, 1},
		{"silent when it does not", `{"quiet":true}`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := memtest.Open(t)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := newStandingRunner(t, store, watcherFn)
			if err := store.ProcessUpsertDefinition(memory.Process{
				ID: "w1", Tool: "watcher", Args: tc.args, IntervalSecs: 3600,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProcessSetState("w1", memory.ProcessRunning); err != nil {
				t.Fatal(err)
			}

			r.runDue(context.Background())

			notes, err := store.UserNotificationList(false)
			if err != nil {
				t.Fatal(err)
			}
			if len(notes) != tc.wantNotes {
				t.Fatalf("posted %d notifications, want %d: %+v", len(notes), tc.wantNotes, notes)
			}
		})
	}
}

// The failure that actually happens: a tool throwing on every call while nobody
// notices. Failures back off, the breaker makes it visible, and only the
// transition notifies — a flapping tool must not produce a storm.
func TestStandingToolBreakerTripsOnceAndBacksOff(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newStandingRunner(t, store, brokenFn)
	declare(t, store, "b1", "broken", 1)

	for range StandingFailureThreshold + 3 {
		// Force it due, so the backoff does not stall the test.
		if _, err := store.ProcessFail("b1", "", time.Now(), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ProcessSetState("b1", memory.ProcessRunning); err != nil {
			t.Fatal(err)
		}
		if err := store.ProcessUpsertDefinition(memory.Process{
			ID: "b1", Tool: "broken", Args: `{}`, IntervalSecs: 1,
		}); err != nil {
			t.Fatal(err)
		}
		r.runDue(context.Background())
	}

	got, _, _ := store.ProcessGet("b1")
	if !strings.Contains(got.LastError, "upstream is down") {
		t.Fatalf("last_error = %q, want the tool's own message", got.LastError)
	}

	notes, err := store.UserNotificationList(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) > 1 {
		t.Fatalf("a repeatedly-failing tool posted %d notifications; only the transition should", len(notes))
	}
}

// A stopped standing tool is not called, and reconciling config does not restart
// it — config owns the definition, the runtime owns the run state.
func TestStoppedStandingToolStaysStoppedAcrossReconcile(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r, c := newStandingRunner(t, store, watcherFn)
	declare(t, store, "w1", "watcher", 1)

	if _, err := store.ProcessSetState("w1", memory.ProcessStopped); err != nil {
		t.Fatal(err)
	}
	r.runDue(context.Background())
	if got, _, _ := store.ProcessGet("w1"); got.Calls != 0 {
		t.Fatalf("a stopped standing tool was called %d times", got.Calls)
	}
	if c.calls != 0 {
		t.Fatalf("the caller was invoked %d times for a stopped tool", c.calls)
	}

	// The operator's file still declares it, as it did before they stopped it.
	ReconcileProcesses(store, []config.ProcessConfig{
		{Name: "w1", Tool: "watcher", Every: "1s"},
	})
	if got, _, _ := store.ProcessGet("w1"); got.State != memory.ProcessStopped {
		t.Fatalf("state = %q after reconcile, want it left stopped", got.State)
	}
}

// Changing args restarts the cycle: the cursor was produced under the old
// arguments and resuming with it would be incoherent.
func TestArgsChangeRestartsTheCycle(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	ReconcileProcesses(store, []config.ProcessConfig{
		{Name: "w1", Tool: "watcher", Every: "1s", Args: map[string]any{"path": "/a"}},
	})
	if _, err := store.ProcessAdvance("w1", "mid-cycle", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.ProcessGet("w1"); got.Cursor != "mid-cycle" {
		t.Fatal("precondition: the run should hold a cursor")
	}

	ReconcileProcesses(store, []config.ProcessConfig{
		{Name: "w1", Tool: "watcher", Every: "1s", Args: map[string]any{"path": "/b"}},
	})
	got, _, _ := store.ProcessGet("w1")
	if got.Cursor != "" {
		t.Fatalf("cursor = %q after an args change, want the cycle restarted", got.Cursor)
	}
}

// A block declared with enabled = false is staged, not started.
func TestDisabledBlockIsDeclaredButNotStarted(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	no := false
	ReconcileProcesses(store, []config.ProcessConfig{
		{Name: "w1", Tool: "watcher", Every: "1s", Enabled: &no},
	})
	got, found, err := store.ProcessGet("w1")
	if err != nil || !found {
		t.Fatalf("the block was not declared: found=%v err=%v", found, err)
	}
	if got.State != memory.ProcessStopped {
		t.Fatalf("state = %q, want stopped", got.State)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02T15:04:05.000000Z", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}

// The journal rule: a standing run's ordinary calls are NOT events. Only its
// transitions and its output are, or a ten-second watcher would write 8,640 rows
// a day and bury what `nine trace` exists to show.
func TestStandingHeartbeatIsNotJournalled(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newStandingRunner(t, store, watcherFn)
	// quiet: the cycle completes with no output, so nothing is worth reporting.
	if err := store.ProcessUpsertDefinition(memory.Process{
		ID: "w1", Tool: "watcher", Args: `{"quiet":true}`, IntervalSecs: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessSetState("w1", memory.ProcessRunning); err != nil {
		t.Fatal(err)
	}

	r.runDue(context.Background())
	if got, _, _ := store.ProcessGet("w1"); got.Cycles != 1 {
		t.Fatalf("precondition: expected one completed cycle, got %d", got.Cycles)
	}

	evs, err := store.SessionEventsByAgent("w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Fatalf("a silent cycle wrote %d journal events; heartbeat calls must not be journalled: %+v",
			len(evs), evs)
	}

	// Recent activity still exists — it just lives in memory, not the journal.
	if got := r.log.recent("w1", 10); len(got) != 1 {
		t.Fatalf("ring buffer holds %d entries, want the 1 call", len(got))
	}
}

// …but a cycle that produced output IS an event, and so are the state changes.
func TestStandingOutputAndTransitionsAreJournalled(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newStandingRunner(t, store, watcherFn)
	declare(t, store, "w1", "watcher", 3600)

	r.runDue(context.Background())
	if err := r.StopProcess("w1", ByOperator); err != nil {
		t.Fatal(err)
	}

	evs, err := store.SessionEventsByAgent("w1")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.Type] = true
	}
	if !seen[evStandingReported] {
		t.Errorf("a cycle with output was not journalled: %+v", evs)
	}
	if !seen[evStandingStopped] {
		t.Errorf("stopping was not journalled: %+v", evs)
	}
}

// A generated standing tool that keeps failing is switched off; an operator's
// own declaration keeps retrying. The asymmetry is the point — one has an author
// to answer to, the other does not.
func TestGeneratedStandingToolAutoDisables(t *testing.T) {
	for _, tc := range []struct {
		name      string
		generated bool
		wantState string
	}{
		{"generated is disabled", true, memory.ProcessStopped},
		{"config-declared keeps retrying", false, memory.ProcessFailing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := memtest.Open(t)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := newStandingRunner(t, store, brokenFn)
			if err := store.ProcessUpsertDefinition(memory.Process{
				ID: "b1", Tool: "broken", Args: `{}`, IntervalSecs: 1, Generated: tc.generated,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ProcessSetState("b1", memory.ProcessRunning); err != nil {
				t.Fatal(err)
			}

			for range StandingGeneratedDisableAfter + 2 {
				got, _, _ := store.ProcessGet("b1")
				if got.State == memory.ProcessStopped {
					break
				}
				// Force it due so the backoff does not stall the test.
				if _, err := store.ProcessFail("b1", got.LastError, time.Now(), got.State); err != nil {
					t.Fatal(err)
				}
				r.runDue(context.Background())
			}

			got, _, _ := store.ProcessGet("b1")
			if got.State != tc.wantState {
				t.Fatalf("state = %q, want %q", got.State, tc.wantState)
			}
		})
	}
}

// SetState("start") clears the failure history and schedules an immediate call,
// so `start` after a fix means "try again now" rather than "wait out the
// backoff you accrued while broken".
func TestStartClearsFailureHistory(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := newStandingRunner(t, store, watcherFn)
	declare(t, store, "w1", "watcher", 3600)
	if _, err := store.ProcessFail("w1", "boom", time.Now().Add(time.Hour), memory.ProcessFailing); err != nil {
		t.Fatal(err)
	}

	if err := r.StartProcess("w1", ByOperator); err != nil {
		t.Fatal(err)
	}
	got, _, _ := store.ProcessGet("w1")
	if got.Failures != 0 || got.LastError != "" {
		t.Fatalf("failures=%d lastErr=%q, want both cleared", got.Failures, got.LastError)
	}
	if got.NextAt != "" {
		t.Fatalf("next_at = %q, want it cleared so the first call happens now", got.NextAt)
	}
}

// Removing a [[process]] block deletes the process it declared at the next
// reconcile, and the human feed says so; a block still in the file, and a
// process no block declared — a goal session, reflection, one Nine wrote — are
// untouched.
func TestRemovingABlockDeletesItsProcess(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	ReconcileProcesses(store, []config.ProcessConfig{
		{Name: "w1", Tool: "watcher", Every: "1s"},
		{Name: "w2", Tool: "watcher", Every: "1s"},
	})
	if err := store.ProcessUpsertDefinition(memory.Process{ID: "goal:g1", Tool: "pursue", Mode: memory.ProcessLive}); err != nil {
		t.Fatal(err)
	}

	ReconcileProcesses(store, []config.ProcessConfig{{Name: "w2", Tool: "watcher", Every: "1s"}})

	if _, found, _ := store.ProcessGet("w1"); found {
		t.Error("the process whose block was removed still exists")
	}
	for _, id := range []string{"w2", "goal:g1"} {
		if _, found, _ := store.ProcessGet(id); !found {
			t.Errorf("%s was deleted", id)
		}
	}
	all, err := store.UserNotificationList(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || !strings.Contains(all[0].Message, "Process w1 was removed from nine.toml") {
		t.Errorf("feed = %+v, want one notice of w1's deletion", all)
	}
}

// A process attached to a deleted owner's session is stopped, since the
// session it ran in has no owner left to give it a role.
func TestRemovingAnOwnerStopsWhatIsAttachedToIt(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	ReconcileProcesses(store, []config.ProcessConfig{{Name: "agent", Tool: "watcher", Every: "1s"}})
	// Live processes share sessions; mark these as such directly, as the
	// reconcile of a live tool would.
	for _, p := range []memory.Process{
		{ID: "agent", Tool: "watcher", Mode: memory.ProcessLive, SessionID: "agent", Owner: true, Declared: true},
		{ID: "beside", Tool: "watcher", Mode: memory.ProcessLive, SessionID: "agent", Owner: false},
	} {
		if err := store.ProcessUpsertDefinition(p); err != nil {
			t.Fatal(err)
		}
	}

	ReconcileProcesses(store, nil)

	got, found, _ := store.ProcessGet("beside")
	if !found || got.State != memory.ProcessStopped {
		t.Errorf("attached process: found %v, state %q; want it kept and stopped", found, got.State)
	}
	all, _ := store.UserNotificationList(false)
	if len(all) != 1 || !strings.Contains(all[0].Message, "session agent is kept") || !strings.Contains(all[0].Message, "beside, attached") {
		t.Errorf("feed = %+v", all)
	}
}
