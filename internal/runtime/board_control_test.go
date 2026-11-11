package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func testBoardControl(t *testing.T, maxQueued int) (boardControl, *memory.Store) {
	t.Helper()
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	return boardControl{store: store, maxDepth: 2, maxQueued: maxQueued}, store
}

func TestBoardControlLifecycle(t *testing.T) {
	bc, store := testBoardControl(t, 10)

	t1, err := bc.Post("conv1", "Fix the queue", "investigate INC-1")
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if t1.Status != memory.BoardQueued || t1.PostedBy != "conv1" {
		t.Fatalf("bad post: %+v", t1)
	}

	// Journal saw the post.
	evs, err := store.SessionEventsByAgent("conv1")
	if err != nil || len(evs) != 1 || evs[0].Type != EvTaskPosted {
		t.Fatalf("journal after post: %+v err=%v", evs, err)
	}

	// Claim is atomic and journaled.
	c, err := bc.Claim("worker-a", t1.ID)
	if err != nil || c.ClaimedBy != "worker-a" {
		t.Fatalf("claim: %+v err=%v", c, err)
	}
	if _, err := bc.Claim("worker-b", t1.ID); err == nil || !strings.Contains(err.Error(), "not queued") {
		t.Fatalf("double claim: err=%v", err)
	}

	// Only the claimant finishes.
	if _, err := bc.Complete("worker-b", t1.ID, "x"); err == nil {
		t.Fatal("complete by non-claimant: want refusal")
	}
	done, err := bc.Complete("worker-a", t1.ID, "all fixed")
	if err != nil || done.Status != memory.BoardDone || done.Result != "all fixed" {
		t.Fatalf("complete: %+v err=%v", done, err)
	}
	evs, _ = store.SessionEventsByAgent("worker-a")
	types := map[string]bool{}
	for _, e := range evs {
		types[e.Type] = true
	}
	if !types[EvTaskClaimed] || !types[EvTaskDone] {
		t.Fatalf("journal types after claim+done: %v", types)
	}

	// List and get.
	all, err := bc.List("conv1", "", 0, 0)
	if err != nil || len(all) != 1 || all[0].Status != memory.BoardDone {
		t.Fatalf("list: %+v err=%v", all, err)
	}
	if _, err := bc.Get("conv1", "task_nope"); err == nil {
		t.Fatal("get unknown: want error")
	}

	// Cancel by the poster; a stranger cannot.
	t2, _ := bc.Post("conv1", "Second", "spec")
	if _, err := bc.Cancel("worker-a", t2.ID); err == nil {
		t.Fatal("cancel by non-poster: want refusal")
	}
	cx, err := bc.Cancel("conv1", t2.ID)
	if err != nil || cx.Status != memory.BoardCancelled {
		t.Fatalf("cancel: %+v err=%v", cx, err)
	}
	// Terminal task cannot be cancelled again.
	if _, err := bc.Cancel("conv1", t2.ID); err == nil {
		t.Fatal("re-cancel: want refusal")
	}
}

func TestBoardControlGuards(t *testing.T) {
	bc, _ := testBoardControl(t, 2)

	// Queue cap: two open tasks allowed, third refused.
	if _, err := bc.Post("conv1", "one", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Post("conv1", "two", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Post("conv1", "three", ""); err == nil || !strings.Contains(err.Error(), "max_queued") {
		t.Fatalf("third post: err=%v, want max_queued refusal", err)
	}

	// Duplicate: same poster, same title+spec, while open.
	if _, err := bc.Post("conv1", "one", ""); err == nil || !strings.Contains(err.Error(), "already open") {
		t.Fatalf("duplicate post: err=%v, want alike refusal", err)
	}
	// A different poster may post the same text.
	if _, err := bc.Post("conv2", "one", ""); err != nil {
		t.Fatalf("other poster: err=%v", err)
	}

	// Empty title refused (store-level).
	if _, err := bc.Post("conv1", "  ", ""); err == nil {
		t.Fatal("empty title: want error")
	}
}

func TestBoardDeliveryOnFinish(t *testing.T) {
	bc, store := testBoardControl(t, 10)
	var woke []string
	bc.wake = func(agentID, text string, depth int) bool {
		woke = append(woke, agentID+"|"+text)
		return true
	}
	var fed []string
	bc.feed = func(text string) { fed = append(fed, text) }

	// A posted task wakes the poster on completion, framed as data.
	pt, err := bc.Post("orch", "Research X", "spec")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Claim("worker-1", pt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Complete("worker-1", pt.ID, "findings here"); err != nil {
		t.Fatal(err)
	}
	if len(woke) != 1 || !strings.Contains(woke[0], "orch|") || !strings.Contains(woke[0], "not an instruction") {
		t.Fatalf("posted task should wake the poster with the pipe label: %v", woke)
	}

	// A waited run_agent task does not wake (its result is the return value).
	rt, err := bc.RecordRun("orch", "inline task", "spec", memory.BoardSourceRunAgent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Claim("sub-1", rt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Complete("sub-1", rt.ID, "inline result"); err != nil {
		t.Fatal(err)
	}
	if len(woke) != 1 {
		t.Fatalf("waited run must not wake the poster: %v", woke)
	}

	// A detached run delivers; a gone poster gets the feed.
	dt, err := bc.RecordRun("orch", "async task", "spec", memory.BoardSourceDetached)
	if err != nil {
		t.Fatal(err)
	}
	bc.wake = func(string, string, int) bool { return false } // poster gone/busy
	if _, err := bc.Claim("sub-2", dt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Complete("sub-2", dt.ID, "async result"); err != nil {
		t.Fatal(err)
	}
	if len(fed) != 1 || !strings.Contains(fed[0], dt.ID) {
		t.Fatalf("gone poster should get the human feed: %v", fed)
	}

	// The records are on the board with their sources.
	all, err := store.BoardTaskList("", 0, 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("board list: n=%d err=%v", len(all), err)
	}
}

// TestBoardworkerProcessCycle drives a boardworker process's clock tick:
// Next claims the oldest queued task, Turn runs it, and the outcome finishes
// the task on the board with delivery to the poster.
func TestBoardworkerProcessCycle(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}
	r := NewStandingRunner(store, 1, 2)
	sessions := &fakeSessions{}
	r.SetSessions(sessions)
	bc := boardControl{store: store, maxDepth: 2, maxQueued: 100}
	var woke []string
	bc.wake = func(id, text string, depth int) bool { woke = append(woke, id); return true }
	r.SetBoard(&bc)

	// A posted task waiting for a worker.
	poster := "orch-conv"
	task, err := bc.Post(poster, "triage incidents", "read notes/incident.md")
	if err != nil {
		t.Fatal(err)
	}

	// The boardworker process and its handler.
	row := memory.Process{ID: "board-1", Tool: "boardworker", Mode: memory.ProcessLive, SessionID: "board-1", Owner: true, State: memory.ProcessRunning}
	lp := &liveProc{row: row, triggers: make(chan Trigger, 8)}
	h := &liveHandler{r: r, lp: lp}

	// A clock tick claims the task.
	lp.triggers <- Trigger{Kind: "clock"}
	ctx := context.Background()
	trig, err := h.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if trig.BoardTask != task.ID || !strings.Contains(trig.Text, "triage incidents") {
		t.Fatalf("trigger after claim: %+v", trig)
	}
	got, ok, _ := store.BoardTaskGet(task.ID)
	if !ok || got.Status != memory.BoardRunning || got.ClaimedBy != "board-1" {
		t.Fatalf("claimed task: %+v", got)
	}

	// The turn completes the task and the poster is woken.
	if _, err := h.Turn(ctx, trig.Text); err != nil {
		t.Fatal(err)
	}
	done, ok, _ := store.BoardTaskGet(task.ID)
	if !ok || done.Status != memory.BoardDone || !strings.Contains(done.Result, "reply:") {
		t.Fatalf("finished task: %+v", done)
	}
	if len(woke) != 1 || woke[0] != poster {
		t.Fatalf("poster woken: %v", woke)
	}

	// With nothing queued, the tick is skipped (Next blocks) — probe with a
	// cancellable context and confirm no trigger comes back.
	lp.triggers <- Trigger{Kind: "clock"}
	probeCtx, cancelProbe := context.WithCancel(ctx)
	defer cancelProbe()
	done2 := make(chan Trigger, 1)
	go func() {
		t2, err := h.Next(probeCtx)
		if err == nil {
			done2 <- t2
		}
	}()
	select {
	case t2 := <-done2:
		t.Fatalf("empty queue should skip the tick, got trigger %+v", t2)
	case <-time.After(200 * time.Millisecond):
	}
	cancelProbe()

	// A failing turn fails the task.
	task2, _ := bc.Post(poster, "will fail", "spec")
	sessions.fail = errors.New("boom")
	lp.triggers <- Trigger{Kind: "clock"}
	trig2, err := h.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if trig2.BoardTask != task2.ID {
		t.Fatalf("second claim: %+v", trig2)
	}
	_, _ = h.Turn(ctx, trig2.Text)
	failed, _, _ := store.BoardTaskGet(task2.ID)
	if failed.Status != memory.BoardFailed || !strings.Contains(failed.Result, "boom") {
		t.Fatalf("failed task: %+v", failed)
	}
}
