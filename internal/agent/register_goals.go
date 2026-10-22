package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"nine/internal/cron"
	"nine/internal/llm"
	"nine/internal/memory"
)

var goalToolDefs = []llm.ToolDef{
	{
		Name:        "goal_create",
		DisplayName: "Create Goal",
		Description: "Create a persistent, open-ended goal (no defined end condition, e.g. 'monitor this repo for security issues'). A top-level goal gets a background session that works on it between your turns. Give a schedule for recurring work ('keep me updated', 'a daily digest'): the session then does the whole job once per run instead of checking every few minutes.",
		InputSchema: json.RawMessage(`{"type":"object","required":["description"],"properties":{"description":{"type":"string"},"schedule":{"type":"string","description":"For recurring work on a top-level goal: how often it runs, as a duration (24h, 6h) or a 5-field cron expression ('0 8 * * *' is daily at 08:00 UTC). Leave out for a goal to work on until it is done."},"parent_id":{"type":"string","description":"The conversation or goal this one comes from. Defaults to the current conversation."},"parent_type":{"type":"string","enum":["conversation","goal"]}}}`),
	},
	{
		Name:        "goal_get",
		DisplayName: "Get Goal",
		Description: "Get a goal by ID, including its status and sub-goal/task tree.",
		InputSchema: json.RawMessage(`{"type":"object","required":["goal_id"],"properties":{"goal_id":{"type":"string"}}}`),
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
}

// goalStatuses are the statuses goal_update_status accepts, as its schema lists.
var goalStatuses = []string{"active", "paused", "done", "archived"}

// RegisterGoalTools registers all four goal handlers into d (goal_create plus
// the three self-management tools). It is the combined convenience used where a
// role gets both delegation and goal-ownership; the daemon's loop builder wires
// the two subsets independently (adr/predefined-agents-design.md §3.1).
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
// goal_update_status) are registered separately by
// RegisterGoalManagement, since a standing agent must steer its own goal
// regardless of whether it can delegate (adr/predefined-agents-design.md §3.1).
func RegisterGoalCreate(d *Dispatcher, agentID string, store *memory.Store, spawnFn GoalSessionSpawnFn) {
	d.handlers["goal_create"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Description string `json:"description"`
			Schedule    string `json:"schedule"`
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
		if req.Schedule != "" {
			if req.ParentType != "conversation" {
				return "", fmt.Errorf("goal_create: only a top-level goal recurs; a sub-goal is worked on in its parent's runs")
			}
			if spawnFn == nil {
				return "", fmt.Errorf("goal_create: this session cannot start a goal's runs, so its goal cannot recur; leave out the schedule")
			}
			if _, _, err := cron.ParseCadence(req.Schedule); err != nil {
				return "", fmt.Errorf("goal_create: %w", err)
			}
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
			spawned, err := spawnFn(ctx, id, req.Schedule)
			if err != nil {
				return "", err
			}
			if spawned {
				result["pursue_session"] = "spawned"
			} else {
				result["pursue_session"] = "limit_reached"
			}
			// The cadence lives on the goal's process, which the spawn wrote.
			if g, err := store.GoalGet(id); err == nil && g != nil && g.Schedule != "" {
				result["schedule"] = g.Schedule
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
// goal_get, goal_list, and goal_update_status. These are
// the tools a goal-owning session uses to steer its own goal (read status,
// pause/finish itself, record findings). They carry no delegation authority and
// are registered for every pursue-shell session regardless of role
// (adr/predefined-agents-design.md §3.1), and for delegating roles alongside
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
		// The store writes any string to any id and reports success even when
		// no goal matched, so a wrong id or a misspelt status came back "ok"
		// and the model believed the goal had changed.
		if req.GoalID == "" {
			return "", fmt.Errorf("goal_update_status: goal_id is required")
		}
		if !slices.Contains(goalStatuses, req.Status) {
			return "", fmt.Errorf("goal_update_status: status %q is not one of %s", req.Status, strings.Join(goalStatuses, ", "))
		}
		g, err := store.GoalGet(req.GoalID)
		if err != nil {
			return "", err
		}
		if g == nil {
			return "", fmt.Errorf("goal not found: %s; goal_list shows every goal's id", req.GoalID)
		}
		if err := store.GoalUpdateStatus(req.GoalID, req.Status); err != nil {
			return "", err
		}
		return "ok", nil
	}
}
