package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"nine/internal/llm"
)

// DefaultRoleEnum is the fallback leaf-role list for the `role` field on
// run_agent/run_agents (docs/roles.md R-ROLE.8), naming the built-in leaf
// roles shipped in skills/roles/. A live daemon renders the real list — which
// also covers operator- and agent-authored role skills — from the role
// registry and passes it to SubAgentDefs; this constant only serves storeless
// callers (tests, builders with no memory store).
const DefaultRoleEnum = "One of: executor (default; full toolset), software-dev (implement/modify code: shell + files), sysadmin (operate the system: shell, files, http), report-writer (web research and writing; no shell), monitor (read-only: web, http GET, file reads, memory; no shell or writes)."

// roleFieldDescription wraps a rendered leaf-role list in the guidance the
// model needs to choose well. Empty roleEnum falls back to DefaultRoleEnum.
func roleFieldDescription(roleEnum string) string {
	if roleEnum == "" {
		roleEnum = DefaultRoleEnum
	}
	return "Optional worker role for the sub-agent. " + roleEnum +
		" Pick the narrowest role that fits the task; omit for executor. Unknown roles fall back to executor."
}

// SubAgentDefs returns the sub-agent tool definitions with roleEnum rendered
// into the `role` field of run_agent and run_agents. The role property is
// built once and spliced into both schemas, so the two can never drift.
func SubAgentDefs(roleEnum string) []llm.ToolDef {
	// Marshal through encoding/json so any punctuation in a role description
	// (quotes, backslashes) cannot corrupt the surrounding schema.
	desc, err := json.Marshal(roleFieldDescription(roleEnum))
	if err != nil {
		desc = []byte(`"Optional worker role for the sub-agent."`)
	}
	roleProp := `"role":{"type":"string","description":` + string(desc) + `}`

	return []llm.ToolDef{
		{
			Name:        "gap_report",
			DisplayName: "Gap Report",
			Description: "Report a capability gap to the supervisor when no available tool can accomplish the task. The supervisor will attempt to resolve it autonomously.",
			InputSchema: json.RawMessage(`{"type":"object","required":["description"],"properties":{"description":{"type":"string","description":"What capability is missing and why"}}}`),
		},
		{
			Name:        "run_agent",
			DisplayName: "Run Agent",
			Description: "Spawn a sub-agent to execute a single self-contained task and return its final answer. Use this to isolate a focused piece of work or delegate a subtask. For multiple independent tasks at once, use run_agents.",
			InputSchema: json.RawMessage(`{"type":"object","required":["task"],"properties":{"task":{"type":"string","description":"A clear, self-contained description of the task for the sub-agent."},"context":{"type":"string","description":"Optional background the sub-agent needs. Keep concise."},` + roleProp + `}}`),
		},
		{
			Name:        "run_agents",
			DisplayName: "Run Agents",
			Description: "Spawn multiple sub-agents in parallel and wait for all results. Use this when tasks are independent and can run concurrently. Returns when all agents finish or the timeout expires.",
			InputSchema: json.RawMessage(`{"type":"object","required":["tasks"],"properties":{"tasks":{"type":"array","description":"Tasks to run in parallel.","items":{"type":"object","required":["task"],"properties":{"task":{"type":"string","description":"Self-contained task description."},"context":{"type":"string","description":"Optional background context."},` + roleProp + `}}},"timeout_seconds":{"type":"integer","description":"Maximum seconds to wait for the whole group. Defaults to the daemon's configured task timeout (typically 30 minutes); only lower this if the tasks are known to be quick. Agents still running when the timeout fires are cancelled and marked timed_out."}}}`),
		},
	}
}

// subAgentToolDefs is the static default set folded into InterceptedDefs.
var subAgentToolDefs = SubAgentDefs("")

// RegisterGapReport registers the gap_report handler into d. post is called
// with the description whenever the agent reports a capability gap.
func RegisterGapReport(d *Dispatcher, post func(description string)) {
	d.handlers["gap_report"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Description string `json:"description"`
		}
		json.Unmarshal(args, &req) //nolint:errcheck
		post(req.Description)
		return "gap reported to supervisor", nil
	}
}

// RegisterRunAgent registers the run_agent handler into d. fn receives the
// request context, task description, optional extra context, and optional
// role name (empty resolves to the default leaf role), and returns the
// sub-agent's answer.
func RegisterRunAgent(d *Dispatcher, fn func(ctx context.Context, task, extraCtx, role string) (string, error)) {
	d.handlers["run_agent"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Task    string `json:"task"`
			Context string `json:"context"`
			Role    string `json:"role"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("run_agent: %w", err)
		}
		return fn(ctx, req.Task, req.Context, req.Role)
	}
}

// RegisterRunAgents registers the run_agents handler into d. fn receives the
// request context, task list, and timeout in seconds and returns one result
// per task.
func RegisterRunAgents(d *Dispatcher, fn func(ctx context.Context, tasks []SubAgentTask, timeoutSecs int) []SubAgentResult) {
	d.handlers["run_agents"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Tasks []struct {
				Task    string `json:"task"`
				Context string `json:"context"`
				Role    string `json:"role"`
			} `json:"tasks"`
			TimeoutSeconds int `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("run_agents: %w", err)
		}
		tasks := make([]SubAgentTask, len(req.Tasks))
		for i, t := range req.Tasks {
			tasks[i] = SubAgentTask{Task: t.Task, Context: t.Context, Role: t.Role}
		}
		results := fn(ctx, tasks, req.TimeoutSeconds)

		type resultJSON struct {
			Task   string `json:"task"`
			Status string `json:"status"`
			Result string `json:"result,omitempty"`
			Error  string `json:"error,omitempty"`
		}
		out := make([]resultJSON, len(results))
		for i, r := range results {
			rj := resultJSON{Task: r.Task}
			if errors.Is(r.Err, context.DeadlineExceeded) {
				rj.Status = "timed_out"
			} else if r.Err != nil {
				rj.Status = "failed"
				rj.Error = r.Err.Error()
			} else {
				rj.Status = "done"
				rj.Result = r.Result
			}
			out[i] = rj
		}
		data, err := json.Marshal(map[string]any{"results": out})
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}
