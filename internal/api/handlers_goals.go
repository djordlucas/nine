package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// Goal and workflow endpoints.

func (s *Server) ListGoals(ctx context.Context, request apigen.ListGoalsRequestObject) (apigen.ListGoalsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListGoals400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListGoals503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	raw, err := cl.ListGoals()
	if err != nil {
		return apigen.ListGoals500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	rows, err := decodeList[wireGoal](raw, "goals")
	if err != nil {
		return apigen.ListGoals500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	goals := make([]apigen.GoalInfo, 0, len(rows))
	for _, row := range rows {
		goals = append(goals, toGoalInfo(row))
	}

	data, pagination := paginate(goals, p)
	return apigen.ListGoals200JSONResponse{
		Data:       &data,
		Pagination: &pagination,
	}, nil
}

func (s *Server) GetGoal(ctx context.Context, request apigen.GetGoalRequestObject) (apigen.GetGoalResponseObject, error) {
	if request.Id == "" {
		return apigen.GetGoal400JSONResponse(
			errorBody("invalid_request", "missing goal id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetGoal503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	// The wire protocol exposes the goal list but no single-goal query, so
	// select from the list rather than adding a message type for one reader.
	raw, err := cl.ListGoals()
	if err != nil {
		return apigen.GetGoal500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	rows, err := decodeList[wireGoal](raw, "goals")
	if err != nil {
		return apigen.GetGoal500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	for _, row := range rows {
		if row.ID == request.Id {
			return apigen.GetGoal200JSONResponse(toGoalInfo(row)), nil
		}
	}

	return apigen.GetGoal404JSONResponse(
		errorBody("not_found", "goal not found", map[string]any{"id": request.Id})), nil
}

// CreateGoal creates a goal as the operator. The daemon runs it with the same
// standing as a [[agent]] block in nine.toml: the role gate on the agent tool
// goal_create decides which agents may start background work, and an
// authenticated API caller is not an agent.
func (s *Server) CreateGoal(ctx context.Context, request apigen.CreateGoalRequestObject) (apigen.CreateGoalResponseObject, error) {
	if request.Body == nil || strings.TrimSpace(request.Body.Description) == "" {
		return apigen.CreateGoal400JSONResponse(
			errorBody("invalid_request", "description is required", nil)), nil
	}
	parentID := ""
	if request.Body.ParentId != nil {
		parentID = *request.Body.ParentId
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.CreateGoal503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	res, err := cl.CreateGoal(request.Body.Description, parentID)
	if err != nil {
		if errNotFound(err) {
			return apigen.CreateGoal404JSONResponse(
				errorBody("not_found", "parent goal not found", map[string]any{"parent_id": parentID})), nil
		}
		return apigen.CreateGoal500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	var g wireGoal
	if err := json.Unmarshal(res.Goal, &g); err != nil {
		return apigen.CreateGoal500JSONResponse(
			errorBody("server_error", fmt.Sprintf("decode created goal: %v", err), nil)), nil
	}
	out := apigen.CreateGoalResponse{
		Goal:          toGoalInfo(g),
		PursueSession: apigen.CreateGoalResponsePursueSession(res.PursueSession),
	}
	// A top-level goal's session is the goal's own id (docs/goal-sessions.md),
	// whether or not the cap let it start; a sub-goal has none.
	if parentID == "" {
		out.SessionId = ptr(g.ID)
	}
	return apigen.CreateGoal201JSONResponse(out), nil
}

func (s *Server) DeleteGoal(ctx context.Context, request apigen.DeleteGoalRequestObject) (apigen.DeleteGoalResponseObject, error) {
	if request.Id == "" {
		return apigen.DeleteGoal400JSONResponse(
			errorBody("invalid_request", "missing goal id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteGoal503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	res, err := cl.DeleteGoal(request.Id)
	if err != nil {
		switch {
		case errConflict(err):
			return apigen.DeleteGoal409JSONResponse(
				errorBody("conflict", strings.TrimPrefix(strings.TrimPrefix(err.Error(), "daemon: "), protocol.ConflictPrefix),
					map[string]any{"id": request.Id})), nil
		case errNotFound(err):
			return apigen.DeleteGoal404JSONResponse(
				errorBody("not_found", "goal not found", map[string]any{"id": request.Id})), nil
		}
		return apigen.DeleteGoal500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	msg := fmt.Sprintf("deleted %d goal(s)", len(res.Deleted))
	if res.SessionStopped {
		msg += "; its session was stopped and its history kept"
	}
	return apigen.DeleteGoal200JSONResponse{
		Id:             request.Id,
		Message:        ptr(msg),
		Deleted:        res.Deleted,
		SessionStopped: res.SessionStopped,
	}, nil
}

func (s *Server) ListWorkflows(ctx context.Context, request apigen.ListWorkflowsRequestObject) (apigen.ListWorkflowsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListWorkflows400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListWorkflows503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	raw, err := cl.ListWorkflows()
	if err != nil {
		return apigen.ListWorkflows500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	rows, err := decodeList[wireWorkflow](raw, "workflows")
	if err != nil {
		return apigen.ListWorkflows500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	workflows := make([]apigen.WorkflowInfo, 0, len(rows))
	for _, row := range rows {
		workflows = append(workflows, toWorkflowInfo(row))
	}

	data, pagination := paginate(workflows, p)
	return apigen.ListWorkflows200JSONResponse{
		Data:       &data,
		Pagination: &pagination,
	}, nil
}

func (s *Server) StopWorkflow(ctx context.Context, request apigen.StopWorkflowRequestObject) (apigen.StopWorkflowResponseObject, error) {
	if request.Id == "" {
		return apigen.StopWorkflow400JSONResponse(
			errorBody("invalid_request", "missing workflow id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.StopWorkflow503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if err := cl.StopWorkflow(request.Id); err != nil {
		if errNotFound(err) {
			return apigen.StopWorkflow404JSONResponse(
				errorBody("not_found", "workflow not found",
					map[string]any{"id": request.Id})), nil
		}
		return apigen.StopWorkflow500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.StopWorkflow200JSONResponse{
		Id:      ptr(request.Id),
		Message: ptr("workflow cancelled"),
		Status:  ptr("cancelled"),
	}, nil
}

func (s *Server) FailWorkflow(ctx context.Context, request apigen.FailWorkflowRequestObject) (apigen.FailWorkflowResponseObject, error) {
	if request.Id == "" {
		return apigen.FailWorkflow400JSONResponse(
			errorBody("invalid_request", "missing workflow id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.FailWorkflow503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if err := cl.FailWorkflow(request.Id, false); err != nil {
		if errNotFound(err) {
			return apigen.FailWorkflow404JSONResponse(
				errorBody("not_found", "workflow not found",
					map[string]any{"id": request.Id})), nil
		}
		return apigen.FailWorkflow500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.FailWorkflow200JSONResponse{
		Id:      ptr(request.Id),
		Message: ptr("workflow marked as failed"),
		Status:  ptr("failed"),
	}, nil
}
