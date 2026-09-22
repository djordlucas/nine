package api

import (
	"context"
	"io/fs"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/docindex"
	"nine/internal/protocol"
)

// Health, status, notifications, skills, session attach, and the bundled
// documentation and specification endpoints.

func (s *Server) GetHealth(ctx context.Context, request apigen.GetHealthRequestObject) (apigen.GetHealthResponseObject, error) {
	// The probe opens a real socket, so it has to be closed again: health is
	// the most frequently polled endpoint on the server, and leaking one
	// descriptor per call exhausts the process under any liveness probe.
	cl, err := s.getDaemonClient()
	daemonConnected := err == nil
	if daemonConnected {
		defer cl.Close()
	}

	status := "healthy"
	if !daemonConnected {
		status = "degraded"
	}

	return apigen.GetHealth200JSONResponse{
		Status:          status,
		DaemonConnected: daemonConnected,
		UptimeSeconds:   ptr(int(time.Since(s.startTime).Seconds())),
		Version:         s.version,
	}, nil
}

func (s *Server) GetStatus(ctx context.Context, request apigen.GetStatusRequestObject) (apigen.GetStatusResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetStatus503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	info, err := cl.Status()
	if err != nil {
		return apigen.GetStatus500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	// StatusInfo names plugins but carries no per-plugin detail, so each entry
	// reports only what the daemon actually said.
	plugins := make([]apigen.PluginInfo, 0, len(info.Plugins))
	for _, name := range info.Plugins {
		plugins = append(plugins, apigen.PluginInfo{Name: ptr(name), Loaded: ptr(true)})
	}

	return apigen.GetStatus200JSONResponse{
		Status:         ptr("healthy"),
		Version:        ptr(s.version),
		Uptime:         ptr(info.Uptime),
		ActiveSessions: ptr(len(info.Agents)),
		Plugins:        &plugins,
	}, nil
}

func (s *Server) ListNotifications(ctx context.Context, request apigen.ListNotificationsRequestObject) (apigen.ListNotificationsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListNotifications400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListNotifications503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	raw, err := cl.ListNotifications(derefBool(request.Params.All))
	if err != nil {
		return apigen.ListNotifications500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	rows, err := decodeList[wireNotification](raw, "notifications")
	if err != nil {
		return apigen.ListNotifications500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	notifications := make([]apigen.Notification, 0, len(rows))
	for _, row := range rows {
		notifications = append(notifications, toNotification(row))
	}

	data, pagination := paginate(notifications, p)
	return apigen.ListNotifications200JSONResponse{
		Data:       &data,
		Pagination: &pagination,
	}, nil
}

// ListSkills is not implemented: see the detail below.
func (s *Server) ListSkills(ctx context.Context, request apigen.ListSkillsRequestObject) (apigen.ListSkillsResponseObject, error) {
	return apigen.ListSkills501JSONResponse(notImplementedBody(
		"skills live in the memory store and the wire protocol exposes no skills query")), nil
}

func (s *Server) AttachSession(ctx context.Context, request apigen.AttachSessionRequestObject) (apigen.AttachSessionResponseObject, error) {
	if request.Body == nil || request.Body.AgentId == "" {
		return apigen.AttachSession400JSONResponse(
			errorBody("invalid_request", "agent_id is required", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.AttachSession503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	result, err := cl.Attach(request.Body.AgentId)
	if err != nil {
		if errNotFound(err) {
			return apigen.AttachSession404JSONResponse(
				errorBody("not_found", "session not found",
					map[string]any{"agent_id": request.Body.AgentId})), nil
		}
		return apigen.AttachSession500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.AttachSession200JSONResponse{
		AgentId:         ptr(result.AgentID),
		Name:            ptr(result.Name),
		Role:            ptr(result.Role),
		InstanceName:    ptr(result.InstanceName),
		PendingResponse: ptr(result.PendingResponse),
		ReplayEvents:    ptr(toAnySlice(result.ReplayEvents)),
		History:         ptr(toAnySlice(result.History)),
	}, nil
}

func (s *Server) ListDocs(ctx context.Context, request apigen.ListDocsRequestObject) (apigen.ListDocsResponseObject, error) {
	topics, err := topicNames(docindex.Docs())
	if err != nil {
		return apigen.ListDocs500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	return apigen.ListDocs200JSONResponse{Topics: topics}, nil
}

func (s *Server) GetDocs(ctx context.Context, request apigen.GetDocsRequestObject) (apigen.GetDocsResponseObject, error) {
	if request.Topic == "" {
		return apigen.GetDocs400JSONResponse(
			errorBody("invalid_request", "missing topic", nil)), nil
	}

	content, ok := readTopic(docindex.Docs(), request.Topic)
	if !ok {
		return apigen.GetDocs404JSONResponse(
			errorBody("not_found", "documentation topic not found",
				map[string]any{"topic": request.Topic})), nil
	}

	return apigen.GetDocs200JSONResponse{Topic: request.Topic, Content: content}, nil
}

func (s *Server) ListSpec(ctx context.Context, request apigen.ListSpecRequestObject) (apigen.ListSpecResponseObject, error) {
	topics, err := topicNames(docindex.Spec())
	if err != nil {
		return apigen.ListSpec500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	return apigen.ListSpec200JSONResponse{Topics: topics}, nil
}

func (s *Server) GetSpec(ctx context.Context, request apigen.GetSpecRequestObject) (apigen.GetSpecResponseObject, error) {
	if request.Topic == "" {
		return apigen.GetSpec400JSONResponse(
			errorBody("invalid_request", "missing topic", nil)), nil
	}

	content, ok := readTopic(docindex.Spec(), request.Topic)
	if !ok {
		return apigen.GetSpec404JSONResponse(
			errorBody("not_found", "specification topic not found",
				map[string]any{"topic": request.Topic})), nil
	}

	return apigen.GetSpec200JSONResponse{Topic: request.Topic, Content: content}, nil
}

// topicNames lists a bundle's addressable topic names. Both bundles are
// embedded at build time, so this needs no daemon.
func topicNames(bundle docindex.Bundle) ([]string, error) {
	topics, err := bundle.Topics()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(topics))
	for _, t := range topics {
		names = append(names, t.Name)
	}
	return names, nil
}

// readTopic resolves a topic name to its Markdown body. Resolve accepts both
// the short name and an explicit relative path such as "contracts/api".
func readTopic(bundle docindex.Bundle, topic string) (string, bool) {
	path, ok := bundle.Resolve(topic)
	if !ok {
		return "", false
	}
	body, err := fs.ReadFile(bundle.FS, path)
	if err != nil {
		return "", false
	}
	return string(body), true
}

// toPluginInfo maps a daemon plugin roster entry to the API representation.
func toPluginInfo(p protocol.PluginStatus) apigen.PluginInfo {
	info := apigen.PluginInfo{
		Name:     ptr(p.Name),
		Loaded:   ptr(p.Loaded),
		Disabled: ptr(p.Disabled),
	}
	if p.Source != "" {
		info.Source = ptr(p.Source)
	}
	if len(p.Tools) > 0 {
		info.Tools = ptr(p.Tools)
	}
	if p.Error != "" {
		info.Error = ptr(p.Error)
	}
	return info
}

// toAnySlice widens a typed slice for the attach transcript, which the document
// types as opaque: the wire messages are a protocol detail the API does not
// re-describe.
func toAnySlice[T any](in []T) []any {
	out := make([]any, 0, len(in))
	for _, v := range in {
		out = append(out, v)
	}
	return out
}
