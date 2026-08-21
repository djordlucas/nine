package agent

import (
	"context"
)

// SubAgentSpawnFn spawns a sub-agent for the given task and returns its result.
type SubAgentSpawnFn func(ctx context.Context, task, extraCtx string) (string, error)

// GoalSessionSpawnFn spawns a background "pursue" session for a newly-created
// top-level goal (see docs/goal-sessions.md). spawned reports whether a
// session was actually started; it is false (with no error) if the daemon is
// at its concurrent goal-session limit — the goal itself is still recorded
// either way.
type GoalSessionSpawnFn func(ctx context.Context, goalID string) (spawned bool, err error)

// SubAgentTask is one task entry passed to RegisterRunAgents. Role optionally
// names the worker role for the sub-agent; empty resolves to the default leaf
// role (spec/contracts/roles.md R-ROLE.8).
type SubAgentTask struct {
	Task    string
	Context string
	Role    string
}

// SubAgentResult is the result of one sub-agent execution.
type SubAgentResult struct {
	Task   string
	Result string
	Err    error
}
