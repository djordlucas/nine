package memory_test

import (
	"testing"

	"nine/internal/memory"
	"nine/internal/memory/memtest"
)

func TestBoardTaskCRUD(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	// Create requires a title.
	if _, err := store.BoardTaskCreate("  ", "spec", "conv1", "", 0); err == nil {
		t.Fatal("create with empty title: want error")
	}

	t1, err := store.BoardTaskCreate("Fix the queue", "investigate INC-1", "conv1", "", 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if t1.Status != memory.BoardQueued || t1.PostedBy != "conv1" || t1.ID == "" {
		t.Fatalf("bad initial task: %+v", t1)
	}
	t2, err := store.BoardTaskCreate("Write the report", "", "conv1", "", 0)
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}

	// List: newest first, status filter.
	all, err := store.BoardTaskList("", 0, 0)
	if err != nil || len(all) != 2 || all[0].ID != t2.ID {
		t.Fatalf("list all: n=%d err=%v first=%s", len(all), err, all[0].ID)
	}
	q, err := store.BoardTaskList(memory.BoardQueued, 0, 0)
	if err != nil || len(q) != 2 {
		t.Fatalf("list queued: n=%d err=%v", len(q), err)
	}
	if _, err := store.BoardTaskList("bogus", 0, 0); err == nil {
		t.Fatal("list bogus status: want error")
	}

	// Get: unknown id is ok=false.
	if _, ok, err := store.BoardTaskGet("task_nope"); err != nil || ok {
		t.Fatalf("get unknown: ok=%v err=%v", ok, err)
	}

	// Claim is atomic: first claimant wins, second is refused.
	c1, ok, err := store.BoardTaskClaim(t1.ID, "worker-a")
	if err != nil || !ok || c1.Status != memory.BoardRunning || c1.ClaimedBy != "worker-a" {
		t.Fatalf("claim: %+v ok=%v err=%v", c1, ok, err)
	}
	if _, ok, err := store.BoardTaskClaim(t1.ID, "worker-b"); err != nil || ok {
		t.Fatalf("double claim: ok=%v err=%v, want refused", ok, err)
	}

	// Only the claimant can finish; wrong claimant refused.
	if _, ok, err := store.BoardTaskFinish(t1.ID, "worker-b", memory.BoardDone, "ok"); err != nil || ok {
		t.Fatalf("finish by non-claimant: ok=%v err=%v, want refused", ok, err)
	}
	fin, ok, err := store.BoardTaskFinish(t1.ID, "worker-a", memory.BoardDone, "all fixed")
	if err != nil || !ok || fin.Status != memory.BoardDone || fin.Result != "all fixed" || fin.FinishedAt == "" {
		t.Fatalf("finish: %+v ok=%v err=%v", fin, ok, err)
	}

	// A done task cannot be claimed or cancelled.
	if _, ok, _ := store.BoardTaskClaim(t1.ID, "worker-c"); ok {
		t.Fatal("claim of done task: want refused")
	}
	if _, ok, _ := store.BoardTaskCancel(t1.ID); ok {
		t.Fatal("cancel of done task: want refused")
	}

	// Cancel a queued task.
	cx, ok, err := store.BoardTaskCancel(t2.ID)
	if err != nil || !ok || cx.Status != memory.BoardCancelled {
		t.Fatalf("cancel: %+v ok=%v err=%v", cx, ok, err)
	}

	// Update: patch fields, validate status, unknown id refused.
	title := "Fix the queue (urgent)"
	upd, ok, err := store.BoardTaskUpdate(t2.ID, memory.BoardTaskPatch{Title: &title})
	if err != nil || !ok || upd.Title != title {
		t.Fatalf("update: %+v ok=%v err=%v", upd, ok, err)
	}
	bad := "bogus"
	if _, _, err := store.BoardTaskUpdate(t2.ID, memory.BoardTaskPatch{Status: &bad}); err == nil {
		t.Fatal("update bogus status: want error")
	}
	if _, ok, _ := store.BoardTaskUpdate("task_nope", memory.BoardTaskPatch{Title: &title}); ok {
		t.Fatal("update unknown: want ok=false")
	}

	// Delete.
	if ok, err := store.BoardTaskDelete(t2.ID); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if ok, _ := store.BoardTaskDelete(t2.ID); ok {
		t.Fatal("double delete: want ok=false")
	}
}

func TestBoardTaskAdmissionHelpers(t *testing.T) {
	store, err := memtest.Open(t)
	if err != nil {
		t.Fatal(err)
	}

	a, err := store.BoardTaskCreate("Same", "same spec", "conv1", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.BoardTaskCreate("Same", "same spec", "conv1", "", 0); err != nil {
		t.Fatal(err) // creating a duplicate row is allowed at store level; the guard is FindAlike
	}

	// Open-count and alike checks see only queued/running rows.
	n, err := store.BoardTaskCountOpen("conv1")
	if err != nil || n != 2 {
		t.Fatalf("count open: n=%d err=%v", n, err)
	}
	if _, ok, err := store.BoardTaskFindAlike("conv1", "Same", "same spec"); err != nil || !ok {
		t.Fatalf("find alike: ok=%v err=%v", ok, err)
	}
	if _, ok, _ := store.BoardTaskFindAlike("conv2", "Same", "same spec"); ok {
		t.Fatal("alike for another poster: want not found")
	}
	if _, ok, _ := store.BoardTaskClaim(a.ID, "w1"); !ok {
		t.Fatal("claim failed")
	}
	if _, ok, _ := store.BoardTaskFinish(a.ID, "w1", memory.BoardFailed, "boom"); !ok {
		t.Fatal("fail failed")
	}
	// A terminal task is no longer "open": count drops, alike no longer matches.
	n, _ = store.BoardTaskCountOpen("conv1")
	if n != 1 {
		t.Fatalf("count open after finish: n=%d, want 1", n)
	}
	if _, ok, _ := store.BoardTaskFindAlike("conv1", "Same", "same spec"); !ok {
		t.Fatal("alike should still match the one open duplicate")
	}
}
