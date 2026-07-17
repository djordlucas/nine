package workflow

import (
	"fmt"
	"testing"
)

// fakeRepo is an in-memory Repository for testing Service without a database.
type fakeRepo struct {
	workflows map[string]*Workflow
	notifs    []notif

	// optional error injection
	loadErr  error
	saveErr  error
	insertErr error
}

type notif struct {
	conversationID string
	message        string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{workflows: map[string]*Workflow{}}
}

func (f *fakeRepo) Insert(id, name, agentID string, steps []Step, createdAt string) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	f.workflows[id] = &Workflow{
		ID:        id,
		Name:      name,
		Status:    "active",
		AgentID:   agentID,
		Steps:     append([]Step{}, steps...),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}
	return nil
}

func (f *fakeRepo) Load(id string) (*Workflow, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	w, ok := f.workflows[id]
	if !ok {
		return nil, fmt.Errorf("workflow %s not found", id)
	}
	cp := *w
	cp.Steps = append([]Step{}, w.Steps...)
	return &cp, nil
}

func (f *fakeRepo) Save(id string, steps []Step, status string) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	w, ok := f.workflows[id]
	if !ok {
		return fmt.Errorf("workflow %s not found", id)
	}
	w.Steps = append([]Step{}, steps...)
	w.Status = status
	return nil
}

func (f *fakeRepo) ListActive(agentID string) ([]Workflow, error) {
	var out []Workflow
	for _, w := range f.workflows {
		if w.Status != "active" {
			continue
		}
		if agentID != "" && w.AgentID != agentID {
			continue
		}
		out = append(out, *w)
	}
	return out, nil
}

func (f *fakeRepo) ListRecent(agentID string, limit int) ([]Workflow, error) {
	var out []Workflow
	for _, w := range f.workflows {
		if w.Status == "active" {
			continue
		}
		if agentID != "" && w.AgentID != agentID {
			continue
		}
		out = append(out, *w)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeRepo) Notify(conversationID, message string) error {
	f.notifs = append(f.notifs, notif{conversationID, message})
	return nil
}

// --- Create ---

func TestServiceCreate(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test", "ship"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.ID == "" {
		t.Error("expected non-empty ID")
	}
	if len(res.Steps) != 3 {
		t.Fatalf("got %d steps, want 3", len(res.Steps))
	}
	for i, label := range []string{"build", "test", "ship"} {
		if res.Steps[i].Label != label {
			t.Errorf("step %d label = %q, want %q", i, res.Steps[i].Label, label)
		}
		if res.Steps[i].Status != "pending" {
			t.Errorf("step %d status = %q, want pending", i, res.Steps[i].Status)
		}
	}

	w, ok := repo.workflows[res.ID]
	if !ok {
		t.Fatal("workflow not persisted via Insert")
	}
	if w.AgentID != "agent-1" || w.Status != "active" {
		t.Errorf("persisted workflow = %+v, want agent-1/active", w)
	}
}

func TestServiceCreateRequiresName(t *testing.T) {
	svc := NewService(newFakeRepo())
	if _, err := svc.Create("agent-1", "", []string{"a"}); err == nil {
		t.Error("expected error for empty name, got nil")
	}
}

// --- Get ---

func TestServiceGet(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}

	w, err := svc.Get(res.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w.Name != "deploy" {
		t.Errorf("Name = %q, want deploy", w.Name)
	}
}

func TestServiceGetNotFound(t *testing.T) {
	svc := NewService(newFakeRepo())
	if _, err := svc.Get("missing"); err == nil {
		t.Error("expected error for missing workflow, got nil")
	}
}

// --- Update ---

func TestServiceUpdateAdvancesStep(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}

	upd, err := svc.Update(res.ID, "s0", "running", "", "")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !upd.OK || upd.WorkflowStatus != "active" {
		t.Errorf("Update result = %+v, want OK/active", upd)
	}

	w, _ := svc.Get(res.ID)
	if w.Steps[0].Status != "running" {
		t.Errorf("step 0 status = %q, want running", w.Steps[0].Status)
	}
}

func TestServiceUpdateAutoCloses(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(res.ID, "s0", "running", "", ""); err != nil {
		t.Fatal(err)
	}
	upd, err := svc.Update(res.ID, "s0", "done", "build ok", "")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.WorkflowStatus != "done" {
		t.Errorf("WorkflowStatus = %q, want done", upd.WorkflowStatus)
	}

	w, _ := svc.Get(res.ID)
	if w.Status != "done" {
		t.Errorf("workflow status = %q, want done", w.Status)
	}
}

func TestServiceUpdateAutoClosesFailedWhenAnyStepFails(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(res.ID, "s0", "done", "", ""); err != nil {
		t.Fatal(err)
	}
	upd, err := svc.Update(res.ID, "s1", "failed", "", "boom")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.WorkflowStatus != "failed" {
		t.Errorf("WorkflowStatus = %q, want failed", upd.WorkflowStatus)
	}
}

func TestServiceUpdateUnknownStep(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "no-such-step", "done", "", ""); err == nil {
		t.Error("expected error for unknown step, got nil")
	}
}

func TestServiceUpdateCancelledWorkflowDiscardsUpdate(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cancel(res.ID); err != nil {
		t.Fatal(err)
	}

	upd, err := svc.Update(res.ID, "s0", "done", "", "")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !upd.Discarded || upd.WorkflowStatus != "cancelled" {
		t.Errorf("Update result = %+v, want discarded/cancelled", upd)
	}
}

func TestServiceUpdateNotifiesOnTerminalStep(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "s0", "done", "all good", ""); err != nil {
		t.Fatal(err)
	}

	if len(repo.notifs) != 1 {
		t.Fatalf("got %d notifications, want 1", len(repo.notifs))
	}
	if repo.notifs[0].conversationID != "agent-1" {
		t.Errorf("notif conversationID = %q, want agent-1", repo.notifs[0].conversationID)
	}
}

func TestServiceUpdateNoNotificationForNonTerminalStatus(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "s0", "running", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(repo.notifs) != 0 {
		t.Errorf("got %d notifications, want 0 for non-terminal status", len(repo.notifs))
	}
}

// --- Update: dependency gating ---

func TestServiceUpdateDependencyBlocksStart(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := svc.Get(res.ID)
	w.Steps[1].DependsOn = []string{"s0"}
	if err := repo.Save(res.ID, w.Steps, w.Status); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(res.ID, "s1", "running", "", ""); err == nil {
		t.Error("expected error starting step whose dependency is still pending")
	}
}

func TestServiceUpdateDependencySkippedWhenDependencyFailed(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := svc.Get(res.ID)
	w.Steps[1].DependsOn = []string{"s0"}
	if err := repo.Save(res.ID, w.Steps, w.Status); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(res.ID, "s0", "failed", "", "broke"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "s1", "running", "", ""); err != nil {
		t.Fatalf("Update: %v", err)
	}

	w, _ = svc.Get(res.ID)
	if w.Steps[1].Status != "skipped" {
		t.Errorf("dependent step status = %q, want skipped", w.Steps[1].Status)
	}
	if w.Steps[1].FailureReason == "" {
		t.Error("expected a failure reason explaining the skip")
	}
}

func TestServiceUpdateDependencyAllowsStartWhenSatisfied(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := svc.Get(res.ID)
	w.Steps[1].DependsOn = []string{"s0"}
	if err := repo.Save(res.ID, w.Steps, w.Status); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Update(res.ID, "s0", "done", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "s1", "running", "", ""); err != nil {
		t.Fatalf("Update: %v", err)
	}

	w, _ = svc.Get(res.ID)
	if w.Steps[1].Status != "running" {
		t.Errorf("dependent step status = %q, want running", w.Steps[1].Status)
	}
}

// --- List ---

func TestServiceList(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	active, err := svc.Create("agent-1", "active-one", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	done, err := svc.Create("agent-1", "done-one", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(done.ID, "s0", "done", "", ""); err != nil {
		t.Fatal(err)
	}

	all, err := svc.List("agent-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d workflows, want 2", len(all))
	}
	var sawActive, sawDone bool
	for _, w := range all {
		if w.ID == active.ID {
			sawActive = true
		}
		if w.ID == done.ID {
			sawDone = true
		}
	}
	if !sawActive || !sawDone {
		t.Errorf("List = %+v, want both active and done workflows", all)
	}
}

func TestServiceListEmptyReturnsEmptySlice(t *testing.T) {
	svc := NewService(newFakeRepo())
	all, err := svc.List("agent-1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if all == nil {
		t.Error("List returned nil, want empty non-nil slice")
	}
	if len(all) != 0 {
		t.Errorf("got %d workflows, want 0", len(all))
	}
}

// --- Scrub ---

func TestServiceScrub(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "s0", "running", "", ""); err != nil {
		t.Fatal(err)
	}

	n, err := svc.Scrub()
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if n != 1 {
		t.Errorf("Scrub returned %d, want 1", n)
	}

	w, _ := svc.Get(res.ID)
	if w.Steps[0].Status != "failed" || w.Steps[0].FailureReason != "interrupted" {
		t.Errorf("step 0 = %+v, want failed/interrupted", w.Steps[0])
	}
}

func TestServiceScrubNoRunningSteps(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	if _, err := svc.Create("agent-1", "deploy", []string{"build"}); err != nil {
		t.Fatal(err)
	}

	n, err := svc.Scrub()
	if err != nil {
		t.Fatalf("Scrub: %v", err)
	}
	if n != 0 {
		t.Errorf("Scrub returned %d, want 0 when nothing is running", n)
	}
}

// --- Fail ---

func TestServiceFailByID(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test"})
	if err != nil {
		t.Fatal(err)
	}

	n, err := svc.Fail(res.ID, false)
	if err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if n != 1 {
		t.Errorf("Fail returned %d, want 1", n)
	}

	w, _ := svc.Get(res.ID)
	if w.Status != "failed" {
		t.Errorf("workflow status = %q, want failed", w.Status)
	}
	for _, st := range w.Steps {
		if st.Status != "failed" || st.FailureReason != "cancelled" {
			t.Errorf("step = %+v, want failed/cancelled", st)
		}
	}
}

func TestServiceFailAll(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	a, err := svc.Create("agent-1", "a", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.Create("agent-1", "b", []string{"x"})
	if err != nil {
		t.Fatal(err)
	}

	n, err := svc.Fail("", true)
	if err != nil {
		t.Fatalf("Fail(all): %v", err)
	}
	if n != 2 {
		t.Errorf("Fail(all) returned %d, want 2", n)
	}
	for _, id := range []string{a.ID, b.ID} {
		w, _ := svc.Get(id)
		if w.Status != "failed" {
			t.Errorf("workflow %s status = %q, want failed", id, w.Status)
		}
	}
}

func TestServiceFailRequiresIDOrAll(t *testing.T) {
	svc := NewService(newFakeRepo())
	if _, err := svc.Fail("", false); err == nil {
		t.Error("expected error when neither id nor all is set")
	}
}

// --- Cancel ---

func TestServiceCancel(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build", "test", "ship"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "s0", "running", "", ""); err != nil {
		t.Fatal(err)
	}

	n, err := svc.Cancel(res.ID)
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if n != 3 {
		t.Errorf("Cancel changed %d steps, want 3", n)
	}

	w, _ := svc.Get(res.ID)
	if w.Status != "cancelled" {
		t.Errorf("workflow status = %q, want cancelled", w.Status)
	}
	if w.Steps[0].Status != "failed" || w.Steps[0].FailureReason != "stopped" {
		t.Errorf("running step = %+v, want failed/stopped", w.Steps[0])
	}
	if w.Steps[1].Status != "skipped" || w.Steps[2].Status != "skipped" {
		t.Errorf("pending steps = %+v / %+v, want skipped", w.Steps[1], w.Steps[2])
	}
}

func TestServiceCancelRequiresID(t *testing.T) {
	svc := NewService(newFakeRepo())
	if _, err := svc.Cancel(""); err == nil {
		t.Error("expected error for empty id")
	}
}

// --- ResetStep ---

func TestServiceResetStep(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Update(res.ID, "s0", "failed", "", "boom"); err != nil {
		t.Fatal(err)
	}

	w, _ := svc.Get(res.ID)
	if w.Status != "failed" {
		t.Fatalf("precondition: workflow status = %q, want failed", w.Status)
	}

	if err := svc.ResetStep(res.ID, "s0"); err != nil {
		t.Fatalf("ResetStep: %v", err)
	}

	w, _ = svc.Get(res.ID)
	if w.Status != "active" {
		t.Errorf("workflow status = %q, want active after reset", w.Status)
	}
	st := w.Steps[0]
	if st.Status != "pending" || st.Result != "" || st.FailureReason != "" {
		t.Errorf("reset step = %+v, want pending/cleared", st)
	}
}

func TestServiceResetStepUnknownStep(t *testing.T) {
	repo := newFakeRepo()
	svc := NewService(repo)

	res, err := svc.Create("agent-1", "deploy", []string{"build"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ResetStep(res.ID, "no-such-step"); err == nil {
		t.Error("expected error resetting unknown step")
	}
}

// --- error propagation ---

func TestServiceGetPropagatesRepoError(t *testing.T) {
	repo := newFakeRepo()
	repo.loadErr = fmt.Errorf("db unavailable")
	svc := NewService(repo)

	if _, err := svc.Get("anything"); err == nil {
		t.Error("expected error to propagate from repository")
	}
}
