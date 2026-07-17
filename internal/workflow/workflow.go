// Package workflow holds the domain logic for named, persistent execution
// plans: creating them, advancing step status, auto-closing when all steps
// reach a terminal state, and operator actions (cancel/fail/scrub/retry).
//
// Persistence is abstracted behind the Repository interface so Service can be
// tested without a database. The concrete SQLite-backed implementation lives
// in internal/memory, which is the sole owner of the shared database.
package workflow

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Step is one step in a workflow's ordered plan.
type Step struct {
	ID            string   `json:"id"`
	Label         string   `json:"label"`
	Status        string   `json:"status"` // pending|running|done|failed|skipped
	AgentID       string   `json:"agent_id,omitempty"`
	Result        string   `json:"result,omitempty"`
	FailureReason string   `json:"failure_reason,omitempty"`
	DependsOn     []string `json:"depends_on,omitempty"`
	UpdatedAt     string   `json:"updated_at"`
}

// Workflow is a named, persistent execution plan with an ordered list of steps.
type Workflow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"` // active|done|failed|cancelled
	AgentID   string `json:"agent_id"`
	Steps     []Step `json:"steps"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// CreateResult is returned from Service.Create.
type CreateResult struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// UpdateResult is returned from Service.Update.
type UpdateResult struct {
	OK             bool   `json:"ok"`
	WorkflowStatus string `json:"workflow_status"`
	Discarded      bool   `json:"discarded,omitempty"`
}

// Repository is the persistence seam Service depends on. The concrete
// implementation (see internal/memory) is backed by the shared SQLite store;
// tests can supply a fake.
type Repository interface {
	// Insert persists a newly created workflow with status "active".
	Insert(id, name, agentID string, steps []Step, createdAt string) error
	// Load returns a workflow by ID, or an error if it doesn't exist.
	Load(id string) (*Workflow, error)
	// Save persists the workflow's steps and derived status.
	Save(id string, steps []Step, status string) error
	// ListActive returns active workflows, newest first. agentID == "" matches all agents.
	ListActive(agentID string) ([]Workflow, error)
	// ListRecent returns the most recently updated non-active workflows (most
	// recent first, capped at limit). agentID == "" matches all agents.
	ListRecent(agentID string, limit int) ([]Workflow, error)
	// Notify records a user-facing notification for conversationID.
	Notify(conversationID, message string) error
}

// Service implements workflow domain logic against a Repository.
type Service struct {
	repo Repository
}

// NewService returns a Service backed by repo.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

func isTerminal(status string) bool {
	return status == "done" || status == "failed" || status == "skipped"
}

// newID generates a random UUID v4.
func newID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// terminalStatus derives the workflow-level status implied by its steps:
// "failed" if any step failed, "done" if all terminal and none failed, or the
// workflow's current status if any step is still pending/running.
func terminalStatus(current string, steps []Step) string {
	allTerminal := true
	anyFailed := false
	for _, st := range steps {
		if !isTerminal(st.Status) {
			allTerminal = false
			break
		}
		if st.Status == "failed" {
			anyFailed = true
		}
	}
	if !allTerminal {
		return current
	}
	if anyFailed {
		return "failed"
	}
	return "done"
}

// Create starts a new workflow with the given ordered step labels, all initially pending.
func (s *Service) Create(agentID, name string, stepLabels []string) (*CreateResult, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}
	id := newID()
	ts := now()
	steps := make([]Step, len(stepLabels))
	for i, label := range stepLabels {
		steps[i] = Step{
			ID:        fmt.Sprintf("s%d", i),
			Label:     label,
			Status:    "pending",
			DependsOn: []string{},
			UpdatedAt: ts,
		}
	}
	if err := s.repo.Insert(id, name, agentID, steps, ts); err != nil {
		return nil, err
	}
	return &CreateResult{ID: id, Steps: steps}, nil
}

// Get returns a workflow by ID.
func (s *Service) Get(id string) (*Workflow, error) {
	return s.repo.Load(id)
}

// Update sets a step's status (and optionally its result/failure reason),
// resolving dependency gates and auto-closing the workflow once every step
// reaches a terminal state. A terminal step status posts a user-facing
// notification to the workflow's owning conversation.
func (s *Service) Update(workflowID, stepID, status, result, failureReason string) (*UpdateResult, error) {
	w, err := s.repo.Load(workflowID)
	if err != nil {
		return nil, err
	}

	if w.Status == "cancelled" {
		return &UpdateResult{OK: true, WorkflowStatus: w.Status, Discarded: true}, nil
	}

	stepIdx := -1
	for i, step := range w.Steps {
		if step.ID == stepID {
			stepIdx = i
			break
		}
	}
	if stepIdx < 0 {
		return nil, fmt.Errorf("step %s not found in workflow %s", stepID, workflowID)
	}
	step := &w.Steps[stepIdx]

	newStatus := status
	if newStatus == "running" && len(step.DependsOn) > 0 {
		for _, depID := range step.DependsOn {
			for _, dep := range w.Steps {
				if dep.ID != depID {
					continue
				}
				switch dep.Status {
				case "failed", "skipped":
					newStatus = "skipped"
					failureReason = "dependency " + depID + " did not complete successfully"
				case "pending", "running":
					return nil, fmt.Errorf("dependency %s is still %s; cannot start step %s yet", depID, dep.Status, stepID)
				}
			}
		}
	}

	ts := now()
	step.Status = newStatus
	if result != "" {
		step.Result = result
	}
	if failureReason != "" {
		step.FailureReason = failureReason
	}
	step.UpdatedAt = ts

	wfStatus := terminalStatus(w.Status, w.Steps)
	if err := s.repo.Save(workflowID, w.Steps, wfStatus); err != nil {
		return nil, err
	}

	if isTerminal(newStatus) {
		msg := fmt.Sprintf("[workflow step %s] %s", newStatus, step.Label)
		if step.Result != "" {
			msg += " → " + step.Result
		}
		s.repo.Notify(w.AgentID, msg) //nolint:errcheck
	}

	return &UpdateResult{OK: true, WorkflowStatus: wfStatus}, nil
}

// List returns active workflows and the 10 most recently completed ones.
// Pass "" for agentID to list across all agents.
func (s *Service) List(agentID string) ([]Workflow, error) {
	active, err := s.repo.ListActive(agentID)
	if err != nil {
		return nil, err
	}
	recent, err := s.repo.ListRecent(agentID, 10)
	if err != nil {
		return nil, err
	}
	all := append(active, recent...)
	if all == nil {
		all = []Workflow{}
	}
	return all, nil
}

// Scrub marks any "running" step as "failed" ("interrupted") across all active
// workflows — recovery for ungraceful shutdowns where no process will ever
// update those steps. Returns the number of workflows modified.
func (s *Service) Scrub() (int, error) {
	workflows, err := s.repo.ListActive("")
	if err != nil {
		return 0, err
	}

	ts := now()
	scrubbed := 0
	for _, w := range workflows {
		changed := false
		for i, step := range w.Steps {
			if step.Status == "running" {
				w.Steps[i].Status = "failed"
				w.Steps[i].FailureReason = "interrupted"
				w.Steps[i].UpdatedAt = ts
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := s.repo.Save(w.ID, w.Steps, terminalStatus("active", w.Steps)); err != nil {
			return scrubbed, err
		}
		scrubbed++
	}
	return scrubbed, nil
}

// Fail marks all pending/running steps as failed ("cancelled") and closes the
// workflow as failed. Pass id="" with all=true to fail every active workflow.
// Returns the number of workflows modified.
func (s *Service) Fail(id string, all bool) (int, error) {
	var workflows []Workflow
	if all {
		var err error
		workflows, err = s.repo.ListActive("")
		if err != nil {
			return 0, err
		}
	} else {
		if id == "" {
			return 0, fmt.Errorf("id or all is required")
		}
		w, err := s.repo.Load(id)
		if err != nil {
			return 0, err
		}
		workflows = []Workflow{*w}
	}

	ts := now()
	updated := 0
	for _, w := range workflows {
		for i, step := range w.Steps {
			if step.Status == "running" || step.Status == "pending" {
				w.Steps[i].Status = "failed"
				w.Steps[i].FailureReason = "cancelled"
				w.Steps[i].UpdatedAt = ts
			}
		}
		if err := s.repo.Save(w.ID, w.Steps, "failed"); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

// Cancel cancels an ongoing workflow: pending steps are skipped, running steps
// fail with reason "stopped". Returns the number of steps changed.
func (s *Service) Cancel(id string) (int, error) {
	if id == "" {
		return 0, fmt.Errorf("id is required")
	}
	w, err := s.repo.Load(id)
	if err != nil {
		return 0, err
	}

	ts := now()
	updated := 0
	for i, step := range w.Steps {
		switch step.Status {
		case "pending":
			w.Steps[i].Status = "skipped"
			w.Steps[i].UpdatedAt = ts
			updated++
		case "running":
			w.Steps[i].Status = "failed"
			w.Steps[i].FailureReason = "stopped"
			w.Steps[i].UpdatedAt = ts
			updated++
		}
	}
	if err := s.repo.Save(id, w.Steps, "cancelled"); err != nil {
		return updated, err
	}
	return updated, nil
}

// ResetStep resets a step back to pending, clearing its result and failure
// reason, and re-opens the workflow if it had already closed.
func (s *Service) ResetStep(workflowID, stepID string) error {
	w, err := s.repo.Load(workflowID)
	if err != nil {
		return err
	}

	ts := now()
	found := false
	for i, step := range w.Steps {
		if step.ID == stepID {
			w.Steps[i].Status = "pending"
			w.Steps[i].Result = ""
			w.Steps[i].FailureReason = ""
			w.Steps[i].UpdatedAt = ts
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("step %s not found in workflow %s", stepID, workflowID)
	}

	wfStatus := w.Status
	if wfStatus == "done" || wfStatus == "failed" {
		wfStatus = "active"
	}
	return s.repo.Save(workflowID, w.Steps, wfStatus)
}
