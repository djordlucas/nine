package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"nine/internal/api/apigen"
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

// GetConversationHistory is not implemented: see the detail below.
func (s *Server) GetConversationHistory(ctx context.Context, request apigen.GetConversationHistoryRequestObject) (apigen.GetConversationHistoryResponseObject, error) {
	return apigen.GetConversationHistory501JSONResponse(notImplementedBody(
		"the wire protocol exposes no journal query; attach carries a " +
			"transcript but registers the caller as attached, which a read must not do")), nil
}

// GetConversationTrace is not implemented: see the detail below.
func (s *Server) GetConversationTrace(ctx context.Context, request apigen.GetConversationTraceRequestObject) (apigen.GetConversationTraceResponseObject, error) {
	return apigen.GetConversationTrace501JSONResponse(notImplementedBody(
		"the wire protocol exposes no per-turn trace; `nine trace` reads the " +
			"memory store directly, which the API process must not do (API-A-1)")), nil
}

// ReplayTurn is not implemented: see the detail below.
func (s *Server) ReplayTurn(ctx context.Context, request apigen.ReplayTurnRequestObject) (apigen.ReplayTurnResponseObject, error) {
	return apigen.ReplayTurn501JSONResponse(notImplementedBody(
		"the wire protocol exposes no replay message")), nil
}

// StreamMessages opens the SSE stream.
//
// The generated response object for an event stream carries an io.Reader and a
// content length, which suits a finite body and not a live stream: the visit
// would buffer and set Content-Length, defeating per-event flushing. The visit
// interface is exported, so sseStream below implements it and writes to the
// ResponseWriter directly, keeping this operation inside the generated routing
// rather than bypassing it.
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
	// Closed by the stream once the client disconnects, not here: the
	// connection has to outlive this function.
	return sseStream{agentID: request.Id, closer: cl.Close, ctx: ctx}, nil
}

// sseStream writes the event stream for one attached client.
type sseStream struct {
	agentID string
	closer  func() error
	ctx     context.Context
}

func (st sseStream) VisitStreamMessagesResponse(w http.ResponseWriter) error {
	defer st.closer() //nolint:errcheck

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// Marshal the payload rather than interpolating the id, which arrives from
	// the request path.
	payload, err := json.Marshal(apigen.StreamConnected{
		Message: "stream connected",
		AgentId: st.agentID,
	})
	if err != nil {
		return fmt.Errorf("marshal connection event: %w", err)
	}
	fmt.Fprintf(w, "event: connected\ndata: %s\n\n", payload)

	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// TODO: forward the daemon's progress events. The socket carries them for
	// an attached turn (protocol.TurnWithProgress), but nothing yet bridges
	// them onto this stream, so a client sees the handshake and then silence.
	<-st.ctx.Done()
	return nil
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
