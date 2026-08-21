// Package subscribe drives programmatic subscribers over the durable session
// event journal (adr/reactive-events.md). A Subscription reads events forward
// from a subscriber's persisted cursor, delivers each to its Handler, and
// advances the cursor — so a subscriber resumes where it left off after a daemon
// restart (catch-up) and never loses the window. Delivery is at-least-once:
// handlers must be idempotent, because a crash between handling an event and
// persisting the cursor can redeliver it.
//
// Subscribers run on their own goroutine, off the turn path — a slow or failing
// handler can never stall a user turn (the out-of-band guarantee, §1a).
package subscribe

import (
	"context"
	"log/slog"
	"time"

	"nine/internal/memory"
)

// Handler consumes journal events. Handle must be idempotent (see package doc).
type Handler interface {
	// ID is the stable subscriber identity used as the durable cursor key.
	ID() string
	// Types is the set of event types to receive; an empty slice receives all.
	Types() []string
	// Handle processes one event. A returned error is logged and skipped — a
	// poison event must not wedge the cursor — so recovery is the handler's job.
	Handle(ctx context.Context, ev memory.SessionEvent) error
}

// Store is the journal subset a Subscription needs.
type Store interface {
	SessionEventsAfter(afterSeq int64, limit int) ([]memory.SessionEvent, error)
	EventCursorGet(subscriberID string) (int64, error)
	EventCursorSet(subscriberID string, seq int64) error
}

const (
	batchSize    = 256             // events read per drain
	pollInterval = 2 * time.Second // fallback wake when Notify isn't wired
)

// Subscription drives a single Handler over the journal from its durable cursor.
type Subscription struct {
	store Store
	h     Handler
	types map[string]bool // nil = accept all
	wake  chan struct{}
}

// New creates a Subscription for h backed by store.
func New(store Store, h Handler) *Subscription {
	var types map[string]bool
	if t := h.Types(); len(t) > 0 {
		types = make(map[string]bool, len(t))
		for _, ty := range t {
			types[ty] = true
		}
	}
	return &Subscription{
		store: store,
		h:     h,
		types: types,
		wake:  make(chan struct{}, 1),
	}
}

// Notify wakes the subscription to drain immediately. Non-blocking: it is an
// in-process low-latency hint (the durable cursor is the source of truth, so a
// missed notify only delays delivery to the next poll). Safe from any goroutine.
func (s *Subscription) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run consumes events until ctx is cancelled. On start it resumes from the
// handler's persisted cursor, then drains on each wake or poll tick.
func (s *Subscription) Run(ctx context.Context) {
	cursor, err := s.store.EventCursorGet(s.h.ID())
	if err != nil {
		slog.Warn("subscriber cursor load failed", "subscriber", s.h.ID(), "err", err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		cursor = s.drain(ctx, cursor)
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-ticker.C:
		}
	}
}

// drain delivers every event past cursor to the handler, advancing and
// persisting the cursor as it goes, until the journal is exhausted. It returns
// the new cursor.
func (s *Subscription) drain(ctx context.Context, cursor int64) int64 {
	for {
		if ctx.Err() != nil {
			return cursor
		}
		evs, err := s.store.SessionEventsAfter(cursor, batchSize)
		if err != nil {
			slog.Warn("subscriber read failed", "subscriber", s.h.ID(), "err", err)
			return cursor
		}
		if len(evs) == 0 {
			return cursor
		}
		for _, e := range evs {
			if s.wants(e.Type) {
				if herr := s.h.Handle(ctx, e); herr != nil {
					slog.Warn("subscriber handle failed (skipping)",
						"subscriber", s.h.ID(), "seq", e.Seq, "type", e.Type, "err", herr)
				}
			}
			cursor = e.Seq
		}
		if err := s.store.EventCursorSet(s.h.ID(), cursor); err != nil {
			slog.Warn("subscriber cursor save failed", "subscriber", s.h.ID(), "err", err)
			return cursor
		}
		if len(evs) < batchSize {
			return cursor
		}
	}
}

func (s *Subscription) wants(eventType string) bool {
	return s.types == nil || s.types[eventType]
}
