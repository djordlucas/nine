package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/embed/keyword"
	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/runtime"
)

// The background snapshots freeze what each kind of background work does
// today — a goal session, a standing agent, the self-reflection session, a
// condition trigger and a standing tool — through the production assembly.
// They are the before-picture for adr/process-sessions.md phase 1, which moves
// all five onto process sessions and must leave these files unchanged.

// A predicate tool in the shape condition triggers use: silent unless it finds
// something.
const (
	predicateManifest = `
name = "predicate"
kind = "js"
entrypoint = "./predicate.js"
description = "Report only when there is something."
resumable = true
`
	predicateSource = `
export default function (args) { return args.found ? "found: " + args.found : ""; }
`
)

// startBackground starts a harness for background sessions, in the given tool
// exposition arm ("" is the default).
func startBackground(t *testing.T, arm string) (*live, *snapshotProvider) {
	t.Helper()
	p := &snapshotProvider{script: func(int, llm.Request) llm.Response { return answer("done") }}
	h := &Harness{Embedder: keyword.New(), BaseDir: t.TempDir()}
	c := Case{
		ID:          "background-snapshot",
		TimeoutSecs: 60,
		Setup: Setup{Files: map[string]string{
			"notes/plan.md":         "# Plan\n\nShip the boundary.\n",
			".tools/predicate.toml": predicateManifest,
			".tools/predicate.js":   predicateSource,
		}},
	}
	if arm != "" {
		c.Session.Config = map[string]any{ArmKey: arm}
	}
	lv, err := h.start(context.Background(), &c, p)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(lv.Result.Close)
	return lv, p
}

// waitForCall returns the first request for session id whose last message
// contains marker ("" matches any), waiting up to 20s.
func waitForCall(t *testing.T, p *snapshotProvider, id, marker string) llm.Request {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		for _, req := range p.calls {
			if !strings.Contains(req.System, "Session ID: "+id+"\n") || len(req.Messages) == 0 {
				continue
			}
			if strings.Contains(req.Messages[len(req.Messages)-1].Text, marker) {
				p.mu.Unlock()
				return req
			}
		}
		p.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no model call for session %q containing %q", id, marker)
	return llm.Request{}
}

// turnEvents drains the journal and returns session id's events for the first
// turn whose turn_start contains marker.
func turnEvents(t *testing.T, lv *live, id, marker string) []memory.SessionEvent {
	t.Helper()
	lv.CloseJournal()
	events, err := lv.Result.Store.SessionEventsByAgent(id)
	if err != nil {
		t.Fatal(err)
	}
	turn := -1
	for _, ev := range events {
		if ev.Type == "turn_start" && strings.Contains(string(ev.Payload), marker) {
			turn = ev.Turn
			break
		}
	}
	var out []memory.SessionEvent
	for _, ev := range events {
		if ev.Turn == turn {
			out = append(out, ev)
		}
	}
	return out
}

func snapshotOne(t *testing.T, lv *live, name string, req llm.Request, events []memory.SessionEvent) {
	t.Helper()
	lv.Result.Events = events
	checkSnapshot(t, name, renderSnapshot(t, []llm.Request{req}, lv.Result))
}

// spawnGoalBound creates goal id and its background session the way goal_create
// and [[agent]] do, waking every second instead of every five minutes.
func spawnGoalBound(t *testing.T, lv *live, id, role, description string) {
	t.Helper()
	if err := lv.Result.Store.GoalCreate(id, description, "", runtime.ConfigGoalOrigin); err != nil {
		t.Fatal(err)
	}
	if _, err := lv.Daemon.SpawnStandingSession(context.Background(), id, role, false, time.Second, "", config.BudgetConfig{}); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundSnapshots(t *testing.T) {
	t.Run("goal-session", func(t *testing.T) {
		lv, p := startBackground(t, "")
		spawnGoalBound(t, lv, "tidy-notes", runtime.PursueRole, "Keep notes/plan.md tidy.")
		req := waitForCall(t, p, "tidy-notes", "")
		snapshotOne(t, lv, "background-goal-session", req, turnEvents(t, lv, "tidy-notes", ""))
	})

	t.Run("standing-agent", func(t *testing.T) {
		lv, p := startBackground(t, "")
		spawnGoalBound(t, lv, "sec-watch", "monitor", "Watch the workspace for files that look like secrets.")
		req := waitForCall(t, p, "sec-watch", "")
		snapshotOne(t, lv, "background-standing-agent", req, turnEvents(t, lv, "sec-watch", ""))
	})

	t.Run("self-reflection", func(t *testing.T) {
		lv, p := startBackground(t, "")
		if err := runtime.ReconcileSelfReflection(lv.Result.Store, time.Second); err != nil {
			t.Fatal(err)
		}
		lv.Processes.Wake()
		id := runtime.SelfReflectionAgentID
		req := waitForCall(t, p, id, "")
		snapshotOne(t, lv, "background-self-reflection", req, turnEvents(t, lv, id, ""))
	})

	t.Run("condition-trigger", func(t *testing.T) {
		lv, p := startBackground(t, "")
		lv.Tools.Load(context.Background(), nil)
		// The agent the trigger wakes; its own idle turns are told apart from the
		// woken one by the finding in the woken turn's text.
		spawnGoalBound(t, lv, "watcher", "monitor", "Act on what the predicate finds.")
		runStanding(t, lv, memory.Process{
			ID: "when-watcher", Tool: "predicate",
			Args: `{"found":"a stray api key in notes/"}`, IntervalSecs: 1, ReportTo: "watcher",
		})
		marker := "a stray api key"
		req := waitForCall(t, p, "watcher", marker)
		snapshotOne(t, lv, "background-condition-trigger", req, turnEvents(t, lv, "watcher", marker))
	})

	// The deferred arm: the goal session's catalog is indexed, the standing
	// agent's and the condition trigger's are small enough to advertise whole.
	t.Run("goal-session-deferred", func(t *testing.T) {
		lv, p := startBackground(t, ArmDeferred)
		spawnGoalBound(t, lv, "tidy-notes", runtime.PursueRole, "Keep notes/plan.md tidy.")
		req := waitForCall(t, p, "tidy-notes", "")
		snapshotOne(t, lv, "background-goal-session-deferred", req, turnEvents(t, lv, "tidy-notes", ""))
	})

	t.Run("standing-agent-deferred", func(t *testing.T) {
		lv, p := startBackground(t, ArmDeferred)
		spawnGoalBound(t, lv, "sec-watch", "monitor", "Watch the workspace for files that look like secrets.")
		req := waitForCall(t, p, "sec-watch", "")
		snapshotOne(t, lv, "background-standing-agent-deferred", req, turnEvents(t, lv, "sec-watch", ""))
	})

	t.Run("condition-trigger-deferred", func(t *testing.T) {
		lv, p := startBackground(t, ArmDeferred)
		lv.Tools.Load(context.Background(), nil)
		spawnGoalBound(t, lv, "watcher", "monitor", "Act on what the predicate finds.")
		runStanding(t, lv, memory.Process{
			ID: "when-watcher", Tool: "predicate",
			Args: `{"found":"a stray api key in notes/"}`, IntervalSecs: 1, ReportTo: "watcher",
		})
		marker := "a stray api key"
		req := waitForCall(t, p, "watcher", marker)
		snapshotOne(t, lv, "background-condition-trigger-deferred", req, turnEvents(t, lv, "watcher", marker))
	})

	t.Run("standing-tool", func(t *testing.T) {
		lv, _ := startBackground(t, "")
		lv.Tools.Load(context.Background(), nil)
		runStanding(t, lv, memory.Process{
			ID: "scan", Tool: "predicate", Args: `{"found":"two new files"}`, IntervalSecs: 1,
		})
		// A standing tool calls no model; what it produces is the human feed.
		var feed []memory.UserNotification
		deadline := time.Now().Add(20 * time.Second)
		for len(feed) == 0 && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			all, err := lv.Result.Store.UserNotificationList(false)
			if err != nil {
				t.Fatal(err)
			}
			feed = all
		}
		if len(feed) == 0 {
			t.Fatal("the standing tool reported nothing to the human feed")
		}
		first := feed[0]
		raw, err := json.MarshalIndent(map[string]any{
			"agent_id": first.AgentID, "message": first.Message,
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		checkSnapshot(t, "background-standing-tool", append(raw, '\n'))
	})
}

// runStanding records a running standing tool, for the harness's process
// runner to drive, as cmd/nine's does.
func runStanding(t *testing.T, lv *live, st memory.Process) {
	t.Helper()
	store := lv.Result.Store
	if err := store.ProcessUpsertDefinition(st); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessSetState(st.ID, memory.ProcessRunning); err != nil {
		t.Fatal(err)
	}
}
