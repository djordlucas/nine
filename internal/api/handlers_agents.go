package api

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// Stored agents (adr/agents-concept.md A5, phase 1): REST over the daemon's
// agent_* wire verbs. The daemon owns validation; its refusal text maps here —
// unknown agent 404, validation refusals 400.

func toAPIAgent(a protocol.Agent) apigen.Agent {
	def := apigen.AgentDefinition{Name: a.Definition.Name}
	if a.Definition.Role != "" {
		def.Role = &a.Definition.Role
	}
	if len(a.Definition.Skills) > 0 {
		def.Skills = &a.Definition.Skills
	}
	if len(a.Definition.SelfModel) > 0 {
		var m map[string]any
		if json.Unmarshal(a.Definition.SelfModel, &m) == nil {
			def.SelfModel = &m
		}
	}
	if len(a.Definition.Reflection) > 0 {
		var m map[string]any
		if json.Unmarshal(a.Definition.Reflection, &m) == nil {
			def.Reflection = &m
		}
	}
	if a.Definition.Context != "" {
		def.Context = (*apigen.AgentDefinitionContext)(&a.Definition.Context)
	}
	return apigen.Agent{
		Id: a.ID, Version: a.Version, Status: apigen.AgentStatus(a.Status), Definition: def,
		CreatedAt: parseTime(a.CreatedAt), UpdatedAt: parseTime(a.UpdatedAt),
	}
}

func toWireDefinition(d apigen.AgentDefinition) protocol.AgentDefinition {
	out := protocol.AgentDefinition{Name: d.Name}
	if d.Role != nil {
		out.Role = *d.Role
	}
	if d.Skills != nil {
		out.Skills = *d.Skills
	}
	if d.SelfModel != nil {
		raw, _ := json.Marshal(*d.SelfModel)
		out.SelfModel = raw
	}
	if d.Reflection != nil {
		raw, _ := json.Marshal(*d.Reflection)
		out.Reflection = raw
	}
	if d.Context != nil {
		out.Context = string(*d.Context)
	}
	return out
}

func (s *Server) ListAgents(ctx context.Context, request apigen.ListAgentsRequestObject) (apigen.ListAgentsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListAgents400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListAgents503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	agents, err := cl.AgentList()
	if err != nil {
		return apigen.ListAgents500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	infos := make([]apigen.Agent, 0, len(agents))
	for _, a := range agents {
		infos = append(infos, toAPIAgent(a))
	}
	data, pagination := paginate(infos, p)
	return apigen.ListAgents200JSONResponse{Data: &data, Pagination: &pagination}, nil
}

func (s *Server) CreateAgent(ctx context.Context, request apigen.CreateAgentRequestObject) (apigen.CreateAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.CreateAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if request.Body == nil {
		return apigen.CreateAgent400JSONResponse(
			errorBody("invalid_request", "request body is required", nil)), nil
	}
	a, err := cl.AgentCreate(toWireDefinition(*request.Body))
	if err != nil {
		return apigen.CreateAgent400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	return apigen.CreateAgent200JSONResponse(toAPIAgent(a)), nil
}

func (s *Server) GetAgent(ctx context.Context, request apigen.GetAgentRequestObject) (apigen.GetAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	a, err := cl.AgentGet(request.Id)
	if err == nil {
		return apigen.GetAgent200JSONResponse(toAPIAgent(a)), nil
	}
	if errNotFound(err) {
		return apigen.GetAgent404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	}
	return apigen.GetAgent500JSONResponse(
		errorBody("server_error", err.Error(), nil)), nil
}

func (s *Server) UpdateAgent(ctx context.Context, request apigen.UpdateAgentRequestObject) (apigen.UpdateAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.UpdateAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if request.Body == nil {
		return apigen.UpdateAgent400JSONResponse(
			errorBody("invalid_request", "request body is required", nil)), nil
	}
	patch := map[string]any{}
	b := *request.Body
	if b.Name != nil {
		patch["name"] = *b.Name
	}
	if b.Role != nil {
		patch["role"] = *b.Role
	}
	if b.Skills != nil {
		patch["skills"] = *b.Skills
	}
	if b.SelfModel != nil {
		patch["self_model"] = *b.SelfModel
	}
	if b.Reflection != nil {
		patch["reflection"] = *b.Reflection
	}
	if b.Context != nil {
		patch["context"] = string(*b.Context)
	}
	a, err := cl.AgentUpdate(request.Id, patch)
	if err == nil {
		return apigen.UpdateAgent200JSONResponse(toAPIAgent(a)), nil
	}
	if errNotFound(err) {
		return apigen.UpdateAgent404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	}
	return apigen.UpdateAgent400JSONResponse(
		errorBody("invalid_request", err.Error(), nil)), nil
}

func (s *Server) DeleteAgent(ctx context.Context, request apigen.DeleteAgentRequestObject) (apigen.DeleteAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	err = cl.AgentDelete(request.Id)
	if err == nil {
		return apigen.DeleteAgent200JSONResponse(apigen.AgentDeleteResponse{Id: request.Id, Deleted: true}), nil
	}
	if errNotFound(err) {
		return apigen.DeleteAgent404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	}
	return apigen.DeleteAgent500JSONResponse(
		errorBody("server_error", err.Error(), nil)), nil
}

func (s *Server) ImportAgent(ctx context.Context, request apigen.ImportAgentRequestObject) (apigen.ImportAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ImportAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if request.Body == nil || request.Body.TarBase64 == "" {
		return apigen.ImportAgent400JSONResponse(
			errorBody("invalid_request", "request body with tar_base64 is required", nil)), nil
	}
	tarBytes, err := base64.StdEncoding.DecodeString(request.Body.TarBase64)
	if err != nil {
		return apigen.ImportAgent400JSONResponse(
			errorBody("invalid_request", "tar_base64 is not base64: "+err.Error(), nil)), nil
	}
	a, err := cl.AgentImport(tarBytes)
	if err != nil {
		return apigen.ImportAgent400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	return apigen.ImportAgent200JSONResponse(toAPIAgent(a)), nil
}

func (s *Server) ExportAgent(ctx context.Context, request apigen.ExportAgentRequestObject) (apigen.ExportAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ExportAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	res, err := cl.AgentExport(request.Id)
	if err == nil {
		return apigen.ExportAgent200JSONResponse(apigen.AgentExportResult{
			Filename: res.Filename, TarBase64: res.TarBase64,
		}), nil
	}
	if errNotFound(err) {
		return apigen.ExportAgent404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	}
	return apigen.ExportAgent500JSONResponse(
		errorBody("server_error", err.Error(), nil)), nil
}

func (s *Server) ApproveAgent(ctx context.Context, request apigen.ApproveAgentRequestObject) (apigen.ApproveAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ApproveAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	a, err := cl.AgentApprove(request.Id)
	if err == nil {
		return apigen.ApproveAgent200JSONResponse(toAPIAgent(a)), nil
	}
	if errNotFound(err) {
		return apigen.ApproveAgent404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	}
	return apigen.ApproveAgent500JSONResponse(
		errorBody("server_error", err.Error(), nil)), nil
}

func (s *Server) RunAgent(ctx context.Context, request apigen.RunAgentRequestObject) (apigen.RunAgentResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.RunAgent503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	interactive := request.Body != nil && request.Body.Interactive != nil && *request.Body.Interactive
	res, err := cl.AgentRun(request.Id, interactive)
	if err == nil {
		out := apigen.AgentRunResult{AgentId: res.AgentID, ConversationId: res.ConversationID}
		if res.Role != "" {
			out.Role = &res.Role
		}
		return apigen.RunAgent200JSONResponse(out), nil
	}
	if errNotFound(err) {
		return apigen.RunAgent404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"id": request.Id})), nil
	}
	return apigen.RunAgent400JSONResponse(
		errorBody("invalid_request", err.Error(), nil)), nil
}
