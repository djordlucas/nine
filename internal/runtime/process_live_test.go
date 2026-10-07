package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// fakeSessions records the turns live processes ask for and answers each with
// "reply:<text>".
type fakeSessions struct {
	mu    sync.Mutex
	turns []string // "<session>|<trigger>|<text>"
	fail  error
}

func (f *fakeSessions) ProcessTurn(_ context.Context, id string, _ RoleParams, text, trigger string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.turns = append(f.turns, id+"|"+trigger+"|"+text)
	if f.fail != nil {
		return "", f.fail
	}
	return "reply:" + text, nil
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

// A clock tick reaches the program through next(); its turn runs in the
// process's session labelled "idle", as the routine turn it replaces was; and
// its report, with no live receiver, reaches the human feed.
func TestLiveProcessTurnsAClockTickIntoASessionTurn(t *testing.T) {
	r, store, sessions := liveSetup(t, memory.Process{
		ID: "digest", SessionID: "digest-session", IntervalSecs: 3600, ReportTo: "nobody",
	})
	r.tickLive(context.Background())

	eventually(t, "the tick's turn", func() bool { return len(sessions.seen()) == 1 })
	if got := sessions.seen()[0]; got != "digest-session|idle|tick" {
		t.Errorf("turn = %q, want the session, the idle label and the program's text", got)
	}
	eventually(t, "the report on the human feed", func() bool {
		return strings.Contains(feedText(t, store), "reply:tick")
	})

	// The next tick is an hour away: another pass delivers nothing new.
	r.tickLive(context.Background())
	time.Sleep(200 * time.Millisecond)
	if n := len(sessions.seen()); n != 1 {
		t.Errorf("turns after a second pass = %d, want 1", n)
	}
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

	handled, delivered := r.Deliver("agent-session", "found: a stray key", "when:agent")
	if !handled || !delivered {
		t.Fatalf("Deliver = handled %v, delivered %v; want both", handled, delivered)
	}
	eventually(t, "the woken turn", func() bool { return len(sessions.seen()) == 1 })
	if got := sessions.seen()[0]; got != "agent-session|condition|found: a stray key" {
		t.Errorf("turn = %q", got)
	}

	if handled, _ := r.Deliver("someone-else", "x", "y"); handled {
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
	r.tickLive(context.Background())
	eventually(t, "the failure recorded", func() bool {
		p, _, _ := store.ProcessGet("digest")
		return p.Failures == 1
	})

	sessions.mu.Lock()
	sessions.fail = nil
	sessions.mu.Unlock()
	if err := store.ProcessSetNextAt("digest", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	r.tickLive(context.Background())
	eventually(t, "the restarted process's turn", func() bool {
		for _, s := range sessions.seen() {
			if s == "digest-session|idle|tick" && len(sessions.seen()) > 1 {
				return true
			}
		}
		return false
	})
}
