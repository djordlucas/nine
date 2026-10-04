package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// Conversation endpoints. Each method implements one operation of
// apigen.StrictServerInterface, generated from internal/api/openapi.yaml.

func (s *Server) ListConversations(ctx context.Context, request apigen.ListConversationsRequestObject) (apigen.ListConversationsResponseObject, error) {
	p, err := page(request.Params.Limit, request.Params.Offset)
	if err != nil {
		return apigen.ListConversations400JSONResponse(
			errorBody("invalid_request", err.Error(), nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListConversations503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	sessions, err := cl.ListSessions()
	if err != nil {
		return apigen.ListConversations500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	conversations := make([]apigen.ConversationInfo, 0, len(sessions))
	for _, sess := range sessions {
		// SessionInfo carries no role, plan mode or timestamps, so those
		// fields stay absent rather than being filled with zero values that
		// serialise as a real date.
		conversations = append(conversations, apigen.ConversationInfo{
			Id:          ptr(sess.ID),
			Name:        ptr(sess.Name),
			Status:      ptr(sess.Status),
			EventsCount: ptr(sess.Events),
			Protected:   ptr(sess.Protected),
			Attached:    ptr(sess.Attached),
			AgeSeconds:  ptr(sess.AgeSeconds),
		})
	}

	data, pagination := paginate(conversations, p)
	return apigen.ListConversations200JSONResponse{
		Data:       &data,
		Pagination: &pagination,
	}, nil
}

func (s *Server) CreateConversation(ctx context.Context, request apigen.CreateConversationRequestObject) (apigen.CreateConversationResponseObject, error) {
	if request.Body == nil {
		return apigen.CreateConversation400JSONResponse(
			errorBody("invalid_request", "request body is required", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.CreateConversation503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	id, role, instanceName, err := cl.NewConversationInteractive(derefBool(request.Body.Interactive))
	if err != nil {
		return apigen.CreateConversation500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.CreateConversation201JSONResponse{
		Id:           id,
		Role:         ptr(role),
		InstanceName: ptr(instanceName),
		CreatedAt:    ptr(time.Now().UTC()),
	}, nil
}

func (s *Server) GetConversation(ctx context.Context, request apigen.GetConversationRequestObject) (apigen.GetConversationResponseObject, error) {
	if request.Id == "" {
		return apigen.GetConversation400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetConversation503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	ctxJSON, err := cl.Context(request.Id)
	if err != nil {
		if errNotFound(err) {
			return apigen.GetConversation404JSONResponse(
				errorBody("not_found", "conversation not found",
					map[string]any{"id": request.Id})), nil
		}
		return apigen.GetConversation500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.GetConversation200JSONResponse{
		Id:      ptr(request.Id),
		Context: ptr(decodeContext(ctxJSON)),
	}, nil
}

func (s *Server) DeleteConversation(ctx context.Context, request apigen.DeleteConversationRequestObject) (apigen.DeleteConversationResponseObject, error) {
	if request.Id == "" {
		return apigen.DeleteConversation400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.DeleteConversation503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msg, err := cl.DeleteSession(request.Id)
	if err != nil {
		if errNotFound(err) {
			return apigen.DeleteConversation404JSONResponse(
				errorBody("not_found", "conversation not found",
					map[string]any{"id": request.Id})), nil
		}
		return apigen.DeleteConversation500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.DeleteConversation200JSONResponse{
		Id:      ptr(request.Id),
		Message: ptr(msg),
	}, nil
}

func (s *Server) StopConversation(ctx context.Context, request apigen.StopConversationRequestObject) (apigen.StopConversationResponseObject, error) {
	if request.Id == "" {
		return apigen.StopConversation400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.StopConversation503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	msg, err := cl.StopSession(request.Id, false)
	if err != nil {
		if errNotFound(err) {
			return apigen.StopConversation404JSONResponse(
				errorBody("not_found", "conversation not found",
					map[string]any{"id": request.Id})), nil
		}
		return apigen.StopConversation500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.StopConversation200JSONResponse{
		Id:      ptr(request.Id),
		Message: ptr(msg),
		Status:  ptr("stopped"),
	}, nil
}

func (s *Server) SendMessage(ctx context.Context, request apigen.SendMessageRequestObject) (apigen.SendMessageResponseObject, error) {
	if request.Id == "" {
		return apigen.SendMessage400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}
	if request.Body == nil || request.Body.Text == "" {
		return apigen.SendMessage400JSONResponse(
			errorBody("invalid_request", "message text is required", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.SendMessage503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	reply, err := cl.Turn(request.Id, request.Body.Text)
	if err != nil {
		if errNotFound(err) {
			return apigen.SendMessage404JSONResponse(
				errorBody("not_found", "conversation not found",
					map[string]any{"id": request.Id})), nil
		}
		return apigen.SendMessage500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.SendMessage200JSONResponse{
		AgentId:     ptr(request.Id),
		Text:        ptr(reply),
		CompletedAt: ptr(time.Now().UTC()),
	}, nil
}

func (s *Server) GetConversationContext(ctx context.Context, request apigen.GetConversationContextRequestObject) (apigen.GetConversationContextResponseObject, error) {
	if request.Id == "" {
		return apigen.GetConversationContext400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.GetConversationContext503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	ctxJSON, err := cl.Context(request.Id)
	if err != nil {
		if errNotFound(err) {
			return apigen.GetConversationContext404JSONResponse(
				errorBody("not_found", "conversation not found",
					map[string]any{"id": request.Id})), nil
		}
		return apigen.GetConversationContext500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}

	return apigen.GetConversationContext200JSONResponse{
		AgentId: ptr(request.Id),
		Context: ptr(decodeContext(ctxJSON)),
	}, nil
}

// StreamMessages opens the SSE stream.
//
// The generated response object for an event stream carries an io.Reader and a
// content length, which suits a finite body and not a live stream: the visit
// would buffer and set Content-Length, defeating per-event flushing. The visit
// interface is exported, so sseStream below implements it and writes to the
// ResponseWriter directly, keeping this operation inside the generated routing
// rather than bypassing it.
//
// The daemon accepts the watch before any status is written, so an unknown
// conversation is a 404 rather than a 200 followed by silence.
func (s *Server) StreamMessages(ctx context.Context, request apigen.StreamMessagesRequestObject) (apigen.StreamMessagesResponseObject, error) {
	if request.Id == "" {
		return apigen.StreamMessages400JSONResponse(
			errorBody("invalid_request", "missing conversation id", nil)), nil
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.StreamMessages503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	if err := cl.Watch(request.Id); err != nil {
		cl.Close() //nolint:errcheck
		if errNotFound(err) {
			return apigen.StreamMessages404JSONResponse(
				errorBody("not_found", "conversation not found", map[string]any{"id": request.Id})), nil
		}
		return apigen.StreamMessages500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	// Closed by the stream once the client disconnects, not here: the
	// connection has to outlive this function.
	return sseStream{agentID: request.Id, events: cl, ctx: ctx, keepalive: sseKeepalive}, nil
}

// sseKeepalive is how often an idle stream writes an SSE comment. A session can
// sit between turns for minutes, and proxies close a connection that has been
// silent that long; a comment line keeps it open and is ignored by clients.
const sseKeepalive = 15 * time.Second

// watchFeed is the daemon side of a stream: a watched connection.
type watchFeed interface {
	NextEvent() (protocol.Msg, error)
	Close() error
}

// sseStream writes the event stream for one attached client.
type sseStream struct {
	agentID   string
	events    watchFeed
	ctx       context.Context
	keepalive time.Duration
}

func (st sseStream) VisitStreamMessagesResponse(w http.ResponseWriter) error {
	defer st.events.Close() //nolint:errcheck

	// The server's WriteTimeout bounds a whole response, which for a stream
	// would end it after the timeout however live it is. Lift the deadline for
	// this response only; the keepalive below is what detects a dead client.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("event stream: cannot lift write deadline", "err", err)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flush := func() { rc.Flush() } //nolint:errcheck // a failed flush shows up as the next write's error

	if err := writeSSE(w, "connected", apigen.StreamConnected{
		Message: "stream connected",
		AgentId: st.agentID,
	}); err != nil {
		return err
	}
	flush()

	// NextEvent blocks on the socket, so it runs on its own goroutine; closing
	// the feed (the deferred Close) is what unblocks it when the client leaves.
	type next struct {
		msg protocol.Msg
		err error
	}
	feed := make(chan next)
	go func() {
		for {
			m, err := st.events.NextEvent()
			select {
			case feed <- next{m, err}:
			case <-st.ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	tick := time.NewTicker(st.keepalive)
	defer tick.Stop()
	for {
		select {
		case <-st.ctx.Done():
			return nil
		case <-tick.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return nil
			}
			flush()
		case n := <-feed:
			if n.err != nil {
				// The daemon ended the watch (it shut down, or the connection
				// failed). Say so rather than leaving the client on a stream
				// that will never speak again.
				writeSSE(w, "error", errorBody("service_unavailable", //nolint:errcheck
					"daemon closed the stream: "+n.err.Error(), nil))
				flush()
				return nil
			}
			name, payload, ok := toStreamEvent(n.msg)
			if !ok {
				continue
			}
			if err := writeSSE(w, name, payload); err != nil {
				return nil
			}
			flush()
		}
	}
}

// writeSSE writes one event. The payload is marshalled rather than
// interpolated: it carries text from the model and the request path.
func writeSSE(w io.Writer, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", event, err)
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	return err
}

// toStreamEvent maps one daemon message to an SSE event name and payload. The
// daemon's other progress types — reasoning tokens, stage labels, context
// usage — are TUI rendering detail the API does not declare, so they are
// skipped rather than sent under names a client has no schema for.
func toStreamEvent(m protocol.Msg) (string, any, bool) {
	ts := m.Timestamp
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	switch m.Type {
	case protocol.TypeToolStart:
		e := apigen.StreamToolStart{ToolName: m.ToolName, Timestamp: ts}
		if len(m.ToolInput) > 0 {
			e.ToolInput = rawJSON(m.ToolInput)
		}
		return "tool_start", e, true
	case protocol.TypeToolEnd:
		return "tool_end", apigen.StreamToolEnd{ToolName: m.ToolName, ToolOutput: ptr(m.ToolOutput), Timestamp: ts}, true
	case protocol.TypeResponseChunk:
		return "response_chunk", apigen.StreamResponseChunk{Text: m.Text, Timestamp: ts}, true
	case protocol.TypeResponse:
		return "response", apigen.StreamResponse{AgentId: m.AgentID, Text: m.Text, Timestamp: ts}, true
	case protocol.TypeSubAgentStart, protocol.TypeSubAgentEnd:
		return string(m.Type), apigen.StreamSubAgent{
			SubAgentId: m.SubAgentID,
			Task:       nonEmpty(m.Text),
			Role:       nonEmpty(m.Role),
			Status:     nonEmpty(m.Status),
			Timestamp:  ts,
		}, true
	case protocol.TypeNotice:
		return "notice", apigen.StreamNotice{Text: m.Text, Timestamp: ts}, true
	case protocol.TypeDone:
		return "done", apigen.StreamDone{AgentId: m.AgentID, Timestamp: ts}, true
	case protocol.TypeError:
		return "error", errorBody("server_error", m.Text, nil), true
	}
	return "", nil, false
}

// decodeContext parses the daemon's context payload. A payload that will not
// parse is reported in place rather than failing the request: the caller still
// learns the conversation exists, and sees why the breakdown is missing.
func decodeContext(raw string) map[string]any {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		slog.Warn("failed to parse context JSON", "err", err, "raw_length", len(raw))
		return map[string]any{
			"error": "failed to parse context",
			"raw":   raw,
		}
	}
	return parsed
}

// ptr returns a pointer to v, for the optional fields the generator emits.
func ptr[T any](v T) *T { return &v }
