package api

import (
	"context"

	"nine/internal/api/apigen"
)

// Capability endpoints: the generated-tool ceiling, and the agent's requests to
// widen it (docs/sandboxed-tools.md).
//
// Both go over the wire protocol to the daemon rather than reading the store
// directly, for the reason every other endpoint does: an approval must apply to the
// daemon that is running the agents, and only that process can install a new ceiling
// on its live tool host.

func (s *Server) ListCapabilities(ctx context.Context, request apigen.ListCapabilitiesRequestObject) (apigen.ListCapabilitiesResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListCapabilities503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	state, err := cl.ListCapabilities()
	if err != nil {
		return apigen.ListCapabilities500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	grants := make([]apigen.CapabilityGrant, 0, len(state.Grants))
	for _, g := range state.Grants {
		grants = append(grants, apigen.CapabilityGrant{
			Id:         g.ID,
			Source:     g.Source,
			Capability: g.Capability,
			Params:     optional(g.Params),
			RequestId:  optional(g.RequestID),
			CreatedAt:  optional(g.CreatedAt),
		})
	}
	requests := make([]apigen.CapabilityRequest, 0, len(state.Requests))
	for _, r := range state.Requests {
		requests = append(requests, apigen.CapabilityRequest{
			Id:         r.ID,
			AgentId:    optional(r.AgentID),
			ToolName:   optional(r.ToolName),
			Capability: r.Capability,
			Params:     optional(r.Params),
			Reason:     optional(r.Reason),
			Status:     r.Status,
			CreatedAt:  optional(r.CreatedAt),
			DecidedAt:  optional(r.DecidedAt),
		})
	}

	return apigen.ListCapabilities200JSONResponse{
		Grants:   &grants,
		Requests: &requests,
	}, nil
}

func (s *Server) DecideCapability(ctx context.Context, request apigen.DecideCapabilityRequestObject) (apigen.DecideCapabilityResponseObject, error) {
	if request.Id == "" {
		return apigen.DecideCapability400JSONResponse(
			errorBody("invalid_request", "missing id", nil)), nil
	}
	if request.Body == nil {
		return apigen.DecideCapability400JSONResponse(
			errorBody("invalid_request", "missing body", nil)), nil
	}
	action := string(request.Body.Action)

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DecideCapability503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	message, err := cl.DecideCapability(request.Id, action)
	if err != nil {
		// An unknown action, an already-settled request and an unrevocable grant are
		// all the caller's mistake, not the server's — they are reported as 400 so a
		// client can distinguish them from a daemon that failed.
		return apigen.DecideCapability400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	return apigen.DecideCapability200JSONResponse{Message: message}, nil
}

// optional returns a pointer to s, or nil when s is empty, for the generated
// structs' optional string fields.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
