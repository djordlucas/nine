package memory

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

// stateEvents returns the journal's state-change events, oldest first.
func stateEvents(t *testing.T, s *Store) []SessionEvent {
	t.Helper()
	evs, err := s.SessionEventsAfter(0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []SessionEvent
	for _, e := range evs {
		if e.SpanID == stateSpanID {
			out = append(out, e)
		}
	}
	return out
}

func payloadOf(t *testing.T, e SessionEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(e.Payload, &m); err != nil {
		t.Fatalf("payload %s: %v", e.Payload, err)
	}
	return m
}

// wantTypes checks the state-change events are exactly these, in order, each
// attributed to the agent given beside it.
func wantTypes(t *testing.T, s *Store, want ...[2]string) []SessionEvent {
	t.Helper()
	got := stateEvents(t, s)
	if len(got) != len(want) {
		var types []string
		for _, e := range got {
			types = append(types, e.Type)
		}
		t.Fatalf("got %d state events %v, want %d", len(got), types, len(want))
	}
	for i, w := range want {
		if got[i].Type != w[0] || got[i].AgentID != w[1] {
			t.Errorf("event %d: got %s by %q, want %s by %q", i, got[i].Type, got[i].AgentID, w[0], w[1])
		}
		if got[i].Turn != 0 {
			t.Errorf("event %d: turn %d, want 0", i, got[i].Turn)
		}
	}
	return got
}

func TestJournalHumanRequests(t *testing.T) {
	s := openTestStore(t)
	exp := time.Now().Add(time.Hour)
	if err := s.HumanRequestCreate("q1", "conv1", "Proceed?", []string{"yes", "no"}, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.HumanRequestAnswer("q1", "yes"); err != nil {
		t.Fatal(err)
	}
	if err := s.HumanRequestCreate("q2", "conv1", "Again?", nil, exp); err != nil {
		t.Fatal(err)
	}
	if err := s.HumanRequestMarkTimedOut("q2"); err != nil {
		t.Fatal(err)
	}
	// Already settled: nothing changes, so nothing is journaled.
	if err := s.HumanRequestMarkTimedOut("q2"); err != nil {
		t.Fatal(err)
	}
	if err := s.HumanRequestAnswer("missing", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.HumanRequestCreate("q3", "conv2", "Stale?", nil, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.HumanRequestExpireStale(time.Now()); err != nil {
		t.Fatal(err)
	}

	pending, err := s.HumanRequestListPending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %v, %v; want none", pending, err)
	}
	if err := s.HumanRequestCreate("q4", "conv3", "Open?", []string{"a"}, exp); err != nil {
		t.Fatal(err)
	}
	pending, err = s.HumanRequestListPending()
	if err != nil || len(pending) != 1 || pending[0].ID != "q4" || pending[0].Options[0] != "a" {
		t.Fatalf("pending = %+v, %v; want q4", pending, err)
	}

	evs := wantTypes(t, s,
		[2]string{EvHumanInputRequested, "conv1"},
		[2]string{EvHumanInputAnswered, "conv1"},
		[2]string{EvHumanInputRequested, "conv1"},
		[2]string{EvHumanInputTimedOut, "conv1"},
		[2]string{EvHumanInputRequested, "conv2"},
		[2]string{EvHumanInputTimedOut, "conv2"},
		[2]string{EvHumanInputRequested, "conv3"},
	)
	if p := payloadOf(t, evs[0]); p["question"] != "Proceed?" || p["id"] != "q1" {
		t.Errorf("requested payload = %v", p)
	}
	if p := payloadOf(t, evs[1]); p["answer"] != "yes" {
		t.Errorf("answered payload = %v", p)
	}
}

func TestJournalCapabilities(t *testing.T) {
	s := openTestStore(t)
	if err := s.CapabilityRequestCreate("r1", "conv1", "fetcher", "net.http", `{"hosts":["a.example"]}`, "to fetch"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CapabilityRequestDecide("r1", CapabilityRequestApproved, "g1"); err != nil || !ok {
		t.Fatalf("decide: %v %v", ok, err)
	}
	// A second decision changes nothing.
	if ok, _ := s.CapabilityRequestDecide("r1", CapabilityRequestDenied, ""); ok {
		t.Fatal("second decision took effect")
	}
	if ok, err := s.CapabilityGrantRevoke("g1"); err != nil || !ok {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if ok, _ := s.CapabilityGrantRevoke("g1"); ok {
		t.Fatal("second revoke took effect")
	}

	evs := wantTypes(t, s,
		[2]string{EvCapabilityRequested, "conv1"},
		[2]string{EvCapabilityDecided, "conv1"},
		[2]string{EvCapabilityRevoked, DaemonAgentID},
	)
	if p := payloadOf(t, evs[0]); p["capability"] != "net.http" || p["tool"] != "fetcher" {
		t.Errorf("requested payload = %v", p)
	}
	if p := payloadOf(t, evs[1]); p["status"] != CapabilityRequestApproved || p["grant_id"] != "g1" {
		t.Errorf("decided payload = %v", p)
	}
}

func TestJournalNotifications(t *testing.T) {
	s := openTestStore(t)
	if err := s.UserNotificationCreate("n1", "", "backup done"); err != nil {
		t.Fatal(err)
	}
	if err := s.UserNotificationCreate("n1", "", "backup done"); err != nil {
		t.Fatal(err)
	}
	if err := s.UserNotificationCreate("n2", "conv1", "found it"); err != nil {
		t.Fatal(err)
	}
	evs := wantTypes(t, s,
		[2]string{EvNotification, DaemonAgentID},
		[2]string{EvNotification, "conv1"},
	)
	if p := payloadOf(t, evs[0]); p["message"] != "backup done" {
		t.Errorf("payload = %v", p)
	}
}

func TestJournalGoals(t *testing.T) {
	s := openTestStore(t)
	if err := s.GoalCreate("g1", "learn Go", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.GoalCreate("g1", "learn Go", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.GoalCreate("g2", "read the spec", "g1", "goal"); err != nil {
		t.Fatal(err)
	}
	if err := s.GoalUpdateStatus("g1", "paused"); err != nil {
		t.Fatal(err)
	}
	if err := s.GoalUpdateStatus("missing", "paused"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GoalDelete("g1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GoalDelete("g1"); err != nil {
		t.Fatal(err)
	}
	evs := wantTypes(t, s,
		[2]string{EvGoalCreated, "g1"},
		[2]string{EvGoalCreated, "g2"},
		[2]string{EvGoalStatus, "g1"},
		[2]string{EvGoalDeleted, "g1"},
	)
	if p := payloadOf(t, evs[1]); p["parent_id"] != "g1" {
		t.Errorf("created payload = %v", p)
	}
	ids, _ := payloadOf(t, evs[3])["ids"].([]any)
	if len(ids) != 2 {
		t.Errorf("deleted ids = %v, want the goal and its sub-goal", ids)
	}
}

func TestJournalWorkflows(t *testing.T) {
	s := openTestStore(t)
	wf, err := s.WorkflowCreate("conv1", "ship", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkflowUpdate(wf.ID, wf.Steps[0].ID, "done", "built", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkflowFail(wf.ID, false); err != nil {
		t.Fatal(err)
	}
	wf2, err := s.WorkflowCreate("conv2", "other", []string{"one"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WorkflowCancel(wf2.ID); err != nil {
		t.Fatal(err)
	}
	evs := wantTypes(t, s,
		[2]string{EvWorkflowCreated, "conv1"},
		[2]string{EvWorkflowStep, "conv1"},
		[2]string{EvWorkflowFailed, "conv1"},
		[2]string{EvWorkflowCreated, "conv2"},
		[2]string{EvWorkflowCancelled, "conv2"},
	)
	if p := payloadOf(t, evs[1]); p["status"] != "done" || p["workflow_status"] == nil {
		t.Errorf("step payload = %v", p)
	}
}

func TestJournalHookAndBounds(t *testing.T) {
	s := openTestStore(t)
	oldest, latest, err := s.SessionEventsBounds()
	if err != nil || oldest != 0 || latest != 0 {
		t.Fatalf("empty bounds = %d, %d, %v", oldest, latest, err)
	}

	var calls atomic.Int32
	s.OnJournal(func() { calls.Add(1) })
	if err := s.GoalCreate("g1", "a", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SessionEventsAppend([]SessionEvent{{AgentID: "a", Turn: 1, SpanID: "t", Type: "turn_start"}}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("hook called %d times, want 2", calls.Load())
	}
	s.OnJournal(nil)
	if err := s.GoalCreate("g2", "b", "", ""); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Errorf("hook called after removal")
	}

	// A scrub of every row keeps the high-water mark: the next event's seq
	// still exceeds a cursor taken before the scrub.
	if _, err := s.SessionEventsScrub(0, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := s.SessionEventsScrub(0, time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	oldest, latest, err = s.SessionEventsBounds()
	if err != nil || oldest != 0 || latest != 3 {
		t.Fatalf("after scrub bounds = %d, %d, %v; want 0, 3", oldest, latest, err)
	}
}
