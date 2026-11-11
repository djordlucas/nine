package api

import (
	"context"

	"nine/internal/api/apigen"
)

// The role registry (adr/orchestrator-board.md §2.2.1): what the UI's agent
// picker lists and what validates a conversation's requested role.

func (s *Server) ListRoles(ctx context.Context, request apigen.ListRolesRequestObject) (apigen.ListRolesResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListRoles503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	roles, err := cl.RolesList()
	if err != nil {
		return apigen.ListRoles500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	out := make([]apigen.RoleInfo, 0, len(roles))
	for _, r := range roles {
		ri := apigen.RoleInfo{
			Name:     r.Name,
			AllTools: r.AllTools,
			Delegates: &r.Delegates,
		}
		if r.Description != "" {
			ri.Description = &r.Description
		}
		if len(r.Tools) > 0 {
			ri.Tools = &r.Tools
		}
		out = append(out, ri)
	}
	return apigen.ListRoles200JSONResponse{Data: &out}, nil
}
