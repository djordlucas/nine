package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"nine/internal/memory"
)

// pursueRoutine implements the "pursue" RoutineHandler (see
// docs/goal-sessions.md and docs/session-plans.md Pilot 2): each pursue
// session is keyed 1:1 to a goal (agentID == goalID). OnIdle prompts the
// session to assess and act on that goal; OnTurnEnd reads the goal back and
// syncs this routine's Status from goals.status, so the idle scheduler stops
// arming further turns once the goal is no longer active.
type pursueRoutine struct {
	store *memory.Store
}

// NewPursueRoutine creates the "pursue" RoutineHandler.
func NewPursueRoutine(store *memory.Store) RoutineHandler {
	return &pursueRoutine{store: store}
}

func (s *pursueRoutine) Init(context.Context, string, json.RawMessage) error { return nil }

// OnTurnEnd syncs this routine's Status from goals.status (Pilot 4's mapping:
// active->active, paused->paused, done/archived->done). On ErrStall, it also
// pauses the goal — a stalled pursue session frees its slot under
// MaxGoalSessions (docs/goal-sessions.md "Resource bounds").
func (s *pursueRoutine) OnTurnEnd(_ context.Context, agentID string, _ string, err error) error {
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
//
// When the goal is no longer active, OnIdle also syncs this routine's Status
// before returning. Without this, a goal paused/finished/archived (e.g. via
// goal_update_status) while the session sat idle would never reach OnTurnEnd —
// which only fires after a turn — so the pursue routine would linger in "active":
// the idle scheduler would re-arm forever and the session would keep counting
// against MaxGoalSessions. Syncing here retires the stage on the idle path too,
// mirroring OnTurnEnd. A missing goal collapses to "done".
func (s *pursueRoutine) OnIdle(_ context.Context, agentID string) (string, bool) {
	goal, err := s.store.GoalGet(agentID)
	if err != nil {
		return "", false
	}
	if goal == nil || goal.Status != "active" {
		status := "done"
		if goal != nil {
			status = pursueStageStatus(goal.Status)
		}
		if serr := s.syncStatus(agentID, status); serr != nil {
			slog.Warn("pursue routine idle status sync failed", "agent_id", agentID, "err", serr)
		}
		return "", false
	}
	return fmt.Sprintf(PursuePromptTemplate, agentID, goal.Description), true
}

// pursueStageStatus maps goals.status to this routine's SessionRoutine.Status
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
func (s *pursueRoutine) syncStatus(agentID, status string) error {
	plan, err := s.store.SessionPlanGet(agentID)
	if err != nil {
		return err
	}
	if plan == nil {
		return nil
	}
	changed := false
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range plan.Routines {
		if plan.Routines[i].Kind != "pursue" {
			continue
		}
		if plan.Routines[i].Status != status {
			plan.Routines[i].Status = status
			plan.Routines[i].UpdatedAt = now
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.store.SessionPlanSave(plan)
}
