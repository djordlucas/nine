package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/toolvm"
)

func standingHost(t *testing.T, name, manifest, src string, store *memory.Store) *toolvm.Host {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".js"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := toolvm.Config{UserDir: dir}
	if store != nil {
		cfg.StateStore = newToolStateStore(store)
		cfg.Grants = map[string]toolvm.Grant{
			name: {State: &toolvm.StateGrant{Scope: toolvm.StateScopeTool}},
		}
	}
	h, err := toolvm.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close(context.Background()) }) //nolint:errcheck
	h.Load(ctx, nil)
	if h.Get(name) == nil {
		t.Fatalf("tool %q did not load: %+v", name, h.Status())
	}
	return h
}

const watcherManifest = `
name = "watcher"
kind = "js"
entrypoint = "./watcher.js"
description = "One pass over two calls, then report."
resumable = true
`

// Two calls per cycle, then a result — the shape a standing tool actually has.
const watcherSrc = `
import { again } from "nine:job";
export default function (args, job) {
  if (!job.cursor) return again({ cursor: "half", afterMs: 0 });
  return args.quiet ? "" : "found something";
}
`

func declare(t *testing.T, store *memory.Store, id, tool string, interval int) {
	t.Helper()
	if err := store.StandingToolUpsertDefinition(memory.StandingTool{
		ID: id, Tool: tool, Args: `{}`, IntervalSecs: interval,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StandingToolSetState(id, memory.StandingRunning); err != nil {
		t.Fatal(err)
	}
}

// A cycle spans as many calls as the tool asks for, and completing one resets the
// cursor so the next cycle starts from the tool's own beginning.
func TestStandingToolCompletesACycleAndResets(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	host := standingHost(t, "watcher", watcherManifest, watcherSrc, nil)
	r := NewStandingRunner(store, host, 1, 2)
	declare(t, store, "w1", "watcher", 3600)

	// Call 1: asks to continue.
	r.runDue(context.Background())
	got, _, _ := store.StandingToolGet("w1")
	if got.Cursor != "half" {
		t.Fatalf("after call 1 cursor = %q, want half", got.Cursor)
	}
	if got.Cycles != 0 {
		t.Fatalf("a cycle completed early (cycles=%d)", got.Cycles)
	}

	// Call 2: completes the cycle.
	time.Sleep(5 * time.Millisecond)
	r.runDue(context.Background())
	got, _, _ = store.StandingToolGet("w1")
	if got.Cycles != 1 {
		t.Fatalf("cycles = %d, want 1", got.Cycles)
	}
	if got.Cursor != "" {
		t.Fatalf("cursor = %q after a completed cycle, want it reset", got.Cursor)
	}
	// The next cycle waits for the trigger, not the tool's after_ms.
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
			host := standingHost(t, "watcher", watcherManifest, watcherSrc, nil)
			r := NewStandingRunner(store, host, 1, 2)
			if err := store.StandingToolUpsertDefinition(memory.StandingTool{
				ID: "w1", Tool: "watcher", Args: tc.args, IntervalSecs: 3600,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.StandingToolSetState("w1", memory.StandingRunning); err != nil {
				t.Fatal(err)
			}

			for range 2 {
				r.runDue(context.Background())
				time.Sleep(5 * time.Millisecond)
			}

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
	host := standingHost(t, "broken", `
name = "broken"
kind = "js"
entrypoint = "./broken.js"
description = "Always throw."
resumable = true
`, `export default function () { throw new Error("upstream is down"); }`, nil)
	r := NewStandingRunner(store, host, 1, 2)
	declare(t, store, "b1", "broken", 1)

	for range StandingFailureThreshold + 3 {
		// Force it due, so the backoff does not stall the test.
		if _, err := store.StandingToolFail("b1", "", time.Now(), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := store.StandingToolSetState("b1", memory.StandingRunning); err != nil {
			t.Fatal(err)
		}
		if err := store.StandingToolUpsertDefinition(memory.StandingTool{
			ID: "b1", Tool: "broken", Args: `{}`, IntervalSecs: 1,
		}); err != nil {
			t.Fatal(err)
		}
		r.runDue(context.Background())
	}

	got, _, _ := store.StandingToolGet("b1")
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
	host := standingHost(t, "watcher", watcherManifest, watcherSrc, nil)
	r := NewStandingRunner(store, host, 1, 2)
	declare(t, store, "w1", "watcher", 1)

	if _, err := store.StandingToolSetState("w1", memory.StandingStopped); err != nil {
		t.Fatal(err)
	}
	r.runDue(context.Background())
	if got, _, _ := store.StandingToolGet("w1"); got.Calls != 0 {
		t.Fatalf("a stopped standing tool was called %d times", got.Calls)
	}

	// The operator's file still declares it, as it did before they stopped it.
	ReconcileStandingTools(store, []config.StandingToolConfig{
		{ID: "w1", Tool: "watcher", Interval: "1s"},
	})
	if got, _, _ := store.StandingToolGet("w1"); got.State != memory.StandingStopped {
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
	ReconcileStandingTools(store, []config.StandingToolConfig{
		{ID: "w1", Tool: "watcher", Interval: "1s", Args: map[string]any{"path": "/a"}},
	})
	if _, err := store.StandingToolAdvance("w1", "mid-cycle", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.StandingToolGet("w1"); got.Cursor != "mid-cycle" {
		t.Fatal("precondition: the run should hold a cursor")
	}

	ReconcileStandingTools(store, []config.StandingToolConfig{
		{ID: "w1", Tool: "watcher", Interval: "1s", Args: map[string]any{"path": "/b"}},
	})
	got, _, _ := store.StandingToolGet("w1")
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
	ReconcileStandingTools(store, []config.StandingToolConfig{
		{ID: "w1", Tool: "watcher", Interval: "1s", Enabled: &no},
	})
	got, found, err := store.StandingToolGet("w1")
	if err != nil || !found {
		t.Fatalf("the block was not declared: found=%v err=%v", found, err)
	}
	if got.State != memory.StandingStopped {
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
