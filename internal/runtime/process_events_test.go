package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

const eventRelayManifest = `
name = "eventrelay"
kind = "js"
entrypoint = "./eventrelay.js"
description = "A live process: turn every event into a model turn."
live = true
`

// eventrelay asks the model about each event it is given.
const eventRelaySource = `
import { next, turn } from "nine:process";
export default () => {
  for (;;) {
    const t = next();
    if (t.kind === "event") turn(JSON.stringify(t.event));
  }
};`

// eventSetup is a runner with one running live process triggered by events,
// and an event router over it.
func eventSetup(t *testing.T, row memory.Process) (*StandingRunner, *EventRouter, *memory.Store, *fakeSessions) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := standingHost(t, "eventrelay", eventRelayManifest, eventRelaySource, nil)
	r := NewStandingRunner(store, host, 1, 2)
	sessions := &fakeSessions{}
	r.SetSessions(sessions)
	t.Cleanup(r.StopLive)

	row.Tool, row.Mode, row.Owner = "eventrelay", memory.ProcessLive, true
	if err := store.ProcessUpsertDefinition(row); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessSetState(row.ID, memory.ProcessRunning); err != nil {
		t.Fatal(err)
	}
	r.tickLive(context.Background())
	eventually(t, "the process running", func() bool {
		r.liveMu.Lock()
		defer r.liveMu.Unlock()
		return r.lives[row.ID] != nil
	})
	e := r.EventRouter()
	e.started = time.Now().Add(-time.Minute)
	return r, e, store, sessions
}

func toolEnd(session string, turn int, name, output string) memory.SessionEvent {
	p, _ := json.Marshal(map[string]any{"name": name, "output": output, "duration_ms": 5, "attempts": 1})
	return memory.SessionEvent{AgentID: session, Turn: turn, Type: "tool_end", TS: time.Now(), Payload: p}
}

func turnStart(session string, turn, depth int) memory.SessionEvent {
	p, _ := json.Marshal(map[string]any{"input": "x", "trigger": "event", "depth": depth})
	return memory.SessionEvent{AgentID: session, Turn: turn, Type: "turn_start", TS: time.Now(), Payload: p}
}

func trigger(on []string, f config.EventFilter, content bool) (string, string, bool) {
	return encodeEventTrigger(on, f, content)
}

// An event of a type the process is on reaches it as an event trigger, with
// metadata only: the tool's name and timing, not its output. Its turn runs at
// the event's depth plus one, unrestricted.
func TestEventReachesTheProcessWithMetadata(t *testing.T) {
	on, f, c := trigger([]string{"tool_end"}, config.EventFilter{}, false)
	_, e, _, sessions := eventSetup(t, memory.Process{ID: "watch", SessionID: "watch", OnEvents: on, EventFilter: f, EventContent: c})

	if err := e.Handle(context.Background(), toolEnd("conv-1", 1, "web_page_read", "SECRET PAGE TEXT")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event's turn", func() bool { return len(sessions.seen()) == 1 })
	got := sessions.seen()[0]
	if !strings.Contains(got, `"name":"web_page_read"`) || !strings.Contains(got, `"session":"conv-1"`) {
		t.Errorf("turn = %q, want the tool's name and session", got)
	}
	if strings.Contains(got, "SECRET") {
		t.Errorf("metadata-only delivery carried the output: %q", got)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if sessions.depths[0] != 1 || sessions.allows[0] != nil {
		t.Errorf("depth %d, allow %v; want 1 and unrestricted", sessions.depths[0], sessions.allows[0])
	}
}

// With event_content the output arrives too, and the turn is restricted as a
// piped report's is: outside text is in it.
func TestEventContentIsDeliveredAndRestricted(t *testing.T) {
	on, f, c := trigger([]string{"tool_end"}, config.EventFilter{}, true)
	_, e, _, sessions := eventSetup(t, memory.Process{ID: "watch", SessionID: "watch", OnEvents: on, EventFilter: f, EventContent: c})
	if err := e.Handle(context.Background(), toolEnd("conv-1", 1, "web_page_read", "PAGE TEXT")); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the event's turn", func() bool { return len(sessions.seen()) == 1 })
	if got := sessions.seen()[0]; !strings.Contains(got, "PAGE TEXT") {
		t.Errorf("turn = %q, want the output", got)
	}
	sessions.mu.Lock()
	defer sessions.mu.Unlock()
	if !slices.Equal(sessions.allows[0], PipedTurnTools) {
		t.Errorf("allow = %v, want PipedTurnTools", sessions.allows[0])
	}
}

// Events at max_depth or deeper are not delivered, and the skip is journaled
// once per session and turn; events from the process's own session never
// reach it; events from before the daemon started are not replayed.
func TestEventSkips(t *testing.T) {
	on, f, c := trigger([]string{"tool_end"}, config.EventFilter{}, false)
	_, e, store, sessions := eventSetup(t, memory.Process{ID: "watch", SessionID: "watch", OnEvents: on, EventFilter: f, EventContent: c})
	ctx := context.Background()

	_ = e.Handle(ctx, turnStart("deep", 1, 2))
	_ = e.Handle(ctx, toolEnd("deep", 1, "read_file", ""))
	_ = e.Handle(ctx, toolEnd("deep", 1, "list_files", ""))
	_ = e.Handle(ctx, toolEnd("watch", 1, "read_file", ""))
	old := toolEnd("conv-1", 1, "read_file", "")
	old.TS = time.Now().Add(-time.Hour)
	_ = e.Handle(ctx, old)

	time.Sleep(300 * time.Millisecond)
	if n := len(sessions.seen()); n != 0 {
		t.Fatalf("%d turns ran; every event should have been skipped", n)
	}
	evs, err := store.SessionEventsByAgent("watch")
	if err != nil {
		t.Fatal(err)
	}
	skips := 0
	for _, ev := range evs {
		if ev.Type == evProcessSkipped && strings.Contains(string(ev.Payload), `"max_depth"`) {
			skips++
		}
	}
	if skips != 1 {
		t.Errorf("%d depth skips journaled, want 1 for the deep turn", skips)
	}

}

// A filter narrows events by tool, by session, and by whether the session is
// a conversation or a process's.
func TestEventFilter(t *testing.T) {
	on, f, c := trigger([]string{"tool_end"}, config.EventFilter{Tool: "web_page_read", Sessions: "conversations"}, false)
	_, e, store, sessions := eventSetup(t, memory.Process{ID: "watch", SessionID: "watch", OnEvents: on, EventFilter: f, EventContent: c})
	if err := store.ProcessUpsertDefinition(memory.Process{ID: "other", Tool: "x", Mode: memory.ProcessLive, SessionID: "other", Owner: true}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = e.Handle(ctx, toolEnd("conv-1", 1, "read_file", ""))     // wrong tool
	_ = e.Handle(ctx, toolEnd("other", 1, "web_page_read", ""))  // a process's session
	_ = e.Handle(ctx, toolEnd("conv-1", 2, "web_page_read", "")) // matches
	eventually(t, "the matching event's turn", func() bool { return len(sessions.seen()) >= 1 })
	time.Sleep(300 * time.Millisecond)
	if got := sessions.seen(); len(got) != 1 || !strings.Contains(got[0], `"turn":2`) {
		t.Errorf("turns = %v, want only the matching event", got)
	}
}

// A pipe's report at max_depth is not delivered: the skip is journaled and the
// report reaches the human feed instead.
func TestPipeAtMaxDepthGoesToTheFeed(t *testing.T) {
	r, _, store, _ := eventSetup(t, memory.Process{ID: "watch", SessionID: "watch"})
	if r.pipe(memory.Process{ID: "src", ReportTo: "watch"}, "a finding", 2) {
		t.Fatal("a report at max_depth was delivered")
	}
	if feed := feedText(t, store); !strings.Contains(feed, "at max_depth 2") || !strings.Contains(feed, "a finding") {
		t.Errorf("feed = %q", feed)
	}
}
