package runtime

import (
	"context"
	"encoding/json"
	"log/slog"

	"nine/internal/memory"
	"nine/internal/subscribe"
)

// EventKind identifies the type of supervisor event.
type EventKind string

const (
	EventAgentCompletes EventKind = "agent_completes"
	EventGoalStalls     EventKind = "goal_stalls"
	EventGapReported    EventKind = "gap_reported"
	EventPluginCrashed  EventKind = "plugin_crashed"
)

// supervisorEventType is the session_events type under which supervisor
// (control-plane) events are journaled. They carry the kind in span_id and the
// full detail in the payload, so they are durable, ordered, inspectable via
// `nine trace`, and consumable by the supervisor as a journal subscriber.
const supervisorEventType = "supervisor"

// supervisorSubscriberID is the durable cursor key for the supervisor's own
// consumption of its events.
const supervisorSubscriberID = "supervisor"

// Event is a notification posted to the supervisor.
type Event struct {
	Kind    EventKind
	AgentID string
	Payload string // free-form detail (description, error message, etc.)
}

// SupervisorStore is the journal surface the supervisor needs: durable append
// for the producer side, plus the cursor-based read surface (subscribe.Store)
// for the consumer side. *memory.Store satisfies it.
type SupervisorStore interface {
	SessionEventsAppend([]memory.SessionEvent) error
	subscribe.Store
}

// Supervisor receives control-plane events from agent runners and handles them.
// Events are durably journaled on Post (no longer dropped or lost on restart)
// and consumed via a cursor-backed subscription over that journal, so a reaction
// that was pending when the daemon stopped is picked up on the next boot.
type Supervisor struct {
	store SupervisorStore
	sub   *subscribe.Subscription
}

// NewSupervisor creates a Supervisor. bufSize is retained for call-site
// compatibility and no longer used — durability is the journal's, not an
// in-memory buffer's. Call Attach to wire the journal before Run/Post do
// anything; without it the supervisor is inert (used by wiring-only tests).
func NewSupervisor(bufSize int) *Supervisor {
	return &Supervisor{}
}

// Attach wires the journal the supervisor persists to and consumes from. Must be
// called before Run. Idempotent-safe to call once at startup.
func (s *Supervisor) Attach(store SupervisorStore) {
	s.store = store
	s.sub = subscribe.New(store, s)
}

// Post durably journals a control-plane event and wakes the consumer. Unlike the
// former in-memory bus it does not drop under load — a supervisor event is
// control-plane state, so it commits synchronously before returning. A nil store
// (unattached, tests) makes it a no-op.
func (s *Supervisor) Post(e Event) {
	if s.store == nil {
		return
	}
	payload, _ := json.Marshal(supervisorPayload(e))
	ev := memory.SessionEvent{
		AgentID: e.AgentID,
		Turn:    0,
		SpanID:  string(e.Kind),
		Type:    supervisorEventType,
		Payload: payload,
	}
	if err := s.store.SessionEventsAppend([]memory.SessionEvent{ev}); err != nil {
		slog.Warn("supervisor event persist failed", "kind", string(e.Kind), "agent_id", e.AgentID, "err", err)
		return
	}
	if s.sub != nil {
		s.sub.Notify()
	}
}

// supervisorPayload is the JSON body of a journaled supervisor event.
type supervisorPayload struct {
	Kind    EventKind `json:"kind"`
	AgentID string    `json:"agent_id,omitempty"`
	Payload string    `json:"payload,omitempty"`
}

// Run consumes journaled supervisor events until ctx is cancelled, resuming from
// the durable cursor on start. Call in a goroutine. A no-op if unattached.
func (s *Supervisor) Run(ctx context.Context) {
	if s.sub == nil {
		<-ctx.Done()
		return
	}
	s.sub.Run(ctx)
}

// --- subscribe.Handler: the supervisor consumes its own journaled events ---

func (s *Supervisor) ID() string      { return supervisorSubscriberID }
func (s *Supervisor) Types() []string { return []string{supervisorEventType} }

func (s *Supervisor) Handle(_ context.Context, ev memory.SessionEvent) error {
	var p supervisorPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		return err
	}
	s.handle(Event(p))
	return nil
}

func (s *Supervisor) handle(e Event) {
	slog.Info("supervisor event", "kind", string(e.Kind), "agent_id", e.AgentID, "payload", e.Payload)

	switch e.Kind {
	case EventGoalStalls, EventGapReported, EventAgentCompletes:
		// diagnose gap or surface to user (reactions plug in here)
	case EventPluginCrashed:
		// A plugin subprocess exited unexpectedly. Logged above; recovery
		// (restart from the existing binary) is left to the plugin manager.
	}
}
