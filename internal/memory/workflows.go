package memory

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"nine/internal/workflow"
)

// NewID generates a random UUID v4.
func NewID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// sqlWorkflowRepo is the SQL-backed workflow.Repository implementation.
// internal/memory remains the sole owner of the database handle; workflow.Service
// holds no database reference and depends only on this narrow interface.
type sqlWorkflowRepo struct {
	db db
}

func (r *sqlWorkflowRepo) Insert(id, name, agentID string, steps []workflow.Step, createdAt string) error {
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	_, err = r.db.Exec(
		`INSERT INTO workflows (id, name, status, agent_id, steps, created_at, updated_at)
		 VALUES (?, ?, 'active', ?, ?, ?, ?)`,
		id, name, agentID, string(stepsJSON), createdAt, createdAt)
	return err
}

func (r *sqlWorkflowRepo) Load(id string) (*workflow.Workflow, error) {
	var w workflow.Workflow
	var stepsJSON string
	err := r.db.QueryRow(
		`SELECT id, name, status, agent_id, steps, created_at, updated_at
		 FROM workflows WHERE id = ?`, id).
		Scan(&w.ID, &w.Name, &w.Status, &w.AgentID, &stepsJSON, &w.CreatedAt, &w.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("workflow not found: %s", id)
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(stepsJSON), &w.Steps); err != nil {
		return nil, fmt.Errorf("parse steps: %w", err)
	}
	return &w, nil
}

func (r *sqlWorkflowRepo) Save(id string, steps []workflow.Step, status string) error {
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = r.db.Exec(
		`UPDATE workflows SET steps = ?, status = ?, updated_at = ? WHERE id = ?`,
		string(stepsJSON), status, now, id)
	return err
}

func (r *sqlWorkflowRepo) scan(rows *sql.Rows) ([]workflow.Workflow, error) {
	var out []workflow.Workflow
	for rows.Next() {
		var w workflow.Workflow
		var stepsJSON string
		if err := rows.Scan(&w.ID, &w.Name, &w.Status, &w.AgentID, &stepsJSON, &w.CreatedAt, &w.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(stepsJSON), &w.Steps); err != nil {
			w.Steps = []workflow.Step{}
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func (r *sqlWorkflowRepo) ListActive(agentID string) ([]workflow.Workflow, error) {
	const sel = `SELECT id, name, status, agent_id, steps, created_at, updated_at FROM workflows`
	var rows *sql.Rows
	var err error
	if agentID == "" {
		rows, err = r.db.Query(sel + ` WHERE status = 'active' ORDER BY created_at DESC`)
	} else {
		rows, err = r.db.Query(sel+` WHERE agent_id = ? AND status = 'active' ORDER BY created_at DESC`, agentID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return r.scan(rows)
}

func (r *sqlWorkflowRepo) ListRecent(agentID string, limit int) ([]workflow.Workflow, error) {
	const sel = `SELECT id, name, status, agent_id, steps, created_at, updated_at FROM workflows`
	var rows *sql.Rows
	var err error
	if agentID == "" {
		rows, err = r.db.Query(sel+` WHERE status != 'active' ORDER BY updated_at DESC LIMIT ?`, limit)
	} else {
		rows, err = r.db.Query(sel+` WHERE agent_id = ? AND status != 'active' ORDER BY updated_at DESC LIMIT ?`, agentID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return r.scan(rows)
}

func (r *sqlWorkflowRepo) Notify(conversationID, message string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := r.db.Exec(
		`INSERT INTO notifications (id, conversation_id, message, requires_approval, delivered, created_at)
		 VALUES (?, ?, ?, 0, 0, ?)`,
		NewID(), conversationID, message, now)
	return err
}

// WorkflowCreate creates a new workflow and returns its id and initial steps.
func (s *Store) WorkflowCreate(agentID, name string, stepLabels []string) (*workflow.CreateResult, error) {
	res, err := s.workflows.Create(agentID, name, stepLabels)
	if err == nil && res != nil {
		s.journalState(agentID, EvWorkflowCreated, map[string]any{
			"id": res.ID, "name": name, "steps": len(res.Steps),
		})
	}
	return res, err
}

// WorkflowGet returns a workflow by id.
func (s *Store) WorkflowGet(id string) (*workflow.Workflow, error) {
	return s.workflows.Get(id)
}

// WorkflowUpdate updates a step's status and auto-closes the workflow when all
// steps reach a terminal state.
func (s *Store) WorkflowUpdate(workflowID, stepID, status, result, failureReason string) (*workflow.UpdateResult, error) {
	res, err := s.workflows.Update(workflowID, stepID, status, result, failureReason)
	if err == nil && res != nil && res.OK {
		s.journalState(s.workflowAgent(workflowID), EvWorkflowStep, map[string]any{
			"id": workflowID, "step_id": stepID, "status": status,
			"workflow_status": res.WorkflowStatus,
		})
	}
	return res, err
}

// workflowAgent returns the session a workflow belongs to, for attributing its
// journal events; "" (the daemon) when it cannot be read.
func (s *Store) workflowAgent(id string) string {
	wf, err := s.workflows.Get(id)
	if err != nil || wf == nil {
		return ""
	}
	return wf.AgentID
}

// WorkflowList returns active workflows and the 10 most recently completed.
// Pass "" for agentID to list across all agents.
func (s *Store) WorkflowList(agentID string) ([]workflow.Workflow, error) {
	return s.workflows.List(agentID)
}

// WorkflowScrub marks any running steps as failed ("interrupted") on startup.
// Returns the number of workflows that were modified.
func (s *Store) WorkflowScrub() (int, error) {
	return s.workflows.Scrub()
}

// WorkflowFail marks all pending/running steps as failed and closes the workflow.
// Pass id="" with all=true to fail all active workflows.
func (s *Store) WorkflowFail(id string, all bool) (int, error) {
	n, err := s.workflows.Fail(id, all)
	if err == nil && n > 0 {
		agent := ""
		if id != "" {
			agent = s.workflowAgent(id)
		}
		s.journalState(agent, EvWorkflowFailed, map[string]any{"id": id, "all": all, "count": n})
	}
	return n, err
}

// WorkflowCancel cancels a workflow: pending steps are skipped, running steps fail.
func (s *Store) WorkflowCancel(id string) (int, error) {
	n, err := s.workflows.Cancel(id)
	if err == nil && n > 0 {
		s.journalState(s.workflowAgent(id), EvWorkflowCancelled, map[string]any{"id": id})
	}
	return n, err
}

// WorkflowResetStep resets a step back to pending and re-opens the workflow if it was closed.
func (s *Store) WorkflowResetStep(workflowID, stepID string) error {
	return s.workflows.ResetStep(workflowID, stepID)
}
