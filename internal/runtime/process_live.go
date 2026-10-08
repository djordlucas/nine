package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"nine/internal/memory"
	"nine/internal/toolvm"
)

// Live processes (adr/process-sessions.md §2): a js tool started once and left
// running, driving its session through nine:process. The runner starts each
// one, feeds it its triggers — clock ticks and the messages piped to its
// session — through next(), runs its turn() calls in its session, and restarts
// it with backoff when it fails. Slice processes, today's standing tools, are
// driven by runDue and are untouched by this file.

// ProcessSessions runs a process's model turns. The daemon implements it.
type ProcessSessions interface {
	// ProcessTurn runs one turn and returns its reply and the tokens it spent.
	ProcessTurn(ctx context.Context, id string, p RoleParams, text, trigger string) (reply string, tokens int, err error)
}

// liveTriggerQueue is how many triggers may wait for a busy process. A clock
// tick already waiting is never queued twice: a process that fell behind runs
// once, not once per missed tick.
const liveTriggerQueue = 8

// liveProc is one running live process.
type liveProc struct {
	row      memory.Process
	live     *toolvm.Live
	triggers chan toolvm.Trigger

	mu sync.Mutex
	// waiting is true while the program is blocked in next() with nothing
	// queued: the state in which a piped report is delivered rather than sent
	// to the human feed, as a condition trigger's wake is today.
	waiting bool
	// clockQueued is true while a clock tick waits in triggers.
	clockQueued bool
	// label is the journal trigger label of the turns the current trigger
	// causes: "idle" for a clock tick, "condition" for a piped report.
	label string
}

// SetSessions wires the session side of live processes. Without it, live
// processes are not started.
func (r *StandingRunner) SetSessions(s ProcessSessions) {
	if r != nil {
		r.sessions = s
	}
}

// tickLive brings the running live processes in line with the store, and
// delivers the clock ticks that are due. It runs on the runner's cadence.
func (r *StandingRunner) tickLive(ctx context.Context) {
	if r.sessions == nil {
		return
	}
	r.resumeBudgets(time.Now())
	r.bindGoals()
	rows, err := r.store.ProcessesLive()
	if err != nil {
		slog.Warn("live processes: list", "err", err)
		return
	}
	want := make(map[string]memory.Process, len(rows))
	now := time.Now()
	for _, row := range rows {
		want[row.ID] = row
	}

	r.liveMu.Lock()
	// Stop what should no longer run: stopped, deleted, or no longer live.
	for id, lp := range r.lives {
		if _, ok := want[id]; !ok {
			lp.live.Stop()
		}
	}
	running := make(map[string]*liveProc, len(r.lives))
	for id, lp := range r.lives {
		running[id] = lp
	}
	r.liveMu.Unlock()

	for _, row := range rows {
		lp, ok := running[row.ID]
		if !ok {
			// A process that failed restarts once its backoff has passed. One
			// that has not starts at once, and waits for its first tick in
			// next(); startLive schedules that tick one cadence away.
			if row.Failures > 0 && !due(row.NextAt, now) {
				continue
			}
			r.startLive(ctx, row)
			continue
		}
		if hasClock(row) && due(row.NextAt, now) {
			lp.enqueueClock(now)
			if err := r.store.ProcessSetNextAt(row.ID, r.nextCycleAt(row, now)); err != nil {
				slog.Warn("live process: schedule next tick", "id", row.ID, "err", err)
			}
		}
	}
}

// bindGoals applies goal binding (adr/process-sessions.md §4): a process whose
// goal is no longer active is stopped by its goal, and one its goal stopped
// runs again once the goal is active. The goal's status decides; the agent owns
// that status.
func (r *StandingRunner) bindGoals() {
	live, err := r.store.ProcessesLive()
	if err != nil {
		slog.Warn("live processes: list for goal binding", "err", err)
		return
	}
	for _, p := range live {
		if p.GoalID == "" || r.goalActive(p.GoalID) {
			continue
		}
		if _, err := r.store.ProcessStop(p.ID, "goal"); err != nil {
			slog.Warn("live process: stop for its goal", "id", p.ID, "err", err)
			continue
		}
		r.log.add(p.ID, "stopped", "its goal is no longer active")
	}
	paused, err := r.store.ProcessesStoppedBy("goal")
	if err != nil {
		slog.Warn("live processes: list goal-stopped", "err", err)
		return
	}
	for _, p := range paused {
		if p.GoalID == "" || !r.goalActive(p.GoalID) {
			continue
		}
		if _, err := r.store.ProcessSetState(p.ID, memory.ProcessRunning); err != nil {
			slog.Warn("live process: restart for its goal", "id", p.ID, "err", err)
		}
	}
}

func (r *StandingRunner) goalActive(id string) bool {
	g, err := r.store.GoalGet(id)
	if err != nil {
		slog.Warn("live process: read its goal", "goal_id", id, "err", err)
		return true // a read failure must not stop a process
	}
	return g != nil && g.Status == "active"
}

// startLive starts one live process and watches it until it ends. A process
// that cannot start is recorded as a failure.
func (r *StandingRunner) startLive(ctx context.Context, row memory.Process) {
	lp := &liveProc{row: row, triggers: make(chan toolvm.Trigger, liveTriggerQueue)}
	live, err := r.host.StartLive(ctx, row.Tool, json.RawMessage(argsOrEmptyString(row.Args)), &liveHandler{r: r, lp: lp})
	if err != nil {
		r.recordFailure(row, err)
		return
	}
	lp.live = live
	// The first tick comes one cadence after the start, at boot as at spawn:
	// the routines processes replace woke one idle interval after their session
	// started, never at once.
	if hasClock(row) {
		if err := r.store.ProcessSetNextAt(row.ID, r.nextCycleAt(row, time.Now())); err != nil {
			slog.Warn("live process: schedule its first tick", "id", row.ID, "err", err)
		}
	}
	r.liveMu.Lock()
	r.lives[row.ID] = lp
	r.liveMu.Unlock()
	if row.Failures > 0 {
		if err := r.store.ProcessRecovered(row.ID); err != nil {
			slog.Warn("live process: clear failures", "id", row.ID, "err", err)
		}
	}
	r.log.add(row.ID, "started", "")
	slog.Info("live process started", "id", row.ID, "tool", row.Tool, "session", row.SessionID)

	go func() {
		_, err := live.Wait()
		r.liveMu.Lock()
		if r.lives[row.ID] == lp {
			delete(r.lives, row.ID)
		}
		r.liveMu.Unlock()
		switch {
		case errors.Is(err, toolvm.ErrStopped):
			r.log.add(row.ID, "stopped", "")
		case err != nil && r.stoppedInStore(row.ID):
			// A program that throws because it was stopped — a budget refusing
			// its turn, which pursue does not catch — ended with its stop, not
			// with a failure.
			r.log.add(row.ID, "stopped", "")
		case err != nil:
			r.recordFailure(row, err)
		default:
			// The program returned: the process is over until something starts
			// it again.
			if _, err := r.store.ProcessSetState(row.ID, memory.ProcessStopped); err != nil {
				slog.Warn("live process: record its end", "id", row.ID, "err", err)
			}
			r.log.add(row.ID, "returned", "")
		}
	}()
}

// stoppedInStore reports whether process id is stopped in the store.
func (r *StandingRunner) stoppedInStore(id string) bool {
	p, ok, err := r.store.ProcessGet(id)
	return err == nil && ok && p.State == memory.ProcessStopped
}

// StopLive stops every running live process, for the daemon's shutdown.
func (r *StandingRunner) StopLive() {
	if r == nil {
		return
	}
	r.liveMu.Lock()
	lives := make([]*liveProc, 0, len(r.lives))
	for _, lp := range r.lives {
		lives = append(lives, lp)
	}
	r.liveMu.Unlock()
	for _, lp := range lives {
		lp.live.Stop()
	}
}

// Deliver pipes text into process session sessionID: as a message trigger to
// the live process that owns it. handled reports whether a live process owns
// that session at all; delivered whether it took the message, which it does
// only while it waits in next() — the rule a condition trigger's wake has
// today, so a finding for a busy process goes to the human feed instead.
func (r *StandingRunner) Deliver(sessionID, text, from string) (handled, delivered bool) {
	if r == nil {
		return false, false
	}
	r.liveMu.Lock()
	var owner *liveProc
	for _, lp := range r.lives {
		if lp.row.Owner && lp.row.SessionID == sessionID {
			owner = lp
			break
		}
	}
	r.liveMu.Unlock()
	if owner == nil {
		return false, false
	}
	return true, owner.offerMessage(toolvm.Trigger{Kind: "message", At: time.Now(), Text: text, From: from})
}

func (lp *liveProc) enqueueClock(now time.Time) {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	if lp.clockQueued {
		return
	}
	select {
	case lp.triggers <- toolvm.Trigger{Kind: "clock", At: now}:
		lp.clockQueued = true
	default:
	}
}

func (lp *liveProc) offerMessage(t toolvm.Trigger) bool {
	lp.mu.Lock()
	defer lp.mu.Unlock()
	if !lp.waiting || len(lp.triggers) > 0 {
		return false
	}
	select {
	case lp.triggers <- t:
		return true
	default:
		return false
	}
}

// liveHandler answers one live process's nine:process calls.
type liveHandler struct {
	r  *StandingRunner
	lp *liveProc
}

func (h *liveHandler) Next(ctx context.Context) (toolvm.Trigger, error) {
	lp := h.lp
	lp.mu.Lock()
	lp.waiting = true
	lp.mu.Unlock()
	defer func() {
		lp.mu.Lock()
		lp.waiting = false
		lp.mu.Unlock()
	}()
	for {
		select {
		case t := <-lp.triggers:
			lp.mu.Lock()
			if t.Kind == "clock" {
				lp.clockQueued = false
				lp.label = "idle"
			} else {
				lp.label = "condition"
			}
			lp.mu.Unlock()
			// A goal-bound process works on its goal as it stands now; a tick
			// that finds the goal gone or inactive does nothing, as the routine
			// it replaces did, and the next pass stops the process.
			if goalID := lp.row.GoalID; goalID != "" && t.Kind == "clock" {
				g, err := h.r.store.GoalGet(goalID)
				if err != nil || g == nil || g.Status != "active" {
					continue
				}
				t.Goal = &toolvm.TriggerGoal{ID: g.ID, Description: g.Description}
			}
			return t, nil
		case <-ctx.Done():
			return toolvm.Trigger{}, toolvm.ErrStopped
		}
	}
}

func (h *liveHandler) Turn(ctx context.Context, text string) (string, error) {
	lp := h.lp
	lp.mu.Lock()
	label := lp.label
	lp.mu.Unlock()
	row := lp.row
	if err := h.r.spendTurn(row.ID, time.Now()); err != nil {
		return "", err
	}
	reply, tokens, err := h.r.sessions.ProcessTurn(ctx, row.SessionID, h.r.sessionParams(row), text, label)
	// A turn that ran is counted, failed or not; one stopped before it began
	// spent nothing.
	if err == nil || tokens > 0 {
		h.r.chargeTurn(row.ID, tokens)
	}
	if err != nil && ctx.Err() != nil {
		return "", toolvm.ErrStopped
	}
	return reply, err
}

func (h *liveHandler) Report(_ context.Context, text string) (bool, error) {
	row := h.lp.row
	if row.ReportTo == "" {
		return false, fmt.Errorf("process %q pipes nowhere: it declares no report_to", row.ID)
	}
	return h.r.pipe(row, text), nil
}

// hasClock reports whether a process has a clock trigger.
func hasClock(p memory.Process) bool { return p.IntervalSecs > 0 || p.Schedule != "" }

// due reports whether a stored next-at time has come; never-set is due.
func due(nextAt string, now time.Time) bool {
	if nextAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, nextAt)
	if err != nil {
		return true
	}
	return !t.After(now)
}

// sessionParams is the role a process's turns run under: its own when it owns
// its session, the owner's when it is attached to one.
func (r *StandingRunner) sessionParams(p memory.Process) RoleParams {
	if p.Owner {
		return processRoleParams(p)
	}
	procs, err := r.store.ProcessesOfSession(p.SessionID)
	if err != nil {
		slog.Warn("live process: find its session's owner", "id", p.ID, "err", err)
	}
	for _, o := range procs {
		if o.Owner {
			return processRoleParams(o)
		}
	}
	return processRoleParams(p)
}
