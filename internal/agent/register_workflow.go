package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/llm"
	"nine/internal/memory"
)

var workflowToolDefs = []llm.ToolDef{
	{
		Name:        "workflow_create",
		DisplayName: "Create Workflow",
		Description: "Create a named workflow with an ordered list of step labels. Use this before delegating multi-step work to sub-agents. Returns a workflow_id and step IDs to reference in subsequent workflow_update calls.",
		InputSchema: json.RawMessage(`{"type":"object","required":["name","steps"],"properties":{"name":{"type":"string","description":"Short human-readable workflow name."},"steps":{"type":"array","items":{"type":"string"},"description":"Ordered list of step labels describing the work to be done."}}}`),
	},
	{
		Name:        "workflow_get",
		DisplayName: "Get Workflow",
		Description: "Get the full plan and current step statuses for a workflow by ID.",
		InputSchema: json.RawMessage(`{"type":"object","required":["workflow_id"],"properties":{"workflow_id":{"type":"string","description":"The workflow to retrieve."}}}`),
	},
	{
		Name:        "workflow_update",
		DisplayName: "Update Workflow",
		Description: "Update a workflow step's status after work completes. When all steps reach a terminal status the workflow closes automatically.",
		InputSchema: json.RawMessage(`{"type":"object","required":["workflow_id","step_id","status"],"properties":{"workflow_id":{"type":"string"},"step_id":{"type":"string"},"status":{"type":"string","enum":["running","done","failed","skipped"]},"result":{"type":"string","description":"Final output of the step (optional)."},"failure_reason":{"type":"string","description":"Why the step failed (optional)."}}}`),
	},
	{
		Name:        "workflow_list",
		DisplayName: "List Workflows",
		Description: "List your active workflows and the 10 most recently completed ones. Call this at the start of every turn to check for in-progress work and to recover after a restart.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "workflow_retry_step",
		DisplayName: "Retry Step",
		Description: "Reset a failed step to pending and immediately re-run it with a new sub-agent.",
		InputSchema: json.RawMessage(`{"type":"object","required":["workflow_id","step_id"],"properties":{"workflow_id":{"type":"string"},"step_id":{"type":"string"}}}`),
	},
}

// RegisterWorkflowTools registers all five workflow handlers into d. agentID
// is the conversation that owns the workflows created by this loop. spawnFn
// is used by workflow_retry_step to re-run a failed step's sub-agent.
func RegisterWorkflowTools(d *Dispatcher, agentID string, store *memory.Store, spawnFn SubAgentSpawnFn) {
	d.handlers["workflow_create"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Name  string   `json:"name"`
			Steps []string `json:"steps"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("workflow_create: %w", err)
		}
		result, err := store.WorkflowCreate(agentID, req.Name, req.Steps)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["workflow_get"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			WorkflowID string `json:"workflow_id"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("workflow_get: %w", err)
		}
		wf, err := store.WorkflowGet(req.WorkflowID)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(wf)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["workflow_update"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			WorkflowID    string `json:"workflow_id"`
			StepID        string `json:"step_id"`
			Status        string `json:"status"`
			Result        string `json:"result"`
			FailureReason string `json:"failure_reason"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("workflow_update: %w", err)
		}
		result, err := store.WorkflowUpdate(req.WorkflowID, req.StepID, req.Status, req.Result, req.FailureReason)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(result)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["workflow_list"] = func(_ context.Context, _ json.RawMessage) (string, error) {
		workflows, err := store.WorkflowList(agentID)
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(map[string]any{"workflows": workflows})
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["workflow_retry_step"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			WorkflowID string `json:"workflow_id"`
			StepID     string `json:"step_id"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("workflow_retry_step: %w", err)
		}
		if err := store.WorkflowResetStep(req.WorkflowID, req.StepID); err != nil {
			return "", fmt.Errorf("reset step: %w", err)
		}
		wf, err := store.WorkflowGet(req.WorkflowID)
		if err != nil {
			return "", fmt.Errorf("get workflow: %w", err)
		}
		label := req.StepID
		for _, s := range wf.Steps {
			if s.ID == req.StepID {
				label = s.Label
				break
			}
		}
		store.WorkflowUpdate(req.WorkflowID, req.StepID, "running", "", "") //nolint:errcheck
		result, err := spawnFn(ctx, label, "")
		if err != nil {
			store.WorkflowUpdate(req.WorkflowID, req.StepID, "failed", "", err.Error()) //nolint:errcheck
			return "", err
		}
		store.WorkflowUpdate(req.WorkflowID, req.StepID, "done", result, "") //nolint:errcheck
		return result, nil
	}
}
