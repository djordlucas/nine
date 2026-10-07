package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// deleteHarness builds a daemon over a real store. The cascade is SQL, so the
// in-memory mocks the other daemon tests use cannot exercise it.
func deleteHarness(t *testing.T) (*Daemon, *memory.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nine.db")
	store, err := memtest.OpenAt(t, path)
	if err != nil {
		t.Fatal(err)
	}
	d := New("", nil, nil)
	d.ConfigureMemory(store)
	return d, store, path
}

func seedSession(t *testing.T, s *memory.Store, id string) {
	t.Helper()
	if err := s.ConversationCreate(id); err != nil {
		t.Fatal(err)
	}
	if err := s.SessionEventsAppend([]memory.SessionEvent{{
		AgentID: id, Turn: 1, Type: "turn_start", Payload: json.RawMessage(`{}`),
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.ToolStateSet("cache", id, "k", "v", memory.ToolStateQuota{}); err != nil {
		t.Fatal(err)
	}
	if err := s.JobCreate(memory.Job{
		Handle: "job_" + id, Backend: memory.JobBackendTool, Tool: "t",
		OwnerID: id, State: "running",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteSessionRemovesEverythingKeyedToIt(t *testing.T) {
	d, store, _ := deleteHarness(t)
	seedSession(t, store, "agent-1")

	counts, err := d.DeleteSession(context.Background(), "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Total() < 4 {
		t.Fatalf("cascade removed only %d rows: %+v", counts.Total(), counts)
	}
	if ok, _ := store.SessionExists("agent-1"); ok {
		t.Error("the conversation survived")
	}
	// Deleting a tool job's row *is* its cancellation: the sweeper finds nothing
	// to call.
	if _, found, _ := store.JobGet("job_agent-1"); found {
		t.Error("the session's job outlived it and would keep being called")
	}
}

func TestDeleteSessionRejectsAnUnknownID(t *testing.T) {
	d, _, _ := deleteHarness(t)
	if _, err := d.DeleteSession(context.Background(), "never-existed"); err == nil {
		t.Fatal("deleting an unknown session reported success")
	}
}

// The reaper's exclusions, end to end through the daemon rather than at the
// store. These are what make automatic deletion safe to switch on.
func TestReapSessionsSparesLiveWork(t *testing.T) {
	d, store, dbPath := deleteHarness(t)
	for _, id := range []string{"abandoned", "has-goal", "has-plan"} {
		seedSession(t, store, id)
		memtest.Backdate(t, dbPath, id, 30*24*time.Hour)
	}
	// A pursue session's id is its goal id, so an active goal protects it.
	if err := store.GoalCreate("has-goal", "keep going", "", ""); err != nil {
		t.Fatal(err)
	}
	// A standing agent's session is driven by its process.
	if err := store.ProcessUpsertDefinition(memory.Process{
		ID: "goal:has-plan", Tool: "pursue", Mode: memory.ProcessLive, SessionID: "has-plan", Owner: true,
	}); err != nil {
		t.Fatal(err)
	}

	d.reapSessions(context.Background(), 10*24*time.Hour)

	if ok, _ := store.SessionExists("abandoned"); ok {
		t.Error("an abandoned session survived the reaper")
	}
	if ok, _ := store.SessionExists("has-goal"); !ok {
		t.Error("the reaper deleted a session with an active goal, abandoning the goal")
	}
	if ok, _ := store.SessionExists("has-plan"); !ok {
		t.Error("the reaper deleted a standing agent's session")
	}
}

// A session that came alive between the query and the delete is no longer
// abandoned, and taking it would erase something a human is looking at.
func TestReapSessionsSkipsARunningSession(t *testing.T) {
	d, store, dbPath := deleteHarness(t)
	seedSession(t, store, "live-again")
	memtest.Backdate(t, dbPath, "live-again", 30*24*time.Hour)

	// Mark it running, as an attach would.
	d.mu.Lock()
	d.sessions["live-again"] = &AgentWorker{}
	d.mu.Unlock()

	d.reapSessions(context.Background(), 10*24*time.Hour)

	if ok, _ := store.SessionExists("live-again"); !ok {
		t.Fatal("the reaper deleted a session that had become active")
	}
}

// Retention off must mean off. Getting this wrong on an operator who believed
// they had disabled it would be unrecoverable.
func TestSessionReaperDisabledDeletesNothing(t *testing.T) {
	d, store, dbPath := deleteHarness(t)
	seedSession(t, store, "ancient")
	memtest.Backdate(t, dbPath, "ancient", 365*24*time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	RunSessionReaper(ctx, d, 0)

	if ok, _ := store.SessionExists("ancient"); !ok {
		t.Fatal("retention 0 reaped a session; 0 means disabled, not immediate")
	}
}
