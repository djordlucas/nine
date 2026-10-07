package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"nine/internal/llm"
	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

// operatorHarness is a daemon over a real store, with a process store and a
// journal, so the operator verbs run end to end through the socket.
func operatorHarness(t *testing.T, provider llm.Provider) (*memory.Store, string) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	d, sock := startDaemon(t, makeFactory(provider), nil, nil)
	d.ConfigureMemory(store)
	d.ConfigureProcesses(store, nil)
	d.SetEventSink(runtime.NewSQLEventSinkForTest(store))
	return store, sock
}

func TestGoalCreateAndDeleteOverTheWire(t *testing.T) {
	store, sock := operatorHarness(t, seqProvider(nil))
	c := dial(t, sock)

	res, err := c.CreateGoal("watch the repo", "")
	if err != nil {
		t.Fatalf("CreateGoal: %v", err)
	}
	if res.PursueSession != "spawned" {
		t.Errorf("pursue_session = %q, want spawned", res.PursueSession)
	}
	var g memory.Goal
	if err := json.Unmarshal(res.Goal, &g); err != nil {
		t.Fatal(err)
	}
	if g.ID == "" || g.Description != "watch the repo" || g.ParentType != "operator" || g.Status != "active" {
		t.Fatalf("created goal = %+v", g)
	}

	// A sub-goal records its parent and gets no session of its own.
	sub, err := dial(t, sock).CreateGoal("check the PRs", g.ID)
	if err != nil {
		t.Fatalf("CreateGoal(sub): %v", err)
	}
	if sub.PursueSession != "none" {
		t.Errorf("sub-goal pursue_session = %q, want none", sub.PursueSession)
	}
	if _, err := dial(t, sock).CreateGoal("orphan", "no-such-goal"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("CreateGoal under a missing parent: err = %v, want not found", err)
	}

	del, err := dial(t, sock).DeleteGoal(g.ID)
	if err != nil {
		t.Fatalf("DeleteGoal: %v", err)
	}
	if len(del.Deleted) != 2 || del.Deleted[0] != g.ID {
		t.Errorf("deleted = %v, want the goal then its sub-goal", del.Deleted)
	}
	if !del.SessionStopped {
		t.Error("session_stopped = false; the goal's pursue session was running")
	}
	if left, _ := store.GoalList(); len(left) != 0 {
		t.Errorf("goals left after delete: %+v", left)
	}
	if procs, _ := store.ProcessesOfSession(g.ID); len(procs) != 0 {
		t.Errorf("the goal's processes outlived it: %+v", procs)
	}

	if _, err := dial(t, sock).DeleteGoal(g.ID); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("deleting twice: err = %v, want not found", err)
	}
}

// A goal nine.toml declares is re-seeded at every boot, so deleting it over the
// wire would be undone silently. It is refused as a conflict instead.
func TestGoalDeleteRefusesAConfigGoal(t *testing.T) {
	store, sock := operatorHarness(t, seqProvider(nil))
	if err := store.GoalCreate("sec-watch", "watch", "", runtime.ConfigGoalOrigin); err != nil {
		t.Fatal(err)
	}
	_, err := dial(t, sock).DeleteGoal("sec-watch")
	if err == nil || !strings.Contains(err.Error(), protocol.ConflictPrefix) {
		t.Fatalf("err = %v, want a %q error", err, protocol.ConflictPrefix)
	}
	if g, _ := store.GoalGet("sec-watch"); g == nil {
		t.Error("the config goal was deleted anyway")
	}
}

func TestListSkillsOverTheWire(t *testing.T) {
	store, sock := operatorHarness(t, seqProvider(nil))
	if err := store.SkillUpsert(memory.Skill{Name: "triage", Description: "sort issues", Tags: []string{"ops"}, Content: "body", Source: memory.SkillSourceUser}); err != nil {
		t.Fatal(err)
	}
	skills, err := dial(t, sock).ListSkills()
	if err != nil {
		t.Fatalf("ListSkills: %v", err)
	}
	var found bool
	for _, s := range skills {
		if s.Name == "triage" {
			found = true
			if s.Source != "user" || s.Description != "sort issues" || len(s.Tags) != 1 {
				t.Errorf("skill = %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("triage missing from %+v", skills)
	}
}

// History and the journal are read through the daemon, after a turn, without
// attaching — a read must not register the caller as attached.
func TestSessionHistoryAndEventsOverTheWire(t *testing.T) {
	_, sock := operatorHarness(t, seqProvider([]llm.Response{finalResp("hello back")}))
	c := dial(t, sock)
	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Turn(id, "hello"); err != nil {
		t.Fatal(err)
	}

	// The journal is written asynchronously; wait for the turn to land.
	var history []protocol.Msg
	deadline := time.Now().Add(5 * time.Second)
	for {
		history, err = dial(t, sock).SessionHistory(id)
		if err != nil {
			t.Fatalf("SessionHistory: %v", err)
		}
		if len(history) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(history) < 2 || history[0].Type != protocol.TypeHistoryUser || history[0].Text != "hello" ||
		history[len(history)-1].Type != protocol.TypeResponse || history[len(history)-1].Text != "hello back" {
		t.Fatalf("history = %+v, want the prompt then the response", history)
	}

	events, err := dial(t, sock).SessionEvents(id, -1)
	if err != nil {
		t.Fatalf("SessionEvents: %v", err)
	}
	if len(events) == 0 || events[0].Type != "turn_start" || events[0].Turn != 1 {
		t.Fatalf("events = %+v, want turn 1 starting with turn_start", events)
	}
	if none, err := dial(t, sock).SessionEvents(id, 99); err != nil || len(none) != 0 {
		t.Errorf("SessionEvents(turn 99) = %v, %v; want empty, nil", none, err)
	}

	if _, err := dial(t, sock).SessionHistory("no-such-session"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("SessionHistory(unknown) err = %v, want not found", err)
	}
	if _, err := dial(t, sock).SessionEvents("no-such-session", 0); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("SessionEvents(unknown) err = %v, want not found", err)
	}
}

// A watcher sees a turn another connection drives: its outcome and done.
func TestWatchFollowsAnotherConnectionsTurn(t *testing.T) {
	_, sock := operatorHarness(t, seqProvider([]llm.Response{finalResp("watched reply")}))
	c := dial(t, sock)
	id, err := c.NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	w := dial(t, sock)
	if err := w.Watch(id); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if err := dial(t, sock).Watch("no-such-session"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("Watch(unknown) err = %v, want not found", err)
	}

	go c.Turn(id, "hi") //nolint:errcheck

	got := make(chan []protocol.Msg, 1)
	go func() {
		var seen []protocol.Msg
		for {
			m, err := w.NextEvent()
			if err != nil {
				break
			}
			seen = append(seen, m)
			if m.Type == protocol.TypeDone {
				break
			}
		}
		got <- seen
	}()

	select {
	case seen := <-got:
		if len(seen) < 2 {
			t.Fatalf("watch saw %+v, want at least a response and done", seen)
		}
		resp := seen[len(seen)-2]
		if resp.Type != protocol.TypeResponse || resp.Text != "watched reply" {
			t.Errorf("before done: %+v, want the response", resp)
		}
		if seen[len(seen)-1].Type != protocol.TypeDone {
			t.Errorf("last = %+v, want done", seen[len(seen)-1])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watcher never saw the turn finish")
	}
}

// A turn submitted while another runs is queued, and says so; a turn that runs
// and fails says that instead of looking like a refused request.
func TestSubmitTurnReportsQueuedAndFailed(t *testing.T) {
	release := make(chan struct{})
	var calls atomic.Int32
	provider := llm.ProviderFunc(func(ctx context.Context, _ llm.Request) (llm.Response, error) {
		if calls.Add(1) == 1 {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return llm.Response{}, errors.New("model unreachable")
		}
		return finalResp("ok"), nil
	})
	_, sock := operatorHarness(t, provider)
	id, err := dial(t, sock).NewConversation()
	if err != nil {
		t.Fatal(err)
	}

	first := make(chan protocol.TurnOutcome, 1)
	firstDetail := make(chan string, 1)
	go func() {
		o, d, err := dial(t, sock).SubmitTurn(id, "first", false)
		if err != nil {
			d = err.Error()
		}
		first <- o
		firstDetail <- d
	}()

	// Wait until the first turn holds the worker, then submit a second.
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	outcome, detail, err := dial(t, sock).SubmitTurn(id, "second", false)
	if err != nil || outcome != protocol.TurnQueued || !strings.Contains(detail, "queued") {
		t.Errorf("second turn = (%v, %q, %v), want queued", outcome, detail, err)
	}

	close(release)
	select {
	case o := <-first:
		if d := <-firstDetail; o != protocol.TurnFailed || !strings.Contains(d, "model unreachable") {
			t.Errorf("first turn = (%v, %q), want failed with the model's error", o, d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("first turn never settled")
	}
}
