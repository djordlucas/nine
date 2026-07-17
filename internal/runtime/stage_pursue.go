package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"nine/internal/memory"
)

// pursueStage implements the "pursue" StageHandler (see
// docs/goal-sessions.md and docs/session-plans.md Pilot 2): each pursue
// session is keyed 1:1 to a goal (agentID == goalID). OnIdle prompts the
// session to assess and act on that goal; OnTurnEnd reads the goal back and
// syncs this stage's Status from goals.status, so the idle scheduler stops
// arming further turns once the goal is no longer active.
type pursueStage struct {
	store *memory.Store
}

// NewPursueStage creates the "pursue" StageHandler.
func NewPursueStage(store *memory.Store) StageHandler {
	return &pursueStage{store: store}
}

func (s *pursueStage) Init(context.Context, string, json.RawMessage) error { return nil }

// OnTurnEnd syncs this stage's Status from goals.status (Pilot 4's mapping:
// active->active, paused->paused, done/archived->done). On ErrStall, it also
// pauses the goal — a stalled pursue session frees its slot under
// MaxGoalSessions (docs/goal-sessions.md "Resource bounds").
func (s *pursueStage) OnTurnEnd(_ context.Context, agentID string, _ string, err error) error {
	if errors.Is(err, ErrStall) {
		if uerr := s.store.GoalUpdateStatus(agentID, "paused"); uerr != nil {
			return uerr
		}
	}
	goal, gerr := s.store.GoalGet(agentID)
	if gerr != nil {
		return gerr
	}
	if goal == nil {
		return nil
	}
	return s.syncStatus(agentID, pursueStageStatus(goal.Status))
}

// OnIdle prompts the session to assess and act on its goal, unless the goal
// is missing or no longer active.
func (s *pursueStage) OnIdle(_ context.Context, agentID string) (string, bool) {
	goal, err := s.store.GoalGet(agentID)
	if err != nil || goal == nil || goal.Status != "active" {
		return "", false
	}
	return fmt.Sprintf(PursuePromptTemplate, agentID, goal.Description), true
}

// pursueStageStatus maps goals.status to this stage's SessionStage.Status
// (docs/session-plans.md Pilot 4, "Reconciling the three status enums"):
// active->active, paused->paused, and both done and archived collapse to
// done — the stage only needs a binary "more to do / no more to do"
// distinction.
func pursueStageStatus(goalStatus string) string {
	switch goalStatus {
	case "paused":
		return "paused"
	case "done", "archived":
		return "done"
	default:
		return "active"
	}
}

// syncStatus updates the "pursue" stage's Status in agentID's session_plans
// row if it differs from status, and persists the change.
func (s *pursueStage) syncStatus(agentID, status string) error {
	plan, err := s.store.SessionPlanGet(agentID)
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	changed := false
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range plan.Stages {
		if plan.Stages[i].Kind != "pursue" {
			continue
		}
		if plan.Stages[i].Status != status {
			plan.Stages[i].Status = status
			plan.Stages[i].UpdatedAt = now
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.store.SessionPlanSave(plan)
}
