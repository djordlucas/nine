package runtime

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// fakeSessions records the turns live processes ask for and answers each with
// "reply:<text>".
type fakeSessions struct {
	mu    sync.Mutex
	turns []string // "<session>|<trigger>|<text>"
	fail  error
	// tokens is what each turn reports spending.
	tokens int
	// allows records each turn's tool restriction, nil for none.
	allows [][]string
	// depths records each turn's lineage depth.
	depths []int
}

func (f *fakeSessions) ProcessTurn(_ context.Context, id string, _ RoleParams, text string, opt TurnOptions) (string, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, id+"|"+opt.Trigger+"|"+text)
	f.allows = append(f.allows, opt.Allow)
	f.depths = append(f.depths, opt.Depth)
	if f.fail != nil {
		return "", f.tokens, f.fail
	}
	return "reply:" + text, f.tokens, nil
}

func (f *fakeSessions) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.turns...)
}

const relayManifest = `
name = "relay"
kind = "js"
entrypoint = "./relay.js"
description = "A live process: turn every trigger into a model turn and report the reply."
`

// relay asks the model about each trigger and reports the reply.
const relaySource = `
import { next, turn, report } from "nine:process";
export default () => {
  for (;;) {
    const t = next();
    report(turn(t.kind === "clock" ? "tick" : t.text));
  }
};`

func liveSetup(t *testing.T, row memory.Process) (*StandingRunner, *memory.Store, *fakeSessions) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := standingHost(t, "relay", relayManifest, relaySource, nil)
	r := NewStandingRunner(store, host, 1, 2)
	sessions := &fakeSessions{}
	r.SetSessions(sessions)
	t.Cleanup(r.StopLive)

	row.Tool, row.Mode, row.Owner = "relay", memory.ProcessLive, true
	if err := store.ProcessUpsertDefinition(row); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessSetState(row.ID, memory.ProcessRunning); err != nil {
		t.Fatal(err)
	}
	return r, store, sessions
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func feedText(t *testing.T, store *memory.Store) string {
	t.Helper()
	all, err := store.UserNotificationList(false)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, n := range all {
		b.WriteString(n.Message + "\n")
	}
	return b.String()
}

// tickUntil runs runner passes until ok holds, as RunStandingTools would.
func tickUntil(t *testing.T, r *StandingRunner, what string, ok func() bool) {
	t.Helper()
	eventually(t, what, func() bool {
		r.tickLive(context.Background())
		return ok()
	})
}

// A clock tick reaches the program through next(), one cadence after the start
// — as a routine first woke one idle interval after its session started; its
// turn runs in the process's session labelled "idle", as the routine turn it
// replaces was; and its report, with no live receiver, reaches the human feed.
func TestLiveProcessTurnsAClockTickIntoASessionTurn(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{
		ID: "digest", SessionID: "digest-session", IntervalSecs: 1, ReportTo: "nobody",
	})
	r.tickLive(context.Background())
	time.Sleep(300 * time.Millisecond)
	r.tickLive(context.Background())
	if n := len(sessions.seen()); n != 0 {
		t.Fatalf("turns before the first cadence elapsed = %d, want 0", n)
	}

	tickUntil(t, r, "the tick's turn", func() bool { return len(sessions.seen()) >= 1 })
	if got := sessions.seen()[0]; got != "digest-session|idle|tick" {
		t.Errorf("turn = %q, want the session, the idle label and the program's text", got)
	}
	eventually(t, "the report on the human feed", func() bool {
		return strings.Contains(feedText(t, store), "reply:tick")
	})
}

// A report piped to a session a live process owns is a message trigger for
// that process, taken only while it waits in next() — and its turn is labelled
// "condition", as a condition trigger's woken turn is.
func TestPipeDeliversToTheLiveProcessOwningTheSession(t *testing.T) {
	r, _, sessions := liveSetup(t, memory.Process{ID: "agent", SessionID: "agent-session"})
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

	handled, delivered := r.Deliver("agent-session", "found: a stray key", "when:agent", true, 1)
	if !handled || !delivered {
		t.Fatalf("Deliver = handled %v, delivered %v; want both", handled, delivered)
	}
	eventually(t, "the woken turn", func() bool { return len(sessions.seen()) == 1 })
	if got := sessions.seen()[0]; got != "agent-session|condition|found: a stray key" {
		t.Errorf("turn = %q", got)
	}

	if handled, _ := r.Deliver("someone-else", "x", "y", true, 1); handled {
		t.Error("a session no live process owns was handled")
	}
}

// Stopping a process in the store stops its running instance on the next pass.
func TestStoppedLiveProcessIsStopped(t *testing.T) {
	r, store, _ := liveSetup(t, memory.Process{ID: "agent", SessionID: "agent-session"})
	r.tickLive(context.Background())
	eventually(t, "the process running", func() bool {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		return r.lives["agent"] != nil
	})

	if _, err := store.ProcessSetState("agent", memory.ProcessStopped); err != nil {
		t.Fatal(err)
	}
	r.tickLive(context.Background())
	eventually(t, "the process stopping", func() bool {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		return r.lives["agent"] == nil
	})
}

// A process whose turn fails ends with an error, is recorded as failing with
// a backoff, and is restarted once the backoff has passed.
func TestFailedLiveProcessRestartsAfterBackoff(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{
		ID: "digest", SessionID: "digest-session", IntervalSecs: 1, ReportTo: "nobody",
	})
	sessions.fail = errors.New("provider down")
	tickUntil(t, r, "the failure recorded", func() bool {
		p, _, _ := store.ProcessGet("digest")
		return p.Failures == 1
	})

	sessions.mu.Lock()
	sessions.fail = nil
	sessions.mu.Unlock()
	if err := store.ProcessSetNextAt("digest", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	tickUntil(t, r, "the restarted process's turn", func() bool {
		for _, s := range sessions.seen() {
			if s == "digest-session|idle|tick" && len(sessions.seen()) > 1 {
				return true
			}
		}
		return false
	})
}

// Goal binding: a process whose goal is paused is stopped by its goal, and runs
// again when the goal is reactivated.
func TestGoalBindingStopsAndRestartsTheProcess(t *testing.T) {
	r, store, _ := liveSetup(t, memory.Process{ID: "goal:g1", SessionID: "g1", GoalID: "g1", IntervalSecs: 3600})
	if err := store.GoalCreate("g1", "keep notes tidy", "", "operator"); err != nil {
		t.Fatal(err)
	}
	running := func() bool {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		return r.lives["goal:g1"] != nil
	}
	tickUntil(t, r, "the process running", running)

	if err := store.GoalUpdateStatus("g1", "paused"); err != nil {
		t.Fatal(err)
	}
	tickUntil(t, r, "the process stopped", func() bool { return !running() })
	p, _, _ := store.ProcessGet("goal:g1")
	if p.State != memory.ProcessStopped || p.StoppedBy != "goal" {
		t.Errorf("after pausing the goal: state %q, stopped by %q; want stopped by the goal", p.State, p.StoppedBy)
	}

	if err := store.GoalUpdateStatus("g1", "active"); err != nil {
		t.Fatal(err)
	}
	tickUntil(t, r, "the process running again", running)
}

// ReconcileProcesses maps blocks to processes: a live tool makes a live process
// owning its own session; an attached one runs in its owner's session, bound to
// the owner's goal; a pipe names the receiving session; any other tool makes a
// slice process. Goal blocks are left to the goal reconciliation.
func TestReconcileProcessesMapsBlocks(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := standingHost(t, "relay", relayManifest+"live = true\n", relaySource, nil)
	ReconcileProcesses(store, host, []config.ProcessConfig{
		{Name: "sec-watch", Tool: "pursue", Goal: "watch the repo"},
		{Name: "digest", Tool: "relay", Every: "1h", Role: "writer"},
		{Name: "sec-watch-relay", Tool: "relay", Every: "30m", Session: "sec-watch"},
		{Name: "scan", Tool: "scanner", Every: "10s", ReportTo: "sec-watch"},
	})

	if _, found, _ := store.ProcessGet("sec-watch"); found {
		t.Error("the goal block was reconciled here; the goal reconciliation owns it")
	}
	live, _, _ := store.ProcessGet("digest")
	if live.Mode != memory.ProcessLive || live.SessionID != "digest" || !live.Owner || live.Role != "writer" || live.IntervalSecs != 3600 {
		t.Errorf("live process = %+v", live)
	}
	attached, _, _ := store.ProcessGet("sec-watch-relay")
	if attached.Mode != memory.ProcessLive || attached.SessionID != "sec-watch" || attached.Owner || attached.GoalID != "sec-watch" {
		t.Errorf("attached process = %+v", attached)
	}
	slice, _, _ := store.ProcessGet("scan")
	if slice.Mode != memory.ProcessSlice || slice.ReportTo != "sec-watch" || slice.State != memory.ProcessRunning {
		t.Errorf("slice process = %+v", slice)
	}
}

// A live process that fails on every trigger reaches failing, and the feed is
// told once; when its turns work again it recovers, and the feed is told that
// too. A restart alone is not a recovery: were it one, the failures would
// clear at each restart and the process would never reach failing.
func TestLiveProcessFailingAndRecovery(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{
		ID: "digest", SessionID: "digest-session", IntervalSecs: 1, ReportTo: "nobody",
	})
	sessions.mu.Lock()
	sessions.fail = errors.New("model unreachable")
	sessions.mu.Unlock()

	tickUntil(t, r, "the process failing", func() bool {
		return processRow(t, store, "digest").State == memory.ProcessFailing
	})
	if p := processRow(t, store, "digest"); p.Failures < StandingFailureThreshold {
		t.Errorf("failures = %d, want at least %d", p.Failures, StandingFailureThreshold)
	}
	if feed := feedText(t, store); strings.Count(feed, "has failed") != 1 {
		t.Errorf("feed should report failing once:\n%s", feed)
	}

	sessions.mu.Lock()
	sessions.fail = nil
	sessions.mu.Unlock()
	tickUntil(t, r, "the recovery", func() bool {
		return processRow(t, store, "digest").State == memory.ProcessRunning
	})
	if p := processRow(t, store, "digest"); p.Failures != 0 {
		t.Errorf("failures after recovery = %d, want 0", p.Failures)
	}
	if feed := feedText(t, store); !strings.Contains(feed, "Process digest recovered") {
		t.Errorf("feed lacks the recovery:\n%s", feed)
	}
}

// A pipe's report runs its turn restricted to PipedTurnTools; a message a
// person sent with process_send does not, since it carries their request.
func TestPipedTurnIsRestrictedAndASendIsNot(t *testing.T) {
	r, _, sessions := liveSetup(t, memory.Process{ID: "agent", SessionID: "agent-session", ReportTo: "nobody"})
	r.tickLive(context.Background())
	waiting := func() bool {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		lp := r.lives["agent"]
		if lp == nil {
			return false
		}
		lp.mu.Lock()
		defer lp.mu.Unlock()
		return lp.waiting
	}
	eventually(t, "the process waiting in next()", waiting)
	if _, ok := r.Deliver("agent-session", "a finding", "watcher", true, 1); !ok {
		t.Fatal("the piped report was not taken")
	}
	eventually(t, "the piped turn", func() bool { return len(sessions.seen()) == 1 })

	eventually(t, "the process waiting again", waiting)
	if err := r.SendProcess("agent", "do this", "conversation c1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the sent turn", func() bool { return len(sessions.seen()) == 2 })

	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if got := sessions.allows[0]; len(got) == 0 || !slices.Equal(got, PipedTurnTools) {
		t.Errorf("piped turn allow = %v, want PipedTurnTools", got)
	}
	if got := sessions.allows[1]; got != nil {
		t.Errorf("sent turn allow = %v, want no restriction", got)
	}
}

// The allowlist keeps out what an injected instruction could do harm with.
func TestPipedTurnToolsLeaveOutHarm(t *testing.T) {
	for _, name := range []string{"delete_file", "move_file", "shell", "http_get", "http_post",
		"web_page_read", "run_agent", "run_agents", "workflow_create", "tool_write", "tool_delete",
		"js_eval", "skill_write", "skill_modify", "goal_create", "memory_delete", "process_start",
		"process_send", "capability_request"} {
		if slices.Contains(PipedTurnTools, name) {
			t.Errorf("PipedTurnTools holds %s", name)
		}
	}
}

// A live process with no report_to reports to the human feed, as a slice
// process's result does; it does not fail.
func TestReportWithoutAPipeGoesToTheFeed(t *testing.T) {
	r, store, _ := liveSetup(t, memory.Process{ID: "digest", SessionID: "digest-session", IntervalSecs: 1})
	tickUntil(t, r, "the report on the feed", func() bool {
		return strings.Contains(feedText(t, store), "[digest] reply:tick")
	})
	if p := processRow(t, store, "digest"); p.Failures != 0 {
		t.Errorf("reporting with no pipe failed the process: %d failures, %q", p.Failures, p.LastError)
	}
}
