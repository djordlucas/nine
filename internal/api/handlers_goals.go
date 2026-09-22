package api

import (
	"context"

	"nine/internal/api/apigen"
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

// CreateGoal is not implemented: see the detail below.
func (s *Server) CreateGoal(ctx context.Context, request apigen.CreateGoalRequestObject) (apigen.CreateGoalResponseObject, error) {
	return apigen.CreateGoal501JSONResponse(notImplementedBody(
		"goal creation exists only as the agent tool goal_create, which is " +
			"gated behind a role's Delegates flag; routing the API through it " +
			"would have to decide what role an HTTP caller has")), nil
}

// DeleteGoal is not implemented: see the detail below.
func (s *Server) DeleteGoal(ctx context.Context, request apigen.DeleteGoalRequestObject) (apigen.DeleteGoalResponseObject, error) {
	return apigen.DeleteGoal501JSONResponse(notImplementedBody(
		"no goal deletion exists to call: the tool surface has goal_create, " +
			"goal_get, goal_list and goal_update_status, and the wire protocol " +
			"has no goal mutation message")), nil
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
