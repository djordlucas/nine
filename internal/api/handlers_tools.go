package api

import (
	"context"
	"encoding/json"
	"strings"

	"nine/internal/api/apigen"
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

	tools, err := cl.ListTools()
	if err != nil {
		return apigen.ListTools500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	infos := make([]apigen.ToolInfo, 0, len(tools))
	for _, tool := range tools {
		infos = append(infos, toToolInfo(tool.Name, tool.Description, tool.Plugin))
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

	tools, err := cl.ListTools()
	if err != nil {
		return apigen.GetTool500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	for _, tool := range tools {
		if tool.Name != request.Name {
			continue
		}
		// ToolSummary carries no input schema or capabilities, so those stay
		// absent rather than being invented.
		return apigen.GetTool200JSONResponse{
			Name:        ptr(tool.Name),
			Description: ptr(tool.Description),
			Plugin:      ptr(tool.Plugin),
			Kind:        ptr("plugin"),
			Loaded:      ptr(true),
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
		info := toToolInfo(tool.Name, tool.Description, "")
		info.Kind = ptr("sandboxed")
		info.Loaded = ptr(tool.Loaded)
		if tool.Error != "" {
			info.Error = ptr(tool.Error)
		}
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
	return info
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
