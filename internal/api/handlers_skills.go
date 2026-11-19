package api

import (
	"context"
	"strings"

	"nine/internal/api/apigen"
)

// Operator skill writes (adr/agents-concept.md A8, Q23): the Builder UI's
// CRUD over the same store rows the model's skill_write/skill_modify tools
// write — one store, two front doors. The daemon enforces the source-ownership
// rules (builtin/user skills are read-only); its refusal maps to 400 here.

func (s *Server) CreateSkill(ctx context.Context, request apigen.CreateSkillRequestObject) (apigen.CreateSkillResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.CreateSkill503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if request.Body == nil || strings.TrimSpace(request.Body.Name) == "" {
		return apigen.CreateSkill400JSONResponse(
			errorBody("invalid_request", "name is required", nil)), nil
	}
	tags := []string{}
	if request.Body.Tags != nil {
		tags = *request.Body.Tags
	}
	desc := ""
	if request.Body.Description != nil {
		desc = *request.Body.Description
	}
	sk, err := cl.SkillCreate(request.Body.Name, desc, tags, request.Body.Content)
	if err != nil {
		return apigen.CreateSkill400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	return apigen.CreateSkill200JSONResponse{
		Name:        sk.Name,
		Description: ptr(sk.Description),
		Tags:        &sk.Tags,
		Source:      apigen.SkillDetailSource(sk.Source),
		Content:     sk.Content,
	}, nil
}

func (s *Server) UpdateSkill(ctx context.Context, request apigen.UpdateSkillRequestObject) (apigen.UpdateSkillResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.UpdateSkill503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	patch := map[string]any{}
	if request.Body != nil {
		if request.Body.Description != nil {
			patch["description"] = *request.Body.Description
		}
		if request.Body.Tags != nil {
			patch["tags"] = *request.Body.Tags
		}
		if request.Body.Content != nil {
			patch["content"] = *request.Body.Content
		}
	}
	if len(patch) == 0 {
		return apigen.UpdateSkill400JSONResponse(
			errorBody("invalid_request", "nothing to update", map[string]any{"name": request.Name})), nil
	}
	sk, err := cl.SkillUpdate(request.Name, patch)
	if err != nil {
		if errNotFound(err) {
			return apigen.UpdateSkill404JSONResponse(
				errorBody("not_found", "skill not found", map[string]any{"name": request.Name})), nil
		}
		return apigen.UpdateSkill400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	return apigen.UpdateSkill200JSONResponse{
		Name:        sk.Name,
		Description: ptr(sk.Description),
		Tags:        &sk.Tags,
		Source:      apigen.SkillDetailSource(sk.Source),
		Content:     sk.Content,
	}, nil
}

func (s *Server) DeleteSkill(ctx context.Context, request apigen.DeleteSkillRequestObject) (apigen.DeleteSkillResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteSkill503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if err := cl.SkillDelete(request.Name); err != nil {
		if errNotFound(err) {
			return apigen.DeleteSkill404JSONResponse(
				errorBody("not_found", "skill not found", map[string]any{"name": request.Name})), nil
		}
		return apigen.DeleteSkill400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	return apigen.DeleteSkill200JSONResponse{Message: "deleted " + request.Name}, nil
}
