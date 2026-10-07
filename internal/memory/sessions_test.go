package memory_test

import (
	"encoding/json"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// seedSession creates a conversation and one row in every table keyed to it, so
// a cascade test asserts against a session that is actually entangled.
func seedSession(t *testing.T, s *memory.Store, id string) {
	t.Helper()
	if err := s.ConversationCreate(id); err != nil {
		t.Fatal(err)
	}
	if err := s.ConversationNameSave(id, "name of "+id); err != nil {
		t.Fatal(err)
	}
	if err := s.NotificationCreate(memory.NewID(), id, "a note", false); err != nil {
		t.Fatal(err)
	}
	if err := s.UserNotificationCreate(memory.NewID(), id, "for the human"); err != nil {
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

func TestSessionDeleteCascades(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	seedSession(t, store, "agent-doomed")
	seedSession(t, store, "agent-keeper")

	// Tool-scoped state (scope_key "") belongs to no session and must survive.
	if err := store.ToolStateSet("cache", "", "shared", "v", memory.ToolStateQuota{}); err != nil {
		t.Fatal(err)
	}

	counts, err := store.SessionDelete("agent-doomed")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int
	}{
		{"conversations", counts.Conversations},
		{"events", counts.Events},
		{"notifications", counts.Notifications},
		{"user_notifications", counts.UserNotifications},
		{"tool_state", counts.ToolState},
		{"jobs", counts.Jobs},
	} {
		if c.got != 1 {
			t.Errorf("%s removed %d rows, want 1", c.name, c.got)
		}
	}

	if ok, _ := store.SessionExists("agent-doomed"); ok {
		t.Error("the conversation row survived")
	}
	if name := store.ConversationNameLoad("agent-doomed"); name != "" {
		t.Errorf("the display name outlived its session: %q", name)
	}

	// Everything belonging to the other session is untouched.
	if ok, _ := store.SessionExists("agent-keeper"); !ok {
		t.Fatal("the cascade took another session")
	}
	if keys, _ := store.ToolStateList("cache", "agent-keeper", ""); len(keys) != 1 {
		t.Errorf("another session's tool state was deleted: %v", keys)
	}
	if _, found, _ := store.JobGet("job_agent-keeper"); !found {
		t.Error("another session's job was deleted")
	}

	// Shared, tool-scoped state is nobody's session to delete.
	if keys, _ := store.ToolStateList("cache", "", ""); len(keys) != 1 {
		t.Errorf("tool-scoped state was deleted with a conversation: %v", keys)
	}
}

// The exclusions are what make automatic deletion safe to switch on. Both kinds
// of protected session are idle *by design* — a standing agent that wakes weekly
// looks stale after ten days precisely because it is working.
func TestSessionsReapableProtectsLiveWork(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)

	for _, id := range []string{"plain-stale", "goal-session", "standing", "fresh"} {
		if err := store.ConversationCreate(id); err != nil {
			t.Fatal(err)
		}
	}
	// Age three of them; "fresh" keeps its creation timestamp.
	for _, id := range []string{"plain-stale", "goal-session", "standing"} {
		if err := store.ExecRaw(
			`UPDATE conversations SET updated_at = ? WHERE id = ?`,
			old.UTC().Format("2006-01-02T15:04:05.000000Z"), id); err != nil {
			t.Fatal(err)
		}
	}

	// A pursue session's id IS its goal id, so an active goal protects it.
	if err := store.GoalCreate("goal-session", "keep pursuing", "", ""); err != nil {
		t.Fatal(err)
	}
	// A standing agent's session is driven by its process.
	if err := store.ProcessUpsertDefinition(memory.Process{
		ID: "goal:standing", Tool: "pursue", Mode: memory.ProcessLive, SessionID: "standing", Owner: true,
	}); err != nil {
		t.Fatal(err)
	}

	reapable, err := store.SessionsReapable(10 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range reapable {
		got[s.ID] = true
	}

	if !got["plain-stale"] {
		t.Error("an abandoned session was not reapable")
	}
	if got["goal-session"] {
		t.Error("a session with an active goal was reapable — its goal would be silently abandoned")
	}
	if got["standing"] {
		t.Error("a standing agent's session was reapable — configured behaviour would vanish")
	}
	if got["fresh"] {
		t.Error("a session inside the retention window was reapable")
	}
}

// Retention disabled must mean disabled, not "reap everything".
func TestSessionsReapableDisabled(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConversationCreate("whatever"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{0, -time.Hour} {
		got, err := store.SessionsReapable(d)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("maxAge=%v returned %d sessions, want none", d, len(got))
		}
	}
}

// Age is measured from last activity, not creation: a long-lived session used
// today is not stale.
func TestSessionAgeIsFromLastActivity(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConversationCreate("long-lived"); err != nil {
		t.Fatal(err)
	}
	if err := store.ExecRaw(
		`UPDATE conversations SET created_at = ? WHERE id = ?`,
		time.Now().Add(-365*24*time.Hour).UTC().Format("2006-01-02T15:04:05.000000Z"),
		"long-lived"); err != nil {
		t.Fatal(err)
	}

	reapable, err := store.SessionsReapable(10 * 24 * time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(reapable) != 0 {
		t.Fatalf("a year-old session used today was reapable: %+v", reapable)
	}
}
