package api

import (
	"context"
	"strings"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// The operator's board CRUD (adr/orchestrator-board.md §2.6): REST over the
// daemon's board_* wire verbs. The daemon decides what each action may do;
// its refusal text maps here — unknown task 404, other refusals 400/409.

func toAPITask(t protocol.BoardTask) apigen.BoardTask {
	out := apigen.BoardTask{
		Id:        t.ID,
		Title:     t.Title,
		PostedBy:  t.PostedBy,
		Status:    apigen.BoardTaskStatus(t.Status),
		Source:    apigen.BoardTaskSource(t.Source),
		Depth:     t.Depth,
		CreatedAt: parseTime(t.CreatedAt),
		UpdatedAt: parseTime(t.UpdatedAt),
	}
	if t.Spec != "" {
		out.Spec = &t.Spec
	}
	if t.ClaimedBy != "" {
		out.ClaimedBy = &t.ClaimedBy
	}
	if t.Result != "" {
		out.Result = &t.Result
	}
	if t.ClaimedAt != "" {
		out.ClaimedAt = ptrTime(parseTime(t.ClaimedAt))
	}
	if t.FinishedAt != "" {
		out.FinishedAt = ptrTime(parseTime(t.FinishedAt))
	}
	return out
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func ptrTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (s *Server) ListBoardTasks(ctx context.Context, request apigen.ListBoardTasksRequestObject) (apigen.ListBoardTasksResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListBoardTasks400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListBoardTasks503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	status := ""
	if request.Params.Status != nil {
		status = string(*request.Params.Status)
		if status != "" && !validStatusParam(*request.Params.Status) {
			return apigen.ListBoardTasks400JSONResponse(
				errorBody("invalid_request", "unknown status "+status, nil)), nil
		}
	}
	tasks, err := cl.BoardList(status)
	if err != nil {
		return apigen.ListBoardTasks500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	infos := make([]apigen.BoardTask, 0, len(tasks))
	for _, t := range tasks {
		infos = append(infos, toAPITask(t))
	}
	data, pagination := paginate(infos, p)
	return apigen.ListBoardTasks200JSONResponse{Data: &data, Pagination: &pagination}, nil
}

func validStatusParam(s apigen.ListBoardTasksParamsStatus) bool {
	switch apigen.BoardTaskStatus(s) {
	case "queued", "running", "done", "failed", "cancelled":
		return true
	}
	return false
}

func (s *Server) GetBoardTask(ctx context.Context, request apigen.GetBoardTaskRequestObject) (apigen.GetBoardTaskResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetBoardTask503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	t, err := cl.BoardGet(request.Id)
	switch {
	case err == nil:
		return apigen.GetBoardTask200JSONResponse(toAPITask(t)), nil
	case strings.Contains(err.Error(), "not found"):
		return apigen.GetBoardTask404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	default:
		return apigen.GetBoardTask500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
}

func (s *Server) CreateBoardTask(ctx context.Context, request apigen.CreateBoardTaskRequestObject) (apigen.CreateBoardTaskResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.CreateBoardTask503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if request.Body == nil || strings.TrimSpace(request.Body.Title) == "" {
		return apigen.CreateBoardTask400JSONResponse(
			errorBody("invalid_request", "title is required", nil)), nil
	}
	spec := ""
	if request.Body.Spec != nil {
		spec = *request.Body.Spec
	}
	t, err := cl.BoardCreate(request.Body.Title, spec)
	switch {
	case err == nil:
		return apigen.CreateBoardTask200JSONResponse(toAPITask(t)), nil
	default:
		return apigen.CreateBoardTask400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
}

func (s *Server) UpdateBoardTask(ctx context.Context, request apigen.UpdateBoardTaskRequestObject) (apigen.UpdateBoardTaskResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.UpdateBoardTask503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	patch := map[string]any{}
	if request.Body != nil {
		if request.Body.Title != nil {
			patch["title"] = *request.Body.Title
		}
		if request.Body.Spec != nil {
			patch["spec"] = *request.Body.Spec
		}
		if request.Body.Result != nil {
			patch["result"] = *request.Body.Result
		}
		if request.Body.Status != nil {
			patch["status"] = string(*request.Body.Status)
		}
		if request.Body.ClearClaim != nil && *request.Body.ClearClaim {
			patch["clear_claim"] = true
		}
	}
	if len(patch) == 0 {
		return apigen.UpdateBoardTask400JSONResponse(
			errorBody("invalid_request", "nothing to update", map[string]any{"id": request.Id})), nil
	}
	t, err := cl.BoardUpdate(request.Id, patch)
	switch {
	case err == nil:
		return apigen.UpdateBoardTask200JSONResponse(toAPITask(t)), nil
	case strings.Contains(err.Error(), "not found"):
		return apigen.UpdateBoardTask404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	case strings.Contains(err.Error(), "unknown status"):
		return apigen.UpdateBoardTask400JSONResponse(
			errorBody("invalid_request", err.Error(), map[string]any{"id": request.Id})), nil
	default:
		return apigen.UpdateBoardTask409JSONResponse(
			errorBody("conflict", err.Error(), map[string]any{"id": request.Id})), nil
	}
}

func (s *Server) DeleteBoardTask(ctx context.Context, request apigen.DeleteBoardTaskRequestObject) (apigen.DeleteBoardTaskResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteBoardTask503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	err = cl.BoardDelete(request.Id)
	switch {
	case err == nil:
		return apigen.DeleteBoardTask200JSONResponse(
			apigen.BoardTaskDeleteResponse{Id: request.Id, Message: "deleted " + request.Id}), nil
	case strings.Contains(err.Error(), "not found"):
		return apigen.DeleteBoardTask404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	default:
		return apigen.DeleteBoardTask500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
}
