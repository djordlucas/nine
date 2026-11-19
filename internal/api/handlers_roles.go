package api

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
	"nine/skills"
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
			Name:        r.Name,
			AllTools:    r.AllTools,
			Delegates:   &r.Delegates,
			Leaf:        &r.Leaf,
			SpawnsGoals: &r.SpawnsGoals,
			Persists:    &r.Persists,
			Interactive: &r.Interactive,
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

// --- operator role writes (adr/agents-concept.md A8, Q23) ---
//
// A role is a catalog skill carrying a `role:` frontmatter block
// (docs/roles.md R-ROLE.1), so the REST role endpoints compose that Markdown
// and go through the same skill store writes as everything else — one store,
// two front doors. The daemon enforces source ownership (builtin/user roles
// are read-only); its refusal maps to 400.

// renderRoleSkill composes the Markdown document of a role skill. The body is
// the role's persona (R-ROLE.3): the description doubles as it.
func renderRoleSkill(name, description string, allTools bool, tools []string, delegates, spawnsGoals, persists, interactive bool) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + name + "\n")
	if description != "" {
		b.WriteString("description: " + description + "\n")
	}
	b.WriteString("tags: [role]\n")
	b.WriteString("role:\n")
	if allTools {
		b.WriteString("  tools: \"*\"\n")
	} else if len(tools) > 0 {
		b.WriteString("  tools: [" + strings.Join(tools, ", ") + "]\n")
	} else {
		b.WriteString("  tools: []\n")
	}
	fmt.Fprintf(&b, "  delegates: %t\n", delegates)
	fmt.Fprintf(&b, "  spawns_goals: %t\n", spawnsGoals)
	fmt.Fprintf(&b, "  persists: %t\n", persists)
	fmt.Fprintf(&b, "  interactive: %t\n", interactive)
	b.WriteString("---\n")
	if description != "" {
		b.WriteString(description + "\n")
	} else {
		b.WriteString("Role " + name + ".\n")
	}
	return b.String()
}

// roleInfoOf renders one wire RoleInfo as the API's RoleInfo.
func roleInfoOf(r protocol.RoleInfo) apigen.RoleInfo {
	ri := apigen.RoleInfo{
		Name:        r.Name,
		AllTools:    r.AllTools,
		Delegates:   &r.Delegates,
		Leaf:        &r.Leaf,
		SpawnsGoals: &r.SpawnsGoals,
		Persists:    &r.Persists,
		Interactive: &r.Interactive,
	}
	if r.Description != "" {
		ri.Description = &r.Description
	}
	if len(r.Tools) > 0 {
		ri.Tools = &r.Tools
	}
	return ri
}

// findRole returns the registry entry for name, or a 404-mapped error.
func findRole(cl *protocol.Client, name string) (protocol.RoleInfo, error) {
	roles, err := cl.RolesList()
	if err != nil {
		return protocol.RoleInfo{}, err
	}
	for _, r := range roles {
		if r.Name == name {
			return r, nil
		}
	}
	return protocol.RoleInfo{}, errors.New("role not found: " + name)
}

func (s *Server) CreateRole(ctx context.Context, request apigen.CreateRoleRequestObject) (apigen.CreateRoleResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.CreateRole503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	if request.Body == nil || strings.TrimSpace(request.Body.Name) == "" {
		return apigen.CreateRole400JSONResponse(
			errorBody("invalid_request", "name is required", nil)), nil
	}
	body := request.Body
	allTools := body.AllTools != nil && *body.AllTools
	tools := []string{}
	if body.Tools != nil {
		tools = *body.Tools
	}
	if !allTools && len(tools) == 0 {
		return apigen.CreateRole400JSONResponse(
			errorBody("invalid_request", "a role needs all_tools or a non-empty tools allowlist", nil)), nil
	}
	desc := ""
	if body.Description != nil {
		desc = *body.Description
	}
	name := strings.TrimSpace(body.Name)
	content := renderRoleSkill(name, desc, allTools, tools,
		derefBool(body.Delegates), derefBool(body.SpawnsGoals), derefBool(body.Persists), derefBool(body.Interactive))
	if _, err := cl.SkillCreate(name, desc, []string{"role"}, content); err != nil {
		return apigen.CreateRole400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	role, err := findRole(cl, name)
	if err != nil {
		return apigen.CreateRole500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	return apigen.CreateRole200JSONResponse(roleInfoOf(role)), nil
}

func (s *Server) UpdateRole(ctx context.Context, request apigen.UpdateRoleRequestObject) (apigen.UpdateRoleResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.UpdateRole503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	// The role lives in its skill's frontmatter: read, patch, re-render.
	sk, err := cl.GetSkill(request.Name)
	if err != nil {
		if errNotFound(err) {
			return apigen.UpdateRole404JSONResponse(
				errorBody("not_found", "role not found", map[string]any{"name": request.Name})), nil
		}
		return apigen.UpdateRole500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	parsed := skills.Parse(sk.Content)
	if parsed.Role == nil {
		return apigen.UpdateRole400JSONResponse(
			errorBody("invalid_request", "skill "+request.Name+" has no role block — not a role", nil)), nil
	}
	role := parsed.Role
	desc := parsed.Description
	body := request.Body
	if body == nil {
		return apigen.UpdateRole400JSONResponse(
			errorBody("invalid_request", "nothing to update", map[string]any{"name": request.Name})), nil
	}
	if body.Description != nil {
		desc = *body.Description
	}
	if body.AllTools != nil {
		role.AllTools = *body.AllTools
	}
	if body.Tools != nil {
		role.Tools = *body.Tools
	}
	if body.Delegates != nil {
		role.Delegates = *body.Delegates
	}
	if body.SpawnsGoals != nil {
		role.SpawnsGoals = *body.SpawnsGoals
	}
	if body.Persists != nil {
		role.Persists = *body.Persists
	}
	if body.Interactive != nil {
		role.Interactive = *body.Interactive
	}
	content := renderRoleSkill(request.Name, desc, role.AllTools, role.Tools,
		role.Delegates, role.SpawnsGoals, role.Persists, role.Interactive)
	if _, err := cl.SkillUpdate(request.Name, map[string]any{"description": desc, "content": content}); err != nil {
		if errNotFound(err) {
			return apigen.UpdateRole404JSONResponse(
				errorBody("not_found", "role not found", map[string]any{"name": request.Name})), nil
		}
		return apigen.UpdateRole400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	updated, err := findRole(cl, request.Name)
	if err != nil {
		return apigen.UpdateRole500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	return apigen.UpdateRole200JSONResponse(roleInfoOf(updated)), nil
}

func (s *Server) DeleteRole(ctx context.Context, request apigen.DeleteRoleRequestObject) (apigen.DeleteRoleResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteRole503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	sk, err := cl.GetSkill(request.Name)
	if err != nil {
		if errNotFound(err) {
			return apigen.DeleteRole404JSONResponse(
				errorBody("not_found", "role not found", map[string]any{"name": request.Name})), nil
		}
		return apigen.DeleteRole500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	if skills.Parse(sk.Content).Role == nil {
		return apigen.DeleteRole400JSONResponse(
			errorBody("invalid_request", "skill "+request.Name+" has no role block — not a role", nil)), nil
	}
	if err := cl.SkillDelete(request.Name); err != nil {
		if errNotFound(err) {
			return apigen.DeleteRole404JSONResponse(
				errorBody("not_found", "role not found", map[string]any{"name": request.Name})), nil
		}
		return apigen.DeleteRole400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}
	return apigen.DeleteRole200JSONResponse{Message: "deleted " + request.Name}, nil
}
