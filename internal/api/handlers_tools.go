package api

import (
	"context"
	"encoding/json"
	"strings"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// Tool and plugin endpoints.

func (s *Server) ListTools(ctx context.Context, request apigen.ListToolsRequestObject) (apigen.ListToolsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListTools400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListTools503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	infos, err := toolRoster(cl)
	if err != nil {
		return apigen.ListTools500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	data, pagination := paginate(infos, p)
	return apigen.ListTools200JSONResponse{
		Data:       &data,
		Pagination: &pagination,
	}, nil
}

func (s *Server) GetTool(ctx context.Context, request apigen.GetToolRequestObject) (apigen.GetToolResponseObject, error) {
	if request.Name == "" {
		return apigen.GetTool400JSONResponse(
			errorBody("invalid_request", "missing tool name", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetTool503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	infos, err := toolRoster(cl)
	if err != nil {
		return apigen.GetTool500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	for _, t := range infos {
		if t.Name == nil || *t.Name != request.Name {
			continue
		}
		return apigen.GetTool200JSONResponse{
			Name:         t.Name,
			Description:  t.Description,
			Plugin:       t.Plugin,
			Kind:         t.Kind,
			Loaded:       t.Loaded,
			Generated:    t.Generated,
			Capabilities: t.Capabilities,
			InputSchema:  t.InputSchema,
			ManifestPath: t.ManifestPath,
			Deps:         t.Deps,
			Timeout:      t.Timeout,
		}, nil
	}

	return apigen.GetTool404JSONResponse(
		errorBody("not_found", "tool not found", map[string]any{"name": request.Name})), nil
}

func (s *Server) CallTool(ctx context.Context, request apigen.CallToolRequestObject) (apigen.CallToolResponseObject, error) {
	if request.Name == "" {
		return apigen.CallTool400JSONResponse(
			errorBody("invalid_request", "missing tool name", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.CallTool503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	// A tool's arguments are shaped by its own schema, so they pass through as
	// raw JSON. Absent arguments become a JSON null rather than empty bytes,
	// which the wire protocol would reject as malformed.
	argsRaw := json.RawMessage("null")
	if request.Body != nil && request.Body.Args != nil {
		encoded, err := json.Marshal(request.Body.Args)
		if err != nil {
			return apigen.CallTool400JSONResponse(
				errorBody("invalid_request", "invalid args format", nil)), nil
		}
		argsRaw = encoded
	}

	liveState := request.Body != nil && derefBool(request.Body.LiveState)

	output, err := cl.CallTool(request.Name, argsRaw, liveState)
	if err != nil {
		if errNotFound(err) {
			return apigen.CallTool404JSONResponse(
				errorBody("not_found", "tool not found",
					map[string]any{"name": request.Name})), nil
		}
		return apigen.CallTool500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.CallTool200JSONResponse{
		ToolName: ptr(request.Name),
		Output:   ptr(output),
		Success:  ptr(true),
	}, nil
}

func (s *Server) ReloadTools(ctx context.Context, request apigen.ReloadToolsRequestObject) (apigen.ReloadToolsResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ReloadTools503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	tools, err := cl.ReloadSandboxedTools()
	if err != nil {
		return apigen.ReloadTools500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	infos := make([]apigen.ToolInfo, 0, len(tools))
	for _, tool := range tools {
		info := toToolInfo(tool.Name, tool.Description, "sandboxed")
		applySandboxed(&info, tool)
		infos = append(infos, info)
	}

	return apigen.ReloadTools200JSONResponse{
		Message: ptr("sandboxed tools reloaded"),
		Tools:   &infos,
	}, nil
}

func (s *Server) ListPlugins(ctx context.Context, request apigen.ListPluginsRequestObject) (apigen.ListPluginsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListPlugins400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListPlugins503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	plugins, err := cl.ListPlugins()
	if err != nil {
		return apigen.ListPlugins500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	infos := make([]apigen.PluginInfo, 0, len(plugins))
	for _, plugin := range plugins {
		infos = append(infos, toPluginInfo(plugin))
	}

	data, pagination := paginate(infos, p)
	return apigen.ListPlugins200JSONResponse{
		Data:       &data,
		Pagination: &pagination,
	}, nil
}

func (s *Server) ReloadPlugins(ctx context.Context, request apigen.ReloadPluginsRequestObject) (apigen.ReloadPluginsResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ReloadPlugins503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	plugins, err := cl.ReloadPlugins()
	if err != nil {
		return apigen.ReloadPlugins500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	infos := make([]apigen.PluginInfo, 0, len(plugins))
	loaded, failed := 0, 0
	for _, plugin := range plugins {
		infos = append(infos, toPluginInfo(plugin))
		if plugin.Loaded {
			loaded++
		} else {
			failed++
		}
	}

	return apigen.ReloadPlugins200JSONResponse{
		Message:     ptr("plugins reloaded"),
		LoadedCount: ptr(loaded),
		FailedCount: ptr(failed),
		Plugins:     &infos,
	}, nil
}

// toToolInfo builds a tool summary. Capabilities and the input schema are not
// on the wire, so they are left absent.
func toToolInfo(name, description, plugin string) apigen.ToolInfo {
	info := apigen.ToolInfo{
		Name:        ptr(name),
		Description: ptr(description),
		Kind:        ptr("plugin"),
		Loaded:      ptr(true),
	}
	if plugin != "" {
		info.Plugin = ptr(plugin)
	}
	if plugin == "core" {
		info.Kind = ptr("core")
	}
	return info
}

// toolRoster is every tool the daemon has, as `nine tools` and the agent see
// them: the core and plugin tools with their schemas, and the sandboxed tools
// with where they came from, their capabilities and dependencies. A sandboxed
// tool that failed to load is listed too, with its error, though no agent can
// call it.
func toolRoster(cl *protocol.Client) ([]apigen.ToolInfo, error) {
	tools, err := cl.ListTools()
	if err != nil {
		return nil, err
	}
	boxed, err := cl.ListSandboxedTools()
	if err != nil {
		return nil, err
	}
	byName := make(map[string]protocol.SandboxedToolStatus, len(boxed))
	for _, b := range boxed {
		byName[b.Name] = b
	}
	seen := make(map[string]bool, len(tools))
	infos := make([]apigen.ToolInfo, 0, len(tools)+len(boxed))
	for _, t := range tools {
		info := toToolInfo(t.Name, t.Description, t.Plugin)
		if len(t.InputSchema) > 0 {
			var schema any
			if json.Unmarshal(t.InputSchema, &schema) == nil {
				info.InputSchema = schema
			}
		}
		if b, ok := byName[t.Name]; ok && t.Plugin == "sandboxed" {
			applySandboxed(&info, b)
		}
		seen[t.Name] = true
		infos = append(infos, info)
	}
	for _, b := range boxed {
		if seen[b.Name] {
			continue
		}
		info := toToolInfo(b.Name, b.Description, "sandboxed")
		applySandboxed(&info, b)
		infos = append(infos, info)
	}
	return infos, nil
}

// applySandboxed fills what the sandboxed roster knows of a tool.
func applySandboxed(info *apigen.ToolInfo, b protocol.SandboxedToolStatus) {
	kind := "sandboxed"
	if b.Generated {
		kind = "generated"
	}
	info.Kind = ptr(kind)
	info.Loaded = ptr(b.Loaded)
	info.Generated = ptr(b.Generated)
	if b.Description != "" {
		info.Description = ptr(b.Description)
	}
	if caps := splitCapabilities(b.Capabilities); len(caps) > 0 {
		info.Capabilities = &caps
	}
	if b.ManifestPath != "" {
		info.ManifestPath = ptr(b.ManifestPath)
	}
	if b.Error != "" {
		info.Error = ptr(b.Error)
	}
	if b.Timeout != "" {
		info.Timeout = ptr(b.Timeout)
	}
	if len(b.Deps) > 0 {
		deps := b.Deps
		info.Deps = &deps
	}
}

// splitCapabilities turns the roster's capability summary, "fs.read /work;
// net.http GET api.example.com", into one entry per capability. "none" is none.
func splitCapabilities(s string) []string {
	if s == "" || s == "none" {
		return nil
	}
	var out []string
	for _, c := range strings.Split(s, "; ") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// DeleteTool deletes a tool Nine wrote. The daemon decides what may be deleted;
// its refusal text says why, and is mapped to a status here.
func (s *Server) DeleteTool(ctx context.Context, request apigen.DeleteToolRequestObject) (apigen.DeleteToolResponseObject, error) {
	if request.Name == "" {
		return apigen.DeleteTool400JSONResponse(
			errorBody("invalid_request", "missing tool name", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteTool503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msg, err := cl.DeleteTool(request.Name)
	switch {
	case err == nil:
		return apigen.DeleteTool200JSONResponse{Name: request.Name, Message: msg}, nil
	case errNotFound(err):
		return apigen.DeleteTool404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"name": request.Name})), nil
	case strings.Contains(err.Error(), "cannot be deleted"):
		return apigen.DeleteTool403JSONResponse(
			errorBody("forbidden", err.Error(), map[string]any{"name": request.Name})), nil
	case strings.Contains(err.Error(), "tool writing is off"):
		return apigen.DeleteTool400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	default:
		return apigen.DeleteTool500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
}

// GetToolVersions lists a tool Nine wrote and the versions it keeps.
func (s *Server) GetToolVersions(ctx context.Context, request apigen.GetToolVersionsRequestObject) (apigen.GetToolVersionsResponseObject, error) {
	if request.Name == "" {
		return apigen.GetToolVersions400JSONResponse(errorBody("invalid_request", "missing tool name", nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetToolVersions503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	info, err := cl.ToolVersions(request.Name)
	switch {
	case err == nil:
	case errNotFound(err):
		return apigen.GetToolVersions404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"name": request.Name})), nil
	case strings.Contains(err.Error(), "tool writing is off"):
		return apigen.GetToolVersions400JSONResponse(errorBody("invalid_request", err.Error(), nil)), nil
	default:
		return apigen.GetToolVersions500JSONResponse(errorBody("server_error", err.Error(), nil)), nil
	}
	out := apigen.GetToolVersions200JSONResponse{
		Name: info.Name, Version: info.Version, Previous: []apigen.ToolVersion{},
		Probation: ptr(info.Probation), WrittenBy: ptr(info.WrittenBy), UpdatedAt: ptr(info.UpdatedAt),
	}
	for _, v := range info.Previous {
		out.Previous = append(out.Previous, apigen.ToolVersion{
			Version: v.Version, WrittenAt: v.WrittenAt, WrittenBy: ptr(v.WrittenBy), Lines: ptr(v.Lines),
		})
	}
	return out, nil
}

// RestoreToolVersion makes a kept version of a tool Nine wrote current again.
func (s *Server) RestoreToolVersion(ctx context.Context, request apigen.RestoreToolVersionRequestObject) (apigen.RestoreToolVersionResponseObject, error) {
	if request.Name == "" {
		return apigen.RestoreToolVersion400JSONResponse(errorBody("invalid_request", "missing tool name", nil)), nil
	}
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.RestoreToolVersion503JSONResponse(errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msg, err := cl.RestoreTool(request.Name, request.Version)
	switch {
	case err == nil:
		return apigen.RestoreToolVersion200JSONResponse{Name: request.Name, Message: msg}, nil
	case errNotFound(err) || strings.Contains(err.Error(), "has no version"):
		return apigen.RestoreToolVersion404JSONResponse(
			errorBody("not_found", err.Error(), map[string]any{"name": request.Name, "version": request.Version})), nil
	case strings.Contains(err.Error(), "tool writing is off"):
		return apigen.RestoreToolVersion400JSONResponse(errorBody("invalid_request", err.Error(), nil)), nil
	default:
		return apigen.RestoreToolVersion500JSONResponse(errorBody("server_error", err.Error(), nil)), nil
	}
}
