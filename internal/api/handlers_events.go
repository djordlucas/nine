package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nine/internal/api/apigen"
	"nine/internal/protocol"
)

// The daemon-wide event stream (spec/contracts/api.md API-HTTP-9): the
// journal, followed through the daemon (API-A-1 keeps this process out of the
// store), as Server-Sent Events whose ids are journal seqs.

// maxEventPayload is the largest payload the stream sends as journaled. An
// llm_request carries the whole context window; a phone following the journal
// should not, and the conversation's trace has it in full.
const maxEventPayload = 16 << 10

// maxEventTail mirrors the daemon's bound on a new follower's backlog.
const maxEventTail = 1000

func (s *Server) StreamEvents(ctx context.Context, request apigen.StreamEventsRequestObject) (apigen.StreamEventsResponseObject, error) {
	p := request.Params
	var after int64
	switch {
	case p.LastEventID != nil && strings.TrimSpace(*p.LastEventID) != "":
		n, err := strconv.ParseInt(strings.TrimSpace(*p.LastEventID), 10, 64)
		if err != nil || n < 0 {
			return apigen.StreamEvents400JSONResponse(
				errorBody("invalid_request", "Last-Event-ID must be a journal seq", nil)), nil
		}
		after = n
	case p.After != nil:
		if *p.After < 0 {
			return apigen.StreamEvents400JSONResponse(
				errorBody("invalid_request", "after must not be negative", nil)), nil
		}
		after = *p.After
	}
	tail := 0
	if p.Tail != nil {
		if *p.Tail < 0 || *p.Tail > maxEventTail {
			return apigen.StreamEvents400JSONResponse(
				errorBody("invalid_request", fmt.Sprintf("tail must be between 0 and %d", maxEventTail), nil)), nil
		}
		tail = *p.Tail
	}

	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.StreamEvents503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	start, reset, err := cl.FollowJournal(after, tail)
	if err != nil {
		cl.Close() //nolint:errcheck
		return apigen.StreamEvents500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	// Closed by the stream once the client disconnects, as for a
	// conversation's stream.
	return eventsStream{
		feed:      cl,
		ctx:       ctx,
		start:     start,
		reset:     reset,
		filter:    newEventFilter(listParam(p.Type), listParam(p.Agent)),
		keepalive: sseKeepalive,
	}, nil
}

func listParam(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// journalFeed is the daemon side of the stream: a followed connection.
type journalFeed interface {
	NextJournal() ([]protocol.JournalEvent, error)
	Close() error
}

// eventFilter keeps the events a client asked for. An empty list keeps all.
type eventFilter struct {
	types    map[string]bool
	prefixes []string
	agents   map[string]bool
}

func newEventFilter(types, agents []string) eventFilter {
	var f eventFilter
	for _, t := range types {
		if t = strings.TrimSpace(t); t == "" {
			continue
		}
		if prefix, ok := strings.CutSuffix(t, "*"); ok {
			f.prefixes = append(f.prefixes, prefix)
			continue
		}
		if f.types == nil {
			f.types = map[string]bool{}
		}
		f.types[t] = true
	}
	for _, a := range agents {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if f.agents == nil {
			f.agents = map[string]bool{}
		}
		f.agents[a] = true
	}
	return f
}

func (f eventFilter) keeps(e protocol.JournalEvent) bool {
	if f.agents != nil && !f.agents[e.AgentID] {
		return false
	}
	if f.types == nil && f.prefixes == nil {
		return true
	}
	if f.types[e.Type] {
		return true
	}
	for _, p := range f.prefixes {
		if strings.HasPrefix(e.Type, p) {
			return true
		}
	}
	return false
}

// toJournalEvent maps one journal row, capping its payload.
func toJournalEvent(e protocol.JournalEvent) apigen.JournalEvent {
	out := apigen.JournalEvent{
		Seq:          e.Seq,
		AgentId:      e.AgentID,
		Turn:         e.Turn,
		Type:         e.Type,
		SpanId:       nonEmpty(e.SpanID),
		ParentSpanId: nonEmpty(e.ParentSpanID),
		Timestamp:    e.TS.UTC(),
	}
	switch {
	case len(e.Payload) > maxEventPayload:
		out.Payload = map[string]any{"omitted": true, "bytes": len(e.Payload)}
	case len(e.Payload) > 0:
		out.Payload = rawJSON(e.Payload)
	}
	return out
}

// eventsStream writes the journal stream for one client.
type eventsStream struct {
	feed      journalFeed
	ctx       context.Context
	start     int64
	reset     bool
	filter    eventFilter
	keepalive time.Duration
}

func (st eventsStream) VisitStreamEventsResponse(w http.ResponseWriter) error {
	defer st.feed.Close() //nolint:errcheck

	// As for a conversation's stream: the server's WriteTimeout would end a
	// live stream, so it is lifted for this response; keepalives find a
	// client that has gone.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		slog.Debug("journal stream: cannot lift write deadline", "err", err)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flush := func() { rc.Flush() } //nolint:errcheck // a failed flush shows up as the next write's error

	// Every event carries an id, the handshake included, so a client that
	// drops before the first journal event still resumes from where it began.
	if err := writeSSEEvent(w, st.start, "connected", apigen.EventsConnected{Seq: st.start}); err != nil {
		return nil
	}
	if st.reset {
		if err := writeSSEEvent(w, st.start, "reset", apigen.EventsReset{Seq: st.start}); err != nil {
			return nil
		}
	}
	flush()

	type next struct {
		evs []protocol.JournalEvent
		err error
	}
	feed := make(chan next)
	go func() {
		for {
			evs, err := st.feed.NextJournal()
			select {
			case feed <- next{evs, err}:
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
				writeSSE(w, "error", errorBody("service_unavailable", //nolint:errcheck
					"daemon closed the stream: "+n.err.Error(), nil))
				flush()
				return nil
			}
			if len(n.evs) == 0 {
				continue
			}
			sent := int64(0)
			for _, e := range n.evs {
				if !st.filter.keeps(e) {
					continue
				}
				if err := writeSSEEvent(w, e.Seq, "journal", toJournalEvent(e)); err != nil {
					return nil
				}
				sent = e.Seq
			}
			// A batch whose last events were filtered out still moves the
			// client's position: an id with no data sets Last-Event-ID without
			// dispatching an event, so a narrow filter does not make a
			// reconnect replay what it already skipped.
			if last := n.evs[len(n.evs)-1].Seq; sent < last {
				if _, err := fmt.Fprintf(w, "id: %d\n\n", last); err != nil {
					return nil
				}
			}
			flush()
		}
	}
}

// writeSSEEvent writes one event with an id.
func writeSSEEvent(w io.Writer, id int64, event string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s event: %w", event, err)
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id, event, b)
	return err
}

// ListQuestions returns the ask_human questions waiting for an answer.
func (s *Server) ListQuestions(ctx context.Context, _ apigen.ListQuestionsRequestObject) (apigen.ListQuestionsResponseObject, error) {
	cl, err := s.getDaemonClient()
	if err != nil {
		return apigen.ListQuestions503JSONResponse(
			errorBody("service_unavailable", err.Error(), nil)), nil
	}
	defer cl.Close()

	qs, err := cl.ListHumanRequests()
	if err != nil {
		return apigen.ListQuestions500JSONResponse(
			errorBody("server_error", err.Error(), nil)), nil
	}
	data := make([]apigen.Question, len(qs))
	for i, q := range qs {
		data[i] = apigen.Question{
			Id:        q.ID,
			AgentId:   q.AgentID,
			Question:  q.Question,
			CreatedAt: q.CreatedAt,
			ExpiresAt: q.ExpiresAt.UTC(),
		}
		if len(q.Options) > 0 {
			data[i].Options = ptr(q.Options)
		}
	}
	return apigen.ListQuestions200JSONResponse{Data: data}, nil
}
