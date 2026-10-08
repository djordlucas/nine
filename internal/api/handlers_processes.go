package api

import (
	"context"
	"strings"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// The process roster and the operator's control of it (docs/processes.md). The
// daemon decides what each action may do; its refusal text says why, and is
// mapped to a status here: an unknown process is 404, any other refusal 409.

func (s *Server) ListProcesses(ctx context.Context, request apigen.ListProcessesRequestObject) (apigen.ListProcessesResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListProcesses400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListProcesses503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	procs, err := cl.ListProcesses()
	if err != nil {
		return apigen.ListProcesses500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	infos := make([]apigen.ProcessInfo, 0, len(procs))
	for _, pr := range procs {
		infos = append(infos, toProcessInfo(pr))
	}
	data, pagination := paginate(infos, p)
	return apigen.ListProcesses200JSONResponse{Data: &data, Pagination: &pagination}, nil
}

func (s *Server) GetProcess(ctx context.Context, request apigen.GetProcessRequestObject) (apigen.GetProcessResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetProcess503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	pr, err := cl.ShowProcess(request.Id, 20)
	switch {
	case err == nil:
		return apigen.GetProcess200JSONResponse(toProcessInfo(pr)), nil
	case unknownProcess(err):
		return apigen.GetProcess404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	default:
		return apigen.GetProcess500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
}

func (s *Server) StartProcess(ctx context.Context, request apigen.StartProcessRequestObject) (apigen.StartProcessResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.StartProcess503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msg, err := cl.ControlProcess(request.Id, "start")
	switch {
	case err == nil:
		return apigen.StartProcess200JSONResponse{Id: request.Id, Message: msg}, nil
	case unknownProcess(err):
		return apigen.StartProcess404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	default:
		return apigen.StartProcess409JSONResponse(
			errorBody("conflict", err.Error(), map[string]any{"id": request.Id})), nil
	}
}

func (s *Server) StopProcess(ctx context.Context, request apigen.StopProcessRequestObject) (apigen.StopProcessResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.StopProcess503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msg, err := cl.ControlProcess(request.Id, "stop")
	switch {
	case err == nil:
		return apigen.StopProcess200JSONResponse{Id: request.Id, Message: msg}, nil
	case unknownProcess(err):
		return apigen.StopProcess404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	default:
		return apigen.StopProcess409JSONResponse(
			errorBody("conflict", err.Error(), map[string]any{"id": request.Id})), nil
	}
}

func (s *Server) SendProcessMessage(ctx context.Context, request apigen.SendProcessMessageRequestObject) (apigen.SendProcessMessageResponseObject, error) {
	if request.Body == nil || strings.TrimSpace(request.Body.Text) == "" {
		return apigen.SendProcessMessage400JSONResponse(
			errorBody("invalid_request", "text is required", nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.SendProcessMessage503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msg, err := cl.SendProcess(request.Id, request.Body.Text)
	switch {
	case err == nil:
		return apigen.SendProcessMessage200JSONResponse{Id: request.Id, Message: msg}, nil
	case unknownProcess(err):
		return apigen.SendProcessMessage404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	default:
		return apigen.SendProcessMessage409JSONResponse(
			errorBody("conflict", err.Error(), map[string]any{"id": request.Id})), nil
	}
}

// unknownProcess reports whether the daemon refused for an id no process has.
func unknownProcess(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such process")
}

func toProcessInfo(p protocol.ProcessInfo) apigen.ProcessInfo {
	info := apigen.ProcessInfo{
		Id: p.ID, Tool: p.Tool, Trigger: p.Trigger,
		Mode:  apigen.ProcessInfoMode(p.Mode),
		State: apigen.ProcessInfoState(p.State),
		Budget: apigen.ProcessBudget{
			Turns: p.BudgetTurns, TurnsPerDay: p.BudgetTurnsPerDay,
			Tokens: p.BudgetTokens, TokensPerDay: p.BudgetTokensPerDay,
			ResetsAt: optTime(p.BudgetResetsAt),
		},
		StoppedBy: optString(p.StoppedBy), StoppedAt: optTime(p.StoppedAt),
		Session: optString(p.Session), Goal: optString(p.Goal),
		ReportTo: optString(p.ReportTo), Role: optString(p.Role),
		LastError: optString(p.LastError), LastCallAt: optTime(p.LastCallAt), NextAt: optTime(p.NextAt),
	}
	if p.Attached {
		info.Attached = ptr(true)
	}
	if p.Generated {
		info.Generated = ptr(true)
	}
	if p.Calls > 0 {
		info.Calls = ptr(p.Calls)
	}
	if p.Cycles > 0 {
		info.Cycles = ptr(p.Cycles)
	}
	if p.Failures > 0 {
		info.Failures = ptr(p.Failures)
	}
	if len(p.Recent) > 0 {
		recent := make([]apigen.ProcessActivity, 0, len(p.Recent))
		for _, e := range p.Recent {
			a := apigen.ProcessActivity{Outcome: e.Outcome, Detail: optString(e.Detail)}
			if t := optTime(e.At); t != nil {
				a.At = *t
			}
			recent = append(recent, a)
		}
		info.Recent = &recent
	}
	return info
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optTime parses an RFC 3339 time, nil when it is empty or unreadable.
func optTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	return &t
}
