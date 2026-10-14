package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"sync"
	"time"

	"nine/internal/memory"
	"nine/internal/protocol"
)

// Following the journal (spec/contracts/wire-protocol.md, journal_follow).
//
// A follower reads the whole journal forward from a seq, the way a subscriber
// does, but for a client: the API server's GET /api/v1/events. It reads the
// store, not the sink's queue, so what it sends is exactly what trace and
// replay read later, and a follower that drops and returns with its last seq
// misses nothing still in the journal.

const (
	// followBatch is how many events one journal_follow message carries at most.
	followBatch = 256
	// followMaxTail bounds the backlog a new follower may ask for.
	followMaxTail = 1000
	// followMaxBacklog is how far behind a returning follower may resume. A
	// client further back reloads what it shows (a reset) rather than replaying
	// a day of journal it will mostly discard.
	followMaxBacklog = 10000
	// followPoll is the fallback wake when no journal write rang the bell, for
	// a store that was not given the hook.
	followPoll = time.Second
)

// bell wakes every goroutine waiting on it. A waiter takes the channel before
// it reads, then waits on it after, so a ring between the two is not lost.
type bell struct {
	mu sync.Mutex
	ch chan struct{}
}

func (b *bell) wait() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch == nil {
		b.ch = make(chan struct{})
	}
	return b.ch
}

func (b *bell) ring() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ch != nil {
		close(b.ch)
		b.ch = nil
	}
}

// JournalWritten wakes the journal's followers. The store calls it after every
// journal write (memory.Store.OnJournal); it never blocks and takes no daemon
// lock, so it is safe from inside any handler.
func (d *Daemon) JournalWritten() { d.journalBell.ring() }

// followStart decides where a follower's feed starts. after is the last seq the
// client has (0: none), tail the backlog a new client asks for; oldest and
// latest are the journal's bounds (memory.Store.SessionEventsBounds). reset is
// true when after cannot be honoured, and the feed then starts from now.
func followStart(after int64, tail int, oldest, latest int64) (start int64, reset bool) {
	if after <= 0 {
		tail = min(max(tail, 0), followMaxTail)
		start = max(latest-int64(tail), 0)
		if oldest > 0 {
			start = max(start, oldest-1)
		}
		return start, false
	}
	switch {
	case after > latest:
		// A seq this journal never assigned: another database, or one restored
		// from a backup.
		return latest, true
	case after < latest && (oldest == 0 || after < oldest-1):
		// Events the client has not seen were scrubbed.
		return latest, true
	case latest-after > followMaxBacklog:
		return latest, true
	}
	return after, false
}

// followJournal sends the journal to conn from where followStart says until
// the client hangs up or the daemon shuts down. It owns conn and closes it on
// return, as watch does.
func (d *Daemon) followJournal(ctx context.Context, conn net.Conn, enc *json.Encoder, after int64, tail int) {
	defer conn.Close() //nolint:errcheck

	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("journal not available")) //nolint:errcheck
		return
	}
	oldest, latest, err := d.store.SessionEventsBounds()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	cursor, reset := followStart(after, tail, oldest, latest)
	ack := protocol.Msg{Type: protocol.TypeJournalFollow, After: cursor}
	if reset {
		ack.Status = protocol.StatusJournalReset
	}
	if err := enc.Encode(ack); err != nil {
		return
	}

	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn) //nolint:errcheck
		close(gone)
	}()

	tick := time.NewTicker(followPoll)
	defer tick.Stop()
	for {
		rung := d.journalBell.wait()
		for {
			evs, err := d.store.SessionEventsAfter(cursor, followBatch)
			if err != nil {
				enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
				return
			}
			if len(evs) == 0 {
				break
			}
			if err := enc.Encode(journalBatch(evs)); err != nil {
				return
			}
			cursor = evs[len(evs)-1].Seq
			if len(evs) < followBatch {
				break
			}
		}
		select {
		case <-rung:
		case <-tick.C:
		case <-gone:
			return
		case <-ctx.Done():
			return
		}
	}
}

func journalBatch(evs []memory.SessionEvent) protocol.Msg {
	out := make([]protocol.JournalEvent, len(evs))
	for i, e := range evs {
		out[i] = protocol.JournalEvent{
			Seq: e.Seq, AgentID: e.AgentID, Turn: e.Turn, SpanID: e.SpanID,
			ParentSpanID: e.ParentSpanID, Type: e.Type, TS: e.TS, Payload: e.Payload,
		}
	}
	data, _ := json.Marshal(out)
	return protocol.NewTextMsg(protocol.TypeJournalFollow, string(data))
}

// handleListHumanRequests returns the ask_human questions still waiting.
func (d *Daemon) handleListHumanRequests(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg(protocol.TypeListHumanRequests, `[]`)) //nolint:errcheck
		return
	}
	rows, err := d.store.HumanRequestListPending()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	out := make([]protocol.PendingQuestion, len(rows))
	for i, r := range rows {
		out[i] = protocol.PendingQuestion{
			ID: r.ID, AgentID: r.AgentID, Question: r.Question, Options: r.Options,
			CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt,
		}
	}
	data, _ := json.Marshal(out)
	enc.Encode(protocol.NewTextMsg(protocol.TypeListHumanRequests, string(data))) //nolint:errcheck
}
