package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

// fakeGenesis answers genesis's turns from a script; on the turn named by
// writeOn it records a running process Nine wrote, as a tool_write would.
type fakeGenesis struct {
	store   *memory.Store
	replies []string
	errs    map[int]error
	writeOn int
	// failOn makes the written process fail, as a broken program would;
	// fixOn makes it run, as a fixed one would.
	failOn, fixOn int
	texts         []string
}

func (f *fakeGenesis) ProcessTurn(_ context.Context, id string, p RoleParams, text string, _ TurnOptions) (string, int, error) {
	f.texts = append(f.texts, text)
	n := len(f.texts)
	if id != GenesisSession || p.Role != GenesisRole || p.Process {
		return "", 0, errors.New("wrong session")
	}
	if err := f.errs[n]; err != nil {
		return "", 0, err
	}
	if n == f.writeOn {
		if _, err := f.store.GeneratedToolWrite(memory.GeneratedTool{Name: "digest", Source: "x", Live: true, Probation: true}, ""); err != nil {
			return "", 0, err
		}
		if err := f.store.ProcessUpsertDefinition(memory.Process{ID: "gen:digest", Tool: "digest",
			Mode: memory.ProcessLive, SessionID: "gen:digest", Generated: true, Owner: true}); err != nil {
			return "", 0, err
		}
		if _, err := f.store.ProcessSetState("gen:digest", memory.ProcessRunning); err != nil {
			return "", 0, err
		}
	}
	switch n {
	case f.failOn:
		if _, err := f.store.ProcessFail("gen:digest", "cannot read /work/notes (TypeError)", time.Now(), memory.ProcessRunning); err != nil {
			return "", 0, err
		}
	case f.fixOn:
		if err := f.store.ProcessRecovered("gen:digest"); err != nil {
			return "", 0, err
		}
		if err := f.store.GeneratedToolProve("digest", 0); err != nil {
			return "", 0, err
		}
	}
	return f.replies[min(n, len(f.replies))-1], 0, nil
}

func genesisDoc(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "genesis.md")
	if err := os.WriteFile(p, []byte("# Wren\n\nKeep a daily digest of the notes folder."), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Genesis gives the document, keeps going while the session is not done,
// survives a failed turn, and ends when it says GENESIS DONE with a process
// running — then never runs again in this store.
func TestGenesisRunsUntilDone(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGenesis{store: store, writeOn: 3, fixOn: 4,
		replies: []string{"Writing the digest process.", "", "Written.", "All written. GENESIS DONE"},
		errs:    map[int]error{2: errors.New("llm: stream cut")}}
	doc := genesisDoc(t)
	ok, err := RunGenesis(context.Background(), f, store, doc, GenesisOptions{ProcessRoles: []string{"process"}, Verify: time.Second, Poll: 10 * time.Millisecond})
	if err != nil || !ok {
		t.Fatalf("genesis = %v, %v", ok, err)
	}
	if len(f.texts) != 4 {
		t.Errorf("%d turns, want 4", len(f.texts))
	}
	if !strings.Contains(f.texts[0], "Keep a daily digest") || !strings.Contains(f.texts[0], "Revision is off") {
		t.Errorf("first turn = %q", f.texts[0])
	}
	if !strings.Contains(f.texts[2], "stream cut") {
		t.Errorf("the turn after a failure = %q", f.texts[2])
	}
	if feed := feedText(t, store); !strings.Contains(feed, "Genesis done") || !strings.Contains(feed, "gen:digest") {
		t.Errorf("feed = %q", feed)
	}

	again := &fakeGenesis{store: store, replies: []string{"GENESIS DONE"}}
	if ok, err := RunGenesis(context.Background(), again, store, doc, GenesisOptions{}); ok || err != nil || len(again.texts) != 0 {
		t.Errorf("a second genesis ran: %v, %v, %d turns", ok, err, len(again.texts))
	}
}

// Saying GENESIS DONE with nothing running is not done; a genesis that does
// not finish within its turns is reported and not marked, so it runs again.
func TestGenesisThatDoesNotFinish(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGenesis{store: store, replies: []string{"GENESIS DONE"}}
	doc := genesisDoc(t)
	ok, err := RunGenesis(context.Background(), f, store, doc, GenesisOptions{Turns: 3, Verify: 50 * time.Millisecond, Poll: 10 * time.Millisecond})
	if ok || err == nil {
		t.Fatalf("genesis = %v, %v; want a failure", ok, err)
	}
	if len(f.texts) != 3 || !strings.Contains(f.texts[1], "no process you wrote is running") {
		t.Errorf("turns = %d, second = %q", len(f.texts), f.texts[1])
	}
	if feed := feedText(t, store); !strings.Contains(feed, "did not finish in 3 turns") {
		t.Errorf("feed = %q", feed)
	}
	if _, done, _ := store.Get(genesisSentinel); done {
		t.Error("an unfinished genesis was marked done")
	}
}

// GENESIS DONE is not accepted while a process genesis wrote fails when it
// runs: its error comes back as the next turn, and genesis ends once the fix
// has run.
func TestGenesisHandsBackWhatFails(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGenesis{store: store, writeOn: 1, failOn: 1, fixOn: 2,
		replies: []string{"Written. GENESIS DONE", "Fixed the path. GENESIS DONE"}}
	ok, err := RunGenesis(context.Background(), f, store, genesisDoc(t), GenesisOptions{Verify: time.Second, Poll: 10 * time.Millisecond})
	if err != nil || !ok {
		t.Fatalf("genesis = %v, %v", ok, err)
	}
	if len(f.texts) != 2 || !strings.Contains(f.texts[1], "gen:digest: cannot read /work/notes") {
		t.Errorf("turns = %d; second = %q", len(f.texts), f.texts[len(f.texts)-1])
	}
}
