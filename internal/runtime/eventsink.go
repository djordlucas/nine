package runtime

import (
	"log/slog"
	"sync/atomic"
	"time"

	"nine/internal/memory"
)

// EventSink persists session execution events to the durable journal
// (adr/event-log.md). For observability-tier events (the v1 taxonomy: turn
// boundaries, LLM request/response, tool calls, context updates, sub-agent
// lifecycle) Append is non-blocking and best-effort — it must never stall a
// turn — so a full buffer drops the event rather than blocking. Close drains
// any buffered events and stops the writer.
type EventSink interface {
	Append(ev memory.SessionEvent)
	Close() error
}

// sessionEventStore is the store subset the SQL sink needs. It keeps the sink
// testable with a fake and avoids depending on the full *memory.Store.
type sessionEventStore interface {
	SessionEventsAppend([]memory.SessionEvent) error
}

// NoopSink is an EventSink that discards every event. Used when no journal is
// configured (e.g. sub-agents and tests that don't assert on the journal).
type NoopSink struct{}

func (NoopSink) Append(memory.SessionEvent) {}
func (NoopSink) Close() error               { return nil }

const (
	sinkQueueCap  = 1024                   // buffered events before Append starts dropping
	sinkMaxBatch  = 64                     // max events flushed in one INSERT
	sinkFlushTick = 100 * time.Millisecond // max latency before a partial batch flushes
)

// sqlEventSink is an async, batched, best-effort EventSink backed by a
// sessionEventStore. A single writer goroutine coalesces events into batched
// inserts so persistence never blocks the worker goroutine (adr/event-log.md
// §7.4).
type sqlEventSink struct {
	store   sessionEventStore
	ch      chan memory.SessionEvent
	done    chan struct{}
	dropped atomic.Int64
	onFlush func() // called after each successful batch write; wakes subscribers
}

// NewSQLEventSink starts an async batched sink writing to store. onFlush (may be
// nil) is called after each batch is written, so journal subscribers can wake
// and drain — the in-process wake for the subscription primitive
// (adr/reactive-events.md §3). It is set at construction, before the writer
// goroutine starts, so it needs no synchronization.
func NewSQLEventSink(store sessionEventStore, onFlush func()) EventSink {
	s := &sqlEventSink{
		store:   store,
		ch:      make(chan memory.SessionEvent, sinkQueueCap),
		done:    make(chan struct{}),
		onFlush: onFlush,
	}
	go s.run()
	return s
}

// Append enqueues ev for durable write. Non-blocking: if the buffer is full the
// event is dropped and counted (observability tier — a monitoring gap, never a
// turn stall).
func (s *sqlEventSink) Append(ev memory.SessionEvent) {
	select {
	case s.ch <- ev:
	default:
		s.dropped.Add(1)
	}
}

func (s *sqlEventSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(sinkFlushTick)
	defer ticker.Stop()

	batch := make([]memory.SessionEvent, 0, sinkMaxBatch)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.store.SessionEventsAppend(batch); err != nil {
			slog.Warn("session event append failed", "n", len(batch), "err", err)
		} else if s.onFlush != nil {
			s.onFlush()
		}
		batch = batch[:0]
	}

	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= sinkMaxBatch {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

// Close stops accepting events, drains and flushes what is buffered, and waits
// for the writer to exit. After Close, Append must not be called.
func (s *sqlEventSink) Close() error {
	close(s.ch)
	<-s.done
	if d := s.dropped.Load(); d > 0 {
		slog.Warn("session event sink dropped events under load", "dropped", d)
	}
	return nil
}
