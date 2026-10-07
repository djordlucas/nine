package runtime

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// fakeWaker records what a condition trigger delivered, and can decline the way
// a busy or absent agent does.
type fakeWaker struct {
	mu     sync.Mutex
	woke   []string
	accept bool
}

func (f *fakeWaker) WakeAgent(agentID, text string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.accept {
		return false
	}
	f.woke = append(f.woke, agentID+": "+text)
	return true
}

func (f *fakeWaker) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.woke...)
}

const predicateManifest = `
name = "predicate"
kind = "js"
entrypoint = "./predicate.js"
description = "Report only when there is something."
resumable = true
`

// The predicate shape: silent unless it finds something.
const predicateSrc = `
export default function (args) { return args.found ? "found: " + args.found : ""; }
`

func conditionSetup(t *testing.T, args string) (*StandingRunner, *memory.Store, *fakeWaker) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := standingHost(t, "predicate", predicateManifest, predicateSrc, nil)
	r := NewStandingRunner(store, host, 1, 2)
	w := &fakeWaker{accept: true}
	r.SetWaker(w)

	if err := store.ProcessUpsertDefinition(memory.Process{
		ID: ConditionTriggerID("watcher-agent"), Tool: "predicate", Args: args,
		IntervalSecs: 3600, ReportTo: "watcher-agent",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessSetState(ConditionTriggerID("watcher-agent"), memory.ProcessRunning); err != nil {
		t.Fatal(err)
	}
	return r, store, w
}

// The whole point: the predicate runs with no model in the loop, and the agent's
// turn happens only when it finds something.
func TestConditionTriggerWakesOnlyOnAFinding(t *testing.T) {
	t.Run("silent predicate wakes nothing", func(t *testing.T) {
		r, _, w := conditionSetup(t, `{}`)
		r.runDue(context.Background())
		if got := w.seen(); len(got) != 0 {
			t.Fatalf("a predicate that found nothing woke the agent: %v", got)
		}
	})

	t.Run("a finding wakes the agent with it", func(t *testing.T) {
		r, _, w := conditionSetup(t, `{"found":"a new CVE"}`)
		r.runDue(context.Background())
		got := w.seen()
		if len(got) != 1 {
			t.Fatalf("woke %d times, want 1: %v", len(got), got)
		}
		if !strings.Contains(got[0], "watcher-agent: found: a new CVE") {
			t.Fatalf("delivered %q, want the agent id and the finding", got[0])
		}
	})
}

// A finding that could not be delivered goes to the human feed rather than
// vanishing. A silently-dropped condition is the failure an operator would never
// discover.
func TestUndeliverableFindingFallsBackToTheHumanFeed(t *testing.T) {
	r, store, w := conditionSetup(t, `{"found":"something"}`)
	w.accept = false // the agent is not running, or is mid-turn

	r.runDue(context.Background())

	if got := w.seen(); len(got) != 0 {
		t.Fatalf("the waker reported success it did not have: %v", got)
	}
	notes, err := store.UserNotificationList(false)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 {
		t.Fatalf("posted %d notifications, want the finding to reach the human feed", len(notes))
	}
	if !strings.Contains(notes[0].Message, "not running") {
		t.Fatalf("notification %q does not say why it came here", notes[0].Message)
	}
}

// An ordinary standing tool still reports to the human feed — a condition
// trigger is one with a delivery target, not a change to all of them.
func TestOrdinaryStandingToolStillReportsToHumans(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := standingHost(t, "predicate", predicateManifest, predicateSrc, nil)
	r := NewStandingRunner(store, host, 1, 2)
	w := &fakeWaker{accept: true}
	r.SetWaker(w)

	if err := store.ProcessUpsertDefinition(memory.Process{
		ID: "plain", Tool: "predicate", Args: `{"found":"x"}`, IntervalSecs: 3600,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessSetState("plain", memory.ProcessRunning); err != nil {
		t.Fatal(err)
	}
	r.runDue(context.Background())

	if got := w.seen(); len(got) != 0 {
		t.Fatalf("a standing tool with no wake target reached an agent: %v", got)
	}
	notes, _ := store.UserNotificationList(false)
	if len(notes) != 1 {
		t.Fatalf("posted %d notifications, want 1", len(notes))
	}
}

// Reconciling twice replaces the run rather than accumulating one per boot.
func TestConditionTriggerReconcileIsIdempotent(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	agents := []config.AgentConfig{{
		ID: "sec-watch",
		When: &config.AgentCondition{
			Tool: "predicate", Interval: "10s",
			Args: map[string]any{"path": "/var/log/app.log"},
		},
	}}
	ReconcileConditionTriggers(store, agents)
	ReconcileConditionTriggers(store, agents)

	runs, err := store.ProcessList()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("%d runs after two reconciles, want 1", len(runs))
	}
	got := runs[0]
	if got.ReportTo != "sec-watch" {
		t.Errorf("wake_agent = %q, want the agent it belongs to", got.ReportTo)
	}
	if got.IntervalSecs != 10 {
		t.Errorf("interval = %ds, want 10", got.IntervalSecs)
	}
	if got.State != memory.ProcessRunning {
		t.Errorf("state = %q, want running", got.State)
	}
}

// A condition trigger an operator stopped stays stopped across a reconcile,
// like every other standing run: config owns the definition, the runtime owns
// the run state.
func TestStoppedConditionTriggerStaysStopped(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	agents := []config.AgentConfig{{
		ID:   "sec-watch",
		When: &config.AgentCondition{Tool: "predicate", Interval: "10s"},
	}}
	ReconcileConditionTriggers(store, agents)
	if _, err := store.ProcessSetState(ConditionTriggerID("sec-watch"), memory.ProcessStopped); err != nil {
		t.Fatal(err)
	}

	ReconcileConditionTriggers(store, agents)

	got, _, _ := store.ProcessGet(ConditionTriggerID("sec-watch"))
	if got.State != memory.ProcessStopped {
		t.Fatalf("state = %q after reconcile, want it left stopped", got.State)
	}
}

// Wake is lossy on purpose: a predicate firing again while the agent is still
// reading the first finding wants the agent to look, not two turns.
func TestWakeIsNonBlockingAndCoalesces(t *testing.T) {
	w := &AgentWorker{wake: make(chan string, 1)}
	if !w.Wake("first") {
		t.Fatal("the first wake was dropped")
	}
	if w.Wake("second") {
		t.Fatal("a second wake queued behind the first; it should coalesce")
	}
	select {
	case got := <-w.wake:
		if got != "first" {
			t.Fatalf("delivered %q, want the first finding", got)
		}
	case <-time.After(time.Second):
		t.Fatal("nothing was queued")
	}
}
