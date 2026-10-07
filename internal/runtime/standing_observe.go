package runtime

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"nine/internal/memory"
)

// standingLogSize is how many recent calls per standing tool the ring buffer
// keeps. Enough to see a pattern in `nine tool logs`, small enough that a
// hundred standing tools cost nothing worth measuring.
const standingLogSize = 50

// StandingLogEntry is one line of recent activity.
type StandingLogEntry struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"` // continued | completed | failed
	Detail  string    `json:"detail,omitempty"`
}

// standingLog is a bounded per-tool ring of recent calls.
//
// It is deliberately in memory and deliberately lossy. A standing tool on a
// ten-second cadence makes 8,640 calls a day; the point of this buffer is to
// answer "what has it been doing lately" without any of that reaching the
// journal or the database (see journalTransition for the rule).
type standingLog struct {
	mu      sync.Mutex
	entries map[string][]StandingLogEntry
}

func newStandingLog() *standingLog {
	return &standingLog{entries: map[string][]StandingLogEntry{}}
}

func (l *standingLog) add(id, outcome, detail string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := append(l.entries[id], StandingLogEntry{At: time.Now(), Outcome: outcome, Detail: detail})
	if len(e) > standingLogSize {
		e = e[len(e)-standingLogSize:]
	}
	l.entries[id] = e
}

// recent returns up to n entries, newest last. n <= 0 returns all of them.
func (l *standingLog) recent(id string, n int) []StandingLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[id]
	if n > 0 && len(e) > n {
		e = e[len(e)-n:]
	}
	out := make([]StandingLogEntry, len(e))
	copy(out, e)
	return out
}

// forget drops a tool's buffer, for a standing run that has been deleted.
func (l *standingLog) forget(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, id)
}

// Standing-tool journal event types. These are the *only* things a standing run
// writes to the journal.
const (
	evStandingStarted   = "standing_started"
	evStandingStopped   = "standing_stopped"
	evStandingFailing   = "standing_failing"
	evStandingRecovered = "standing_recovered"
	evStandingReported  = "standing_reported"
)

// journalTransition records a standing tool's state change or its output.
//
// **A standing run's ordinary calls are not journal events, and that is a rule
// rather than an omission.** A tool on a ten-second cadence is 8,640 calls a
// day; writing each one would swamp session_events, distort the retention scrub,
// and bury what `nine trace` exists to show. What is journaled is *things
// happening*: started, stopped, entering and leaving `failing`, and any cycle
// that produced output. Volume is then proportional to events, not to time.
//
// The counterpart rule is that anything a standing tool does which *reaches the
// world* is audited as usual — every net.http call still lands in the journal
// under R-TVM.12, unamended. A standing tool's own heartbeat is not an event;
// what it does is.
//
// Events are attributed to the standing tool's id as the agent, so
// `nine trace <id>` reads a standing tool's history the way it reads a session's.
// Turn is always 0: a standing run has cycles, not turns, and pretending
// otherwise would give the replay machinery something it cannot replay.
func journalTransition(store *memory.Store, id, evType string, payload any) {
	if store == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = json.RawMessage(`{}`)
	}
	if err := store.SessionEventsAppend([]memory.SessionEvent{{
		AgentID: id,
		Turn:    0,
		SpanID:  "standing",
		Type:    evType,
		Payload: raw,
	}}); err != nil {
		slog.Warn("standing tool: journal", "id", id, "type", evType, "err", err)
	}
}

// StandingStatus is one standing run as the operator surface reports it.
type StandingStatus struct {
	ID         string             `json:"id"`
	Tool       string             `json:"tool"`
	State      string             `json:"state"`
	Trigger    string             `json:"trigger"`
	Calls      int                `json:"calls"`
	Cycles     int                `json:"cycles"`
	Failures   int                `json:"failures,omitempty"`
	LastError  string             `json:"last_error,omitempty"`
	LastCallAt string             `json:"last_call_at,omitempty"`
	NextAt     string             `json:"next_at,omitempty"`
	Generated  bool               `json:"generated,omitempty"`
	Recent     []StandingLogEntry `json:"recent,omitempty"`
}

// triggerText renders a standing tool's cadence for display.
func triggerText(t memory.Process) string {
	if t.Schedule != "" {
		return "cron " + t.Schedule
	}
	if t.IntervalSecs > 0 {
		return "every " + (time.Duration(t.IntervalSecs) * time.Second).String()
	}
	return "—"
}

func standingStatusOf(t memory.Process, recent []StandingLogEntry) StandingStatus {
	return StandingStatus{
		ID: t.ID, Tool: t.Tool, State: t.State, Trigger: triggerText(t),
		Calls: t.Calls, Cycles: t.Cycles, Failures: t.Failures,
		LastError: t.LastError, LastCallAt: t.LastCallAt, NextAt: t.NextAt,
		Generated: t.Generated, Recent: recent,
	}
}

func clipDetail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s…", s[:n])
}
