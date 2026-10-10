package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/toolvm"
)

// Journal event type of a delivery skipped for its depth or its source.
const evProcessSkipped = "process_skipped"

// SetMaxDepth sets [processes] max_depth; <= 0 is the default.
func (r *StandingRunner) SetMaxDepth(n int) {
	if r != nil {
		r.maxDepth = n
	}
}

func (r *StandingRunner) maxDepthOrDefault() int {
	if r.maxDepth > 0 {
		return r.maxDepth
	}
	return config.DefaultMaxDepth
}

// encodeEventTrigger stores a process's event trigger on its row.
func encodeEventTrigger(on []string, f config.EventFilter, content bool) (string, string, bool) {
	if len(on) == 0 {
		return "", "", false
	}
	filter := ""
	if f != (config.EventFilter{}) {
		b, _ := json.Marshal(f) // a struct of strings always marshals
		filter = string(b)
	}
	return strings.Join(on, ","), filter, content
}

// decodeEventTrigger reads a process's event trigger back from its row.
func decodeEventTrigger(on, filter string) ([]string, config.EventFilter) {
	var types []string
	if on != "" {
		types = strings.Split(on, ",")
	}
	var f config.EventFilter
	if filter != "" {
		_ = json.Unmarshal([]byte(filter), &f) // written by encodeEventTrigger
	}
	return types, f
}

// contentKeys are the payload fields that carry content — what a tool was
// given and returned, what a turn was asked and answered, a sub-agent's task,
// a report's text. An event delivers them only to a process with
// event_content; every other field is metadata.
var contentKeys = []string{"input", "output", "result", "task", "text"}

// EventRouter delivers journal events to the live processes they trigger
// (adr/process-sessions.md §7). It is a journal subscriber: it reads events in
// order from a durable cursor, off the turn path.
//
// Lineage: a turn's depth is journaled on its turn_start, and every event of
// that turn has it; a conversation's is 0. No process is triggered by an event
// at [processes] max_depth or more, and the skip is journaled. A process is
// never triggered by an event from its own session.
func (r *StandingRunner) EventRouter() *EventRouter {
	return &EventRouter{r: r, started: time.Now(), depths: map[string]map[int]int{}, skipped: map[string]bool{}}
}

// EventRouter is the subscriber EventRouter returns.
type EventRouter struct {
	r       *StandingRunner
	started time.Time
	// depths is each session's turn depths, from their turn_start events.
	depths map[string]map[int]int
	// skipped dedupes depth-skip journal entries per process, session and turn,
	// so a deep turn's many events journal one skip each.
	skipped map[string]bool
}

// ID is the subscriber's durable cursor key.
func (e *EventRouter) ID() string { return "process-events" }

// Types are the event types a process may be triggered by.
func (e *EventRouter) Types() []string { return config.EventTypes }

// Handle routes one event. Events from before the daemon started are not
// delivered: a process reacts to what happens while it runs, and replaying the
// journal's history at boot would trigger it on all of it.
func (e *EventRouter) Handle(_ context.Context, ev memory.SessionEvent) error {
	if ev.TS.Before(e.started) {
		return nil
	}
	if ev.Type == "turn_start" {
		var p struct {
			Depth int `json:"depth"`
		}
		_ = json.Unmarshal(ev.Payload, &p) // a missing depth is 0
		if e.depths[ev.AgentID] == nil {
			e.depths[ev.AgentID] = map[int]int{}
		}
		e.depths[ev.AgentID][ev.Turn] = p.Depth
	}
	depth := e.depths[ev.AgentID][ev.Turn]
	if strings.HasPrefix(ev.Type, "process_") || strings.HasPrefix(ev.Type, "standing_") {
		// A process's own transitions are journaled outside any turn; they are a
		// process's events, one deep.
		depth = max(depth, 1)
	}

	r := e.r
	r.liveMu.Lock()
	lives := make([]*liveProc, 0, len(r.lives))
	for _, lp := range r.lives {
		lives = append(lives, lp)
	}
	r.liveMu.Unlock()

	for _, lp := range lives {
		row := lp.row
		on, filter := decodeEventTrigger(row.OnEvents, row.EventFilter)
		if !slices.Contains(on, ev.Type) || ev.AgentID == row.SessionID || ev.AgentID == row.ID {
			continue
		}
		if !e.matches(filter, ev) {
			continue
		}
		if max := r.maxDepthOrDefault(); depth >= max {
			key := fmt.Sprintf("%s|%s|%d", row.ID, ev.AgentID, ev.Turn)
			if !e.skipped[key] {
				e.skipped[key] = true
				journalTransition(r.store, row.ID, evProcessSkipped, map[string]any{
					"event": ev.Type, "session": ev.AgentID, "turn": ev.Turn,
					"depth": depth, "max_depth": max, "reason": "max_depth",
				})
			}
			continue
		}
		t := toolvm.Trigger{
			Kind: "event", At: ev.TS, From: ev.AgentID, Depth: depth,
			Event: eventPayload(ev, row.EventContent),
			// Content is text from elsewhere; a turn it starts is restricted as a
			// piped report's is.
			Piped: row.EventContent,
		}
		if !lp.offerEvent(t) {
			journalTransition(r.store, row.ID, evProcessSkipped, map[string]any{
				"event": ev.Type, "session": ev.AgentID, "turn": ev.Turn, "reason": "busy",
			})
		}
	}
	return nil
}

// matches applies a process's event filter.
func (e *EventRouter) matches(f config.EventFilter, ev memory.SessionEvent) bool {
	if f.Session != "" && ev.AgentID != f.Session {
		return false
	}
	if f.Tool != "" {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		if p.Name != f.Tool {
			return false
		}
	}
	switch f.Sessions {
	case "conversations":
		return !e.isProcessSession(ev.AgentID)
	case "processes":
		return e.isProcessSession(ev.AgentID)
	}
	return true
}

// isProcessSession reports whether a session is one a process drives, or a
// process's own transitions.
func (e *EventRouter) isProcessSession(id string) bool {
	if _, ok, err := e.r.store.ProcessGet(id); err == nil && ok {
		return true
	}
	procs, err := e.r.store.ProcessesOfSession(id)
	return err == nil && len(procs) > 0
}

// eventPayload is what a process receives for ev: its type, session, turn and
// time, and its payload, without the content fields unless content is set.
func eventPayload(ev memory.SessionEvent, content bool) json.RawMessage {
	var data map[string]any
	_ = json.Unmarshal(ev.Payload, &data)
	if !content {
		for _, k := range contentKeys {
			delete(data, k)
		}
	}
	b, _ := json.Marshal(map[string]any{
		"type": ev.Type, "session": ev.AgentID, "turn": ev.Turn,
		"at": ev.TS.UTC().Format(time.RFC3339), "data": data,
	})
	return b
}
