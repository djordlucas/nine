package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// The WebSocket transport (spec/contracts/api.md API-STREAM-3) carries the same
// events as the SSE stream, as JSON text messages with a `type` field, and adds
// the direction SSE lacks: a client sends turns, and answers an interactive
// session's questions, over the connection it is watching.
//
// It lives outside the OpenAPI document, which cannot describe a WebSocket, and
// outside /api/v1, as the contract specifies. It is mounted on the same mux, so
// authentication, rate limiting and request logging apply to the upgrade
// request exactly as they do to every other route.

// wsPath is the WebSocket route for one conversation.
const wsPath = "/ws/v1/conversations/{id}/messages"

// Tuning for one WebSocket session. Variables rather than constants so tests
// can shorten them.
var (
	// wsPingInterval is how often the server pings. A peer that stops answering
	// is closed; it also keeps an idle connection open through proxies.
	wsPingInterval = 30 * time.Second
	// wsWriteTimeout bounds one write. A client that stops reading is closed
	// rather than allowed to stall the daemon feed behind it.
	wsWriteTimeout = 10 * time.Second
)

// wsReadLimit bounds one client message. A turn's text is the largest thing a
// client sends; 1 MiB is far above any prompt and far below a memory problem.
const wsReadLimit = 1 << 20

// wsClientMessage is everything a client may send. Type selects which fields
// apply.
type wsClientMessage struct {
	Type string `json:"type"`
	// user_turn
	Text       string `json:"text"`
	ForceThink bool   `json:"force_think"`
	// human_input_answer
	RequestID string `json:"request_id"`
	Answer    string `json:"answer"`
}

// handleWebSocket upgrades a request to a WebSocket that follows one
// conversation.
//
// The daemon accepts the watch before the upgrade, so an unknown conversation
// is an ordinary 404 response rather than a connection that opens and then
// closes — the same guarantee the SSE stream gives.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "missing conversation id", nil)
		return
	}

	feed, err := s.getDaemonClient()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable", err.Error(), nil)
		return
	}
	if err := feed.Watch(id); err != nil {
		feed.Close() //nolint:errcheck
		if errNotFound(err) {
			writeError(w, http.StatusNotFound, "not_found", "conversation not found", map[string]any{"id": id})
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error", err.Error(), nil)
		return
	}

	// The server's read and write timeouts do not end the connection: the
	// hijacked socket keeps their deadlines, but the library replaces them on
	// every read and write with the deadline of that call's context. The ping
	// below is what detects a dead peer.
	conn, err := websocket.Accept(w, r, s.wsAcceptOptions())
	if err != nil {
		// Accept has already written the refusal (a bad handshake, or an
		// origin the CORS configuration does not allow).
		feed.Close() //nolint:errcheck
		return
	}
	conn.SetReadLimit(wsReadLimit)

	ws := &wsSession{server: s, agentID: id, conn: conn, feed: feed}
	ws.run(r.Context())
}

// wsAcceptOptions derives origin checking from the CORS configuration, so the
// two cannot disagree about which browser origins may reach the API. A request
// with no Origin header — any non-browser client — is not origin-checked.
func (s *Server) wsAcceptOptions() *websocket.AcceptOptions {
	opts := &websocket.AcceptOptions{}
	for _, o := range s.config.GetCORSOrigins() {
		if o == "*" {
			opts.InsecureSkipVerify = true
			return opts
		}
		// An origin is configured as scheme://host[:port]; the library matches
		// the host part.
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			opts.OriginPatterns = append(opts.OriginPatterns, u.Host)
		} else {
			opts.OriginPatterns = append(opts.OriginPatterns, o)
		}
	}
	return opts
}

// wsSession is one open WebSocket and the daemon watch behind it.
type wsSession struct {
	server  *Server
	agentID string
	conn    *websocket.Conn
	feed    watchFeed

	// turns tracks in-flight turn submissions so run can wait for them.
	turns sync.WaitGroup
}

// run serves the connection until the client leaves, the daemon ends the
// watch, or a ping or write fails.
func (ws *wsSession) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer func() {
		cancel()
		ws.feed.Close() //nolint:errcheck
		ws.turns.Wait()
		ws.conn.Close(websocket.StatusNormalClosure, "") //nolint:errcheck
	}()

	ws.send(ctx, "connected", apigen.StreamConnected{Message: "stream connected", AgentId: ws.agentID})

	go ws.forwardFeed(ctx, cancel)
	go ws.ping(ctx, cancel)

	for {
		typ, data, err := ws.conn.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			ws.sendError(ctx, "invalid_request", "messages must be JSON text")
			continue
		}
		var msg wsClientMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			ws.sendError(ctx, "invalid_request", "invalid JSON: "+err.Error())
			continue
		}
		ws.handle(ctx, msg)
	}
}

// forwardFeed relays the daemon's watch feed to the client. When the daemon
// ends it the client is told why and the connection is closed, rather than left
// open on a feed that will never speak again.
func (ws *wsSession) forwardFeed(ctx context.Context, cancel context.CancelFunc) {
	defer cancel()
	for {
		m, err := ws.feed.NextEvent()
		if err != nil {
			if ctx.Err() == nil {
				ws.sendError(ctx, "service_unavailable", "daemon closed the stream: "+err.Error())
				ws.conn.Close(websocket.StatusGoingAway, "daemon closed the stream") //nolint:errcheck
			}
			return
		}
		if name, payload, ok := toWSEvent(m); ok {
			if !ws.send(ctx, name, payload) {
				return
			}
		}
	}
}

func (ws *wsSession) ping(ctx context.Context, cancel context.CancelFunc) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, done := context.WithTimeout(ctx, wsWriteTimeout)
			err := ws.conn.Ping(pctx)
			done()
			if err != nil {
				cancel()
				return
			}
		}
	}
}

// handle acts on one client message.
func (ws *wsSession) handle(ctx context.Context, msg wsClientMessage) {
	switch msg.Type {
	case "user_turn":
		if strings.TrimSpace(msg.Text) == "" {
			ws.sendError(ctx, "invalid_request", "user_turn requires text")
			return
		}
		ws.turns.Add(1)
		go func() {
			defer ws.turns.Done()
			ws.submitTurn(ctx, msg.Text, msg.ForceThink)
		}()

	case "human_input_answer":
		if msg.RequestID == "" {
			ws.sendError(ctx, "invalid_request", "human_input_answer requires request_id")
			return
		}
		cl, err := ws.server.getDaemonClient()
		if err != nil {
			ws.sendError(ctx, "service_unavailable", err.Error())
			return
		}
		defer cl.Close()
		if err := cl.AnswerHuman(ws.agentID, msg.RequestID, msg.Answer); err != nil {
			code := "server_error"
			if errNotFound(err) {
				code = "not_found"
			}
			ws.sendError(ctx, code, err.Error())
			return
		}
		ws.send(ctx, "human_input_answered", map[string]string{"request_id": msg.RequestID})

	default:
		ws.sendError(ctx, "invalid_request", "unknown message type "+strconvQuote(msg.Type)+
			"; expected user_turn or human_input_answer")
	}
}

// submitTurn runs one turn on its own daemon connection. The turn's events and
// outcome reach the client through the watch, like any other turn's, so only a
// submission that did not become a turn now — queued behind a running one, or
// refused — is reported here.
func (ws *wsSession) submitTurn(ctx context.Context, text string, forceThink bool) {
	cl, err := ws.server.getDaemonClient()
	if err != nil {
		ws.sendError(ctx, "service_unavailable", err.Error())
		return
	}
	// Closing the connection is what abandons a turn the client stopped waiting
	// for; the daemon still finishes it.
	stop := context.AfterFunc(ctx, func() { cl.Close() }) //nolint:errcheck
	defer func() {
		if stop() {
			cl.Close() //nolint:errcheck
		}
	}()

	outcome, detail, err := cl.SubmitTurn(ws.agentID, text, forceThink)
	switch {
	case err != nil:
		if ctx.Err() != nil {
			return
		}
		code := "server_error"
		if errNotFound(err) {
			code = "not_found"
		}
		ws.sendError(ctx, code, err.Error())
	case outcome == protocol.TurnQueued:
		ws.send(ctx, "queued", map[string]string{"text": detail})
	}
}

// send writes one event as a flat JSON object: the payload's fields with
// "type" added. It reports whether the write succeeded; a failed write closes
// the connection, since a client that cannot be written to is gone.
func (ws *wsSession) send(ctx context.Context, typ string, payload any) bool {
	b, err := wsEncode(typ, payload)
	if err != nil {
		slog.Warn("websocket: encode event", "type", typ, "err", err)
		return true
	}
	wctx, done := context.WithTimeout(ctx, wsWriteTimeout)
	defer done()
	if err := ws.conn.Write(wctx, websocket.MessageText, b); err != nil {
		if !errors.Is(err, context.Canceled) {
			ws.conn.Close(websocket.StatusPolicyViolation, "write failed") //nolint:errcheck
		}
		return false
	}
	return true
}

func (ws *wsSession) sendError(ctx context.Context, code, message string) {
	ws.send(ctx, "error", errorBody(code, message, nil))
}

// wsEncode flattens payload into an object carrying "type". The payloads are
// the SSE event schemas, so the two transports agree field for field; an error
// keeps its "error" envelope, giving {"type":"error","error":{...}}.
func wsEncode(typ string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	t, _ := json.Marshal(typ)
	fields["type"] = t
	return json.Marshal(fields)
}

// toWSEvent is toStreamEvent plus the one event only the WebSocket can act on:
// a question from an interactive session, which a client answers with
// human_input_answer on the same connection.
func toWSEvent(m protocol.Msg) (string, any, bool) {
	if m.Type == protocol.TypeHumanInputRequired {
		return "human_input_required", wsHumanInput{
			RequestID:      m.RequestID,
			Question:       m.Question,
			Options:        m.Options,
			TimeoutSeconds: m.TimeoutSeconds,
			Origin:         m.Origin,
		}, true
	}
	return toStreamEvent(m)
}

type wsHumanInput struct {
	RequestID      string   `json:"request_id"`
	Question       string   `json:"question"`
	Options        []string `json:"options,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	Origin         string   `json:"origin,omitempty"`
}

// strconvQuote quotes a client-supplied type for an error message, bounding its
// length so a large bogus field is not echoed back whole.
func strconvQuote(s string) string {
	const max = 64
	if len(s) > max {
		s = s[:max] + "…"
	}
	b, _ := json.Marshal(s)
	return string(b)
}
