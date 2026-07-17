package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/llm"
	"nine/internal/memory"
)

var goalToolDefs = []llm.ToolDef{
	{
		Name:        "goal_create",
		DisplayName: "Create Goal",
		Description: "Create a persistent, open-ended goal (no defined end condition, e.g. 'monitor this repo for security issues'). Defaults to the current conversation as parent if parent_id is omitted. Returns the created goal; top-level goals (parent_type 'conversation') also get a background 'pursue' session that works on the goal between your turns — the response's pursue_session field is 'spawned' if that session started, or 'limit_reached' if the daemon is already at its concurrent goal-session cap (the goal is still recorded either way).",
		InputSchema: json.RawMessage(`{"type":"object","required":["description"],"properties":{"description":{"type":"string","description":"What the goal is."},"parent_id":{"type":"string","description":"ID of the conversation or goal that spawned this one. Defaults to the current conversation."},"parent_type":{"type":"string","enum":["conversation","goal"],"description":"Type of the parent referenced by parent_id."}}}`),
	},
	{
		Name:        "goal_get",
		DisplayName: "Get Goal",
		Description: "Get a goal by ID, including its status and sub-goal/task tree.",
		InputSchema: json.RawMessage(`{"type":"object","required":["goal_id"],"properties":{"goal_id":{"type":"string","description":"The goal to retrieve."}}}`),
	},
	{
		Name:        "goal_list",
		DisplayName: "List Goals",
		Description: "List all goals. Call this at the start of any turn that involves open-ended, persistent work to check for active goals and recover after a restart.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "goal_update_status",
		DisplayName: "Update Goal",
		Description: "Update a goal's status.",
		InputSchema: json.RawMessage(`{"type":"object","required":["goal_id","status"],"properties":{"goal_id":{"type":"string"},"status":{"type":"string","enum":["active","paused","done","archived"],"description":"active = in progress, paused = temporarily on hold, done = completed or resolved, archived = retired without completion."}}}`),
	},
	{
		Name:        "goal_append_subtree",
		DisplayName: "Goal Subtree",
		Description: "Append an entry (typically a sub-goal or task ID) to a goal's append-only subtree log, recording what it has spawned.",
		InputSchema: json.RawMessage(`{"type":"object","required":["goal_id","entry"],"properties":{"goal_id":{"type":"string"},"entry":{"type":"string","description":"The sub-goal or task ID (or short description) to record."}}}`),
	},
}

// RegisterGoalTools registers all five goal handlers into d (goal_create plus
// the four self-management tools). It is the combined convenience used where a
// role gets both delegation and goal-ownership; the daemon's loop builder wires
// the two subsets independently (docs/predefined-agents.md §3.1).
func RegisterGoalTools(d *Dispatcher, agentID string, store *memory.Store, spawnFn GoalSessionSpawnFn) {
	RegisterGoalCreate(d, agentID, store, spawnFn)
	RegisterGoalManagement(d, store)
}

// RegisterGoalCreate registers the goal_create handler into d. agentID is the
// conversation that owns goals created without an explicit parent. spawnFn,
// if non-nil, is called for newly-created top-level goals to start their
// background "pursue" session — pass nil for sub-goals or at depths that
// should not spawn sessions (see docs/goal-sessions.md).
//
// goal_create is a delegation/orchestration tool: it is gated behind a role's
// Delegates flag. The goal *self-management* tools (goal_get, goal_list,
// goal_update_status, goal_append_subtree) are registered separately by
// RegisterGoalManagement, since a standing agent must steer its own goal
// regardless of whether it can delegate (docs/predefined-agents.md §3.1).
func RegisterGoalCreate(d *Dispatcher, agentID string, store *memory.Store, spawnFn GoalSessionSpawnFn) {
	d.handlers["goal_create"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Description string `json:"description"`
			ParentID    string `json:"parent_id"`
			ParentType  string `json:"parent_type"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("goal_create: %w", err)
		}
		if req.Description == "" {
			return "", fmt.Errorf("goal_create: description is required")
		}
		if req.ParentID == "" {
			req.ParentID = agentID
			req.ParentType = "conversation"
		}
		id := memory.NewID()
		if err := store.GoalCreate(id, req.Description, req.ParentID, req.ParentType); err != nil {
			return "", err
		}
		g, err := store.GoalGet(id)
		if err != nil {
			return "", err
		}

		var result map[string]any
		b, err := json.Marshal(g)
		if err != nil {
			return "", err
		}
		if err := json.Unmarshal(b, &result); err != nil {
			return "", err
		}

		if spawnFn != nil && req.ParentType == "conversation" {
			spawned, err := spawnFn(ctx, id)
			if err != nil {
				return "", err
			}
			if spawned {
				result["pursue_session"] = "spawned"
			} else {
				result["pursue_session"] = "limit_reached"
			}
		}

		data, err := json.Marshal(result)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

// RegisterGoalManagement registers the goal self-management handlers into d:
// goal_get, goal_list, goal_update_status, and goal_append_subtree. These are
// the tools a goal-owning session uses to steer its own goal (read status,
// pause/finish itself, record findings). They carry no delegation authority and
// are registered for every pursue-shell session regardless of role
// (docs/predefined-agents.md §3.1), and for delegating roles alongside
// RegisterGoalCreate.
func RegisterGoalManagement(d *Dispatcher, store *memory.Store) {
	d.handlers["goal_get"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			GoalID string `json:"goal_id"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("goal_get: %w", err)
		}
		g, err := store.GoalGet(req.GoalID)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", fmt.Errorf("goal not found: %s", req.GoalID)
		}
		data, err := json.Marshal(g)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["goal_list"] = func(_ context.Context, _ json.RawMessage) (string, error) {
		goals, err := store.GoalList()
		if err != nil {
			return "", err
		}
		data, err := json.Marshal(map[string]any{"goals": goals})
		if err != nil {
			return "", err
		}
		return string(data), nil
	}

	d.handlers["goal_update_status"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			GoalID string `json:"goal_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("goal_update_status: %w", err)
		}
		if err := store.GoalUpdateStatus(req.GoalID, req.Status); err != nil {
			return "", err
		}
		return "ok", nil
	}

	d.handlers["goal_append_subtree"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			GoalID string `json:"goal_id"`
			Entry  string `json:"entry"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("goal_append_subtree: %w", err)
		}
		if err := store.GoalAppendSubtree(req.GoalID, req.Entry); err != nil {
			return "", err
		}
		return "ok", nil
	}
}
