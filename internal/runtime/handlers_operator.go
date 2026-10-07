package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"

	"nine/internal/memory"
	"nine/internal/protocol"
)

// This file holds the operator verbs added so the HTTP API can reach what the
// CLI reaches by opening the store directly (spec/contracts/api.md API-A-1
// forbids the API process that shortcut): the skill catalog, goal create and
// delete, a session's transcript and journal, and a live watch of a session.

// operatorGoalOrigin is the goals.parent_type of a top-level goal an operator
// created over the wire. It sits beside "conversation" (created by an agent's
// goal_create) and "config" (seeded from [[agent]]), so the reconciler, which
// touches only "config" goals, leaves these alone.
const operatorGoalOrigin = "operator"

// ConfigGoalOrigin is the goals.parent_type marking a goal seeded from a
// [[agent]] config block rather than a conversation. The boot reconciler touches
// only these (adr/predefined-agents-design.md §3.3), and deleting one over the
// wire is refused, since the next boot would re-seed it.
const ConfigGoalOrigin = "config"

// watchBuffer bounds the events queued for one watcher. A watcher that falls
// this far behind loses events rather than stalling the session's turn.
const watchBuffer = 1024

func (d *Daemon) handleListSkills(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg(protocol.TypeListSkills, `{"skills":[]}`)) //nolint:errcheck
		return
	}
	rows, err := d.store.SkillList()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	out := make([]protocol.SkillInfo, 0, len(rows))
	for _, sk := range rows {
		tags := sk.Tags
		if tags == nil {
			tags = []string{}
		}
		out = append(out, protocol.SkillInfo{
			Name: sk.Name, Description: sk.Description, Tags: tags, Source: sk.Source,
		})
	}
	data, _ := json.Marshal(map[string]any{"skills": out})
	enc.Encode(protocol.NewTextMsg(protocol.TypeListSkills, string(data))) //nolint:errcheck
}

// handleGoalCreate creates a goal on the operator's behalf.
//
// The agent tool goal_create is gated behind a role's Delegates flag because it
// decides which *agents* may start background work. An operator is not an
// agent: they reach the daemon through its socket, as `nine` and the
// authenticated HTTP API do, with the same standing as a [[agent]] block in
// nine.toml. So this verb is not role-gated — it runs the same steps the tool
// runs, recording the goal and, for a top-level goal, spawning its pursue
// session.
func (d *Daemon) handleGoalCreate(ctx context.Context, enc *json.Encoder, r protocol.GoalCreateReq) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("goals not available")) //nolint:errcheck
		return
	}
	parentType := operatorGoalOrigin
	if r.ParentID != "" {
		parent, err := d.store.GoalGet(r.ParentID)
		if err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
			return
		}
		if parent == nil {
			enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("parent goal %s not found", r.ParentID))) //nolint:errcheck
			return
		}
		parentType = "goal"
	}

	id := memory.NewID()
	if err := d.store.GoalCreate(id, r.Description, r.ParentID, parentType); err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}

	pursue := "none"
	if r.ParentID == "" {
		spawned, err := d.SpawnGoalSession(ctx, id)
		if err != nil {
			// A top-level goal is a promise that something pursues it. Leaving
			// the row behind would make an unattended goal look like a slot-cap
			// outcome, so undo it and report the failure instead.
			if _, derr := d.store.GoalDelete(id); derr != nil {
				slog.Warn("goal create: rollback failed", "id", id, "err", derr)
			}
			enc.Encode(protocol.NewErrorMsg("start pursue session: " + err.Error())) //nolint:errcheck
			return
		}
		pursue = "limit_reached"
		if spawned {
			pursue = "spawned"
		}
	}

	g, err := d.store.GoalGet(id)
	if err != nil || g == nil {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("read back goal %s: %v", id, err))) //nolint:errcheck
		return
	}
	goalJSON, _ := json.Marshal(g)
	data, _ := json.Marshal(protocol.GoalCreateResult{Goal: goalJSON, PursueSession: pursue})
	slog.Info("goal created by operator", "id", id, "parent_id", r.ParentID, "pursue_session", pursue)
	enc.Encode(protocol.NewTextMsg(protocol.TypeGoalCreate, string(data))) //nolint:errcheck
}

// handleGoalDelete deletes a goal and its sub-goals and stops the goal's pursue
// session. The session is stopped, not erased: its transcript and journal stay
// readable, and the retention reaper takes them once the session is abandoned.
func (d *Daemon) handleGoalDelete(ctx context.Context, enc *json.Encoder, id string) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("goals not available")) //nolint:errcheck
		return
	}
	g, err := d.store.GoalGet(id)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if g == nil {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("goal %s not found", id))) //nolint:errcheck
		return
	}
	if g.ParentType == ConfigGoalOrigin {
		msg := fmt.Sprintf("goal %s is declared by an [[agent]] block in nine.toml "+
			"and would be re-created at the next boot; remove the block instead", id)
		enc.Encode(protocol.NewErrorMsg(protocol.ConflictPrefix + msg)) //nolint:errcheck
		return
	}

	// Stop the session before removing the rows, so a turn in flight cannot
	// update a goal that is being deleted under it. Only a top-level goal has a
	// session; a sub-goal's id names none and this is a no-op for it.
	d.mu.RLock()
	_, running := d.sessions[id]
	d.mu.RUnlock()
	// A goal session is its processes as much as its worker: the worker only
	// exists once the pursue process has taken a turn.
	if !running && d.procs != nil {
		if procs, err := d.procs.ProcessesOfSession(id); err == nil && len(procs) > 0 {
			running = true
		}
	}
	if err := d.TeardownStandingSession(ctx, id); err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}

	deleted, err := d.store.GoalDelete(id)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if deleted == nil {
		deleted = []string{}
	}
	slog.Info("goal deleted by operator", "id", id, "deleted", deleted, "session_stopped", running)
	data, _ := json.Marshal(protocol.GoalDeleteResult{Deleted: deleted, SessionStopped: running})
	enc.Encode(protocol.NewTextMsg(protocol.TypeGoalDelete, string(data))) //nolint:errcheck
}

// sessionKnown reports whether id names a session the daemon can say something
// about: a live worker, a stored conversation, or a journal. The last covers
// sub-agents, which journal under their own id without a conversation row.
func (d *Daemon) sessionKnown(id string, events int) bool {
	if events > 0 {
		return true
	}
	d.mu.RLock()
	_, live := d.sessions[id]
	d.mu.RUnlock()
	if live {
		return true
	}
	_, found, err := d.store.SessionGet(id)
	return err == nil && found
}

// handleSessionHistory returns a session's whole transcript — every turn, not
// the reattach cap — as the same ordered messages an attach carries.
func (d *Daemon) handleSessionHistory(enc *json.Encoder, agentID string) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("session history not available")) //nolint:errcheck
		return
	}
	id := d.resolveID(agentID)
	history := d.journalHistoryTurns(id, 0)
	if !d.sessionKnown(id, len(history)) {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("session %s not found", agentID))) //nolint:errcheck
		return
	}
	if history == nil {
		history = []protocol.Msg{}
	}
	data, _ := json.Marshal(history)
	enc.Encode(protocol.NewTextMsg(protocol.TypeSessionHistory, string(data))) //nolint:errcheck
}

// handleSessionEvents returns a session's journal: every turn (turn 0), one
// turn (N), or the latest (-1). An existing session with no such turn yields an
// empty list rather than an error.
func (d *Daemon) handleSessionEvents(enc *json.Encoder, agentID string, turn int) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("session journal not available")) //nolint:errcheck
		return
	}
	id := d.resolveID(agentID)
	events, err := d.store.SessionEventsByAgent(id)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if !d.sessionKnown(id, len(events)) {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("session %s not found", agentID))) //nolint:errcheck
		return
	}
	if turn < 0 && len(events) > 0 {
		turn = events[len(events)-1].Turn
	}
	out := make([]protocol.JournalEvent, 0, len(events))
	for _, e := range events {
		if turn > 0 && e.Turn != turn {
			continue
		}
		out = append(out, protocol.JournalEvent{
			Seq: e.Seq, AgentID: e.AgentID, Turn: e.Turn, SpanID: e.SpanID,
			ParentSpanID: e.ParentSpanID, Type: e.Type, TS: e.TS, Payload: e.Payload,
		})
	}
	data, _ := json.Marshal(out)
	enc.Encode(protocol.NewTextMsg(protocol.TypeSessionEvents, string(data))) //nolint:errcheck
}

// watch streams agentID's progress events and turn outcomes to conn until the
// client hangs up, the session stops, or the daemon shuts down. It owns conn for
// that whole time and closes it on return: nothing else is read from a watched
// connection, so the read below exists only to notice the client leaving.
//
// A session that exists but is not loaded is revived from its checkpoint, the
// same as a turn addressed to it would be — a watcher waiting for a session's
// next idle wake should not need to send a turn first.
func (d *Daemon) watch(ctx context.Context, conn net.Conn, enc *json.Encoder, agentID string) {
	defer conn.Close() //nolint:errcheck

	id := d.resolveID(agentID)
	if err := d.attach(id); err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	d.mu.RLock()
	w := d.sessions[id]
	d.mu.RUnlock()
	if w == nil {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("conversation %s not found", agentID))) //nolint:errcheck
		return
	}

	events := make(chan protocol.Msg, watchBuffer)
	remove := w.addWatcher(func(m protocol.Msg) {
		select {
		case events <- m:
		default:
		}
	})
	defer remove()

	if err := enc.Encode(protocol.Msg{Type: protocol.TypeWatch, AgentID: id}); err != nil {
		return
	}

	gone := make(chan struct{})
	go func() {
		io.Copy(io.Discard, conn) //nolint:errcheck
		close(gone)
	}()

	for {
		select {
		case m := <-events:
			if err := enc.Encode(m); err != nil {
				return
			}
		case <-w.stopped:
			enc.Encode(protocol.NewAgentErrorMsg(id, "session stopped")) //nolint:errcheck
			return
		case <-gone:
			return
		case <-ctx.Done():
			return
		}
	}
}
