package runtime_test

import (
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
	"nine/internal/protocol"
	"nine/internal/runtime"
)

func TestFollowStart(t *testing.T) {
	for _, c := range []struct {
		name                 string
		after                int64
		tail                 int
		oldest, latest, want int64
		reset                bool
	}{
		{"new, empty journal", 0, 50, 0, 0, 0, false},
		{"new, from now", 0, 0, 1, 100, 100, false},
		{"new, with a tail", 0, 10, 1, 100, 90, false},
		{"new, tail past the oldest", 0, 500, 60, 100, 59, false},
		{"new, tail capped", 0, 5000, 1, 20000, 19000, false},
		{"resume", 80, 0, 1, 100, 80, false},
		{"resume at the end", 100, 0, 1, 100, 100, false},
		{"resume just before the oldest", 59, 0, 60, 100, 59, false},
		{"resume into scrubbed events", 40, 0, 60, 100, 100, true},
		{"resume after a full scrub", 40, 0, 0, 100, 100, true},
		{"resume caught up after a full scrub", 100, 0, 0, 100, 100, false},
		{"a seq never assigned", 500, 0, 1, 100, 100, true},
		{"too far behind", 5, 0, 1, 20000, 20000, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, reset := runtime.FollowStart(c.after, c.tail, c.oldest, c.latest)
			if got != c.want || reset != c.reset {
				t.Errorf("followStart(%d, %d, %d, %d) = %d, %v; want %d, %v",
					c.after, c.tail, c.oldest, c.latest, got, reset, c.want, c.reset)
			}
		})
	}
}

// followHarness is a daemon over a real store whose journal writes ring the
// followers, as assembly wires it.
func followHarness(t *testing.T) (*memory.Store, string) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	d, sock := startDaemon(t, makeFactory(seqProvider(nil)), nil, nil)
	d.ConfigureMemory(store)
	store.OnJournal(d.JournalWritten)
	t.Cleanup(func() { store.OnJournal(nil) })
	return store, sock
}

// nextBatch reads one journal batch, failing the test after a few seconds.
func nextBatch(t *testing.T, c *protocol.Client) []protocol.JournalEvent {
	t.Helper()
	type res struct {
		evs []protocol.JournalEvent
		err error
	}
	ch := make(chan res, 1)
	go func() {
		evs, err := c.NextJournal()
		ch <- res{evs, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("NextJournal: %v", r.err)
		}
		return r.evs
	case <-time.After(5 * time.Second):
		t.Fatal("no journal batch within 5s")
	}
	return nil
}

func TestFollowJournalStreamsNewEvents(t *testing.T) {
	store, sock := followHarness(t)
	if err := store.GoalCreate("g-old", "before", "", ""); err != nil {
		t.Fatal(err)
	}

	// A new follower starts from now: the goal created before is not sent.
	f := dial(t, sock)
	start, reset, err := f.FollowJournal(0, 0)
	if err != nil || reset || start != 1 {
		t.Fatalf("FollowJournal = %d, %v, %v; want 1, false, nil", start, reset, err)
	}
	if err := store.GoalCreate("g-new", "after", "", ""); err != nil {
		t.Fatal(err)
	}
	evs := nextBatch(t, f)
	if len(evs) != 1 || evs[0].Type != memory.EvGoalCreated || evs[0].AgentID != "g-new" || evs[0].Seq != 2 {
		t.Fatalf("batch = %+v, want g-new's goal_created at seq 2", evs)
	}

	// One with a tail gets the backlog first.
	tailed := dial(t, sock)
	if start, _, err := tailed.FollowJournal(0, 10); err != nil || start != 0 {
		t.Fatalf("FollowJournal(tail) start = %d, %v; want 0", start, err)
	}
	if evs := nextBatch(t, tailed); len(evs) != 2 {
		t.Fatalf("tail batch = %+v, want both events", evs)
	}

	// A returning one resumes after its seq.
	back := dial(t, sock)
	if start, reset, err := back.FollowJournal(1, 0); err != nil || reset || start != 1 {
		t.Fatalf("FollowJournal(1) = %d, %v, %v", start, reset, err)
	}
	if evs := nextBatch(t, back); len(evs) != 1 || evs[0].Seq != 2 {
		t.Fatalf("resumed batch = %+v, want seq 2", evs)
	}

	// One from another journal is told to reload.
	if start, reset, err := dial(t, sock).FollowJournal(99, 0); err != nil || !reset || start != 2 {
		t.Fatalf("FollowJournal(99) = %d, %v, %v; want 2, reset", start, reset, err)
	}
}

func TestListHumanRequestsOverTheWire(t *testing.T) {
	store, sock := followHarness(t)
	if err := store.HumanRequestCreate("q1", "conv1", "Ship it?", []string{"yes", "no"}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	qs, err := dial(t, sock).ListHumanRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(qs) != 1 || qs[0].ID != "q1" || qs[0].AgentID != "conv1" || len(qs[0].Options) != 2 {
		t.Fatalf("questions = %+v", qs)
	}
}
