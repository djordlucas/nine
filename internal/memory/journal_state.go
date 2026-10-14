package memory

import (
	"encoding/json"
	"log/slog"
)

// State-change events (spec/contracts/event-journal.md R-EVT.6).
//
// The journal began as a record of what sessions execute. Approvals, questions,
// notifications, goals and workflows change state outside any turn, so they
// never reached it, and a client following the journal could not see an
// approval arrive. The store journals them itself, beside the write that makes
// the change: every path that changes one goes through these methods — the
// daemon, a tool in a turn, the CLI — so none is missed.
//
// They are written with turn 0, like a process's transitions: they belong to
// no turn. A change with no session behind it is attributed to DaemonAgentID.

// DaemonAgentID is the agent_id of a state-change event no session made: an
// operator's decision, a notification from the daemon itself.
const DaemonAgentID = "daemon"

// The state-change event types.
const (
	EvHumanInputRequested = "human_input_requested"
	EvHumanInputAnswered  = "human_input_answered"
	EvHumanInputTimedOut  = "human_input_timed_out"
	EvCapabilityRequested = "capability_requested"
	EvCapabilityDecided   = "capability_decided"
	EvCapabilityRevoked   = "capability_revoked"
	EvNotification        = "notification"
	EvGoalCreated         = "goal_created"
	EvGoalStatus          = "goal_status"
	EvGoalDeleted         = "goal_deleted"
	EvWorkflowCreated     = "workflow_created"
	EvWorkflowStep        = "workflow_step"
	EvWorkflowFailed      = "workflow_failed"
	EvWorkflowCancelled   = "workflow_cancelled"
)

// stateSpanID marks a state-change event's span: it has no turn tree to sit in.
const stateSpanID = "state"

// journalHook is called after every successful journal write. The daemon sets
// it to wake whoever follows the journal; nil means no one does.
type journalHook = func()

// OnJournal sets the function called after each journal write, from the
// writing goroutine. It must not block. Passing nil removes it.
func (s *Store) OnJournal(f func()) {
	if f == nil {
		s.journalHook.Store(nil)
		return
	}
	s.journalHook.Store(&f)
}

func (s *Store) journalWritten() {
	if f := s.journalHook.Load(); f != nil {
		(*f)()
	}
}

// journalState appends one state-change event. It never fails the change it
// records: the change has already happened, and a missing journal row is a
// monitoring gap, as for the session sink.
func (s *Store) journalState(agentID, typ string, payload any) {
	if agentID == "" {
		agentID = DaemonAgentID
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte("{}")
	}
	if err := s.SessionEventsAppend([]SessionEvent{{
		AgentID: agentID,
		SpanID:  stateSpanID,
		Type:    typ,
		Payload: raw,
	}}); err != nil {
		slog.Warn("journal state change", "type", typ, "agent_id", agentID, "err", err)
	}
}

// SessionEventsBounds returns the oldest seq still in the journal and the
// highest seq ever assigned. latest is the AUTOINCREMENT high-water mark, not
// max(seq): a scrub can delete the newest row, and a follower's cursor must
// still compare against the seq the next event will exceed. Both are 0 for a
// journal that has never been written.
func (s *Store) SessionEventsBounds() (oldest, latest int64, err error) {
	err = s.db.QueryRow(
		`SELECT COALESCE((SELECT min(seq) FROM session_events), 0),
		        COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'session_events'), 0)`,
	).Scan(&oldest, &latest)
	return oldest, latest, err
}
