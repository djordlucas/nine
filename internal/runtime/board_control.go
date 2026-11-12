package runtime

import (
	"fmt"
	"log/slog"

	"nine/internal/agent"
	"nine/internal/memory"
)

// Board journal event types (adr/orchestrator-board.md §2.1): every transition
// on the board is written to session_events, so the event stream, `nine trace`
// and the web UI see it live.
const (
	EvTaskPosted   = "task_posted"
	EvTaskClaimed  = "task_claimed"
	EvTaskDone     = "task_done"
	EvTaskFailed   = "task_failed"
	EvTaskCancelled = "task_cancelled"
	EvTaskUpdated  = "task_updated"
	EvTaskDeleted  = "task_deleted"
)

// boardControl is the daemon's implementation of agent.BoardControl: the store
// owns the rows and the atomic transitions; this type owns admission (the §2.4
// guards), the journal events, and result delivery to the poster. Wired by
// Assemble, granted per role by the builder's filterByRole.
type boardControl struct {
	store     *memory.Store
	maxDepth  int // [processes] max_depth, defaulted
	maxQueued int // [board] max_queued, defaulted
	// wake delivers a finished task's result to its poster as a restricted
	// condition turn (daemon.WakeAgent); feed posts to the human feed when the
	// poster is gone. Nil disables delivery (tests).
	wake func(agentID, text string, depth int) bool
	feed func(text string)
}

// maxDepthOrDefault mirrors the runner's bound for the posting guard.
func (b boardControl) depthLimit() int {
	if b.maxDepth > 0 {
		return b.maxDepth
	}
	return 2 // config.DefaultMaxDepth
}

var _ agent.BoardControl = boardControl{}

// sessionControl adapts the daemon's StopSession to agent.SessionControl.
type sessionControl struct{ daemon *Daemon }

func (s sessionControl) Stop(callerID, id string) error { return s.daemon.StopSession(id) }

// Post admits a task or refuses with the reason (§2.4): depth, queue bound,
// duplicate. Conversations post at depth 0; a process poster's depth is
// recorded on its process row (phase 3).
func (b boardControl) Post(agentID, title, spec string, priority int) (memory.BoardTask, error) {
	if b.store == nil {
		return memory.BoardTask{}, fmt.Errorf("board is not enabled here")
	}
	if b.depthLimit() <= 0 {
		return memory.BoardTask{}, fmt.Errorf("board_post: posting is disabled (max_depth 0)")
	}
	// A duplicate is not a new task: the alike check runs before the queue cap.
	if dup, ok, err := b.store.BoardTaskFindAlike(agentID, title, spec); err != nil {
		return memory.BoardTask{}, err
	} else if ok {
		return memory.BoardTask{}, fmt.Errorf("board_post: %s is already open as %s — read its result with board_get instead of posting it again", title, dup.ID)
	}
	if n, err := b.store.BoardTaskCountOpen(agentID); err != nil {
		return memory.BoardTask{}, err
	} else if n >= b.maxQueued {
		return memory.BoardTask{}, fmt.Errorf("board_post: %d open tasks is at your max_queued %d; finish or cancel some first", n, b.maxQueued)
	}
	t, err := b.store.BoardTaskCreate(title, spec, agentID, memory.BoardSourcePost, priority, 0)
	if err != nil {
		return memory.BoardTask{}, err
	}
	journalTransition(b.store, agentID, EvTaskPosted, map[string]any{
		"id": t.ID, "title": t.Title, "spec": clipDetail(t.Spec, 2000), "status": t.Status,
	})
	return t, nil
}

// List reads the board; reads are open to every session (§2.1).
func (b boardControl) List(_ string, status string, limit, offset int) ([]memory.BoardTask, error) {
	if b.store == nil {
		return nil, fmt.Errorf("board is not enabled here")
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	return b.store.BoardTaskList(status, limit, offset)
}

// Get reads one task.
func (b boardControl) Get(_ string, id string) (memory.BoardTask, error) {
	if b.store == nil {
		return memory.BoardTask{}, fmt.Errorf("board is not enabled here")
	}
	t, ok, err := b.store.BoardTaskGet(id)
	if err != nil {
		return memory.BoardTask{}, err
	}
	if !ok {
		return memory.BoardTask{}, fmt.Errorf("board task %q not found (board_list shows the ids)", id)
	}
	return t, nil
}

// Claim takes a queued task atomically and journals it.
func (b boardControl) Claim(agentID, id string) (memory.BoardTask, error) {
	if b.store == nil {
		return memory.BoardTask{}, fmt.Errorf("board is not enabled here")
	}
	t, ok, err := b.store.BoardTaskClaim(id, agentID)
	if err != nil {
		return memory.BoardTask{}, err
	}
	if !ok {
		return memory.BoardTask{}, fmt.Errorf("board_claim: %s is not queued (another worker may have taken it)", id)
	}
	journalTransition(b.store, agentID, EvTaskClaimed, map[string]any{"id": t.ID, "title": t.Title})
	return t, nil
}

// Complete finishes a running task with a result; only the claimant.
func (b boardControl) Complete(agentID, id, result string) (memory.BoardTask, error) {
	return b.finish(agentID, id, memory.BoardDone, result, EvTaskDone)
}

// Fail finishes a running task with a reason; only the claimant.
func (b boardControl) Fail(agentID, id, reason string) (memory.BoardTask, error) {
	return b.finish(agentID, id, memory.BoardFailed, reason, EvTaskFailed)
}

func (b boardControl) finish(agentID, id, status, text, evType string) (memory.BoardTask, error) {
	if b.store == nil {
		return memory.BoardTask{}, fmt.Errorf("board is not enabled here")
	}
	t, ok, err := b.store.BoardTaskFinish(id, agentID, status, text)
	if err != nil {
		return memory.BoardTask{}, err
	}
	if !ok {
		return memory.BoardTask{}, fmt.Errorf("board task %s is not running under you — claim it first, or it is already finished", id)
	}
	journalTransition(b.store, agentID, evType, map[string]any{"id": t.ID, "title": t.Title, "result": clipDetail(text, 2000)})
	// Result delivery (§2.3): a posted or detached task's result goes to its
	// poster as a restricted condition turn, framed as data (the pipe label);
	// a waited run_agent's result is the tool's return value already, so it is
	// not delivered again. A poster that is gone gets the human feed.
	if t.Source != memory.BoardSourceRunAgent && t.PostedBy != "" && t.PostedBy != agentID {
		labeled := pipeLabel(agentID, clipDetail(text, 2000))
		if b.wake != nil && b.wake(t.PostedBy, labeled, t.Depth+1) {
			return t, nil
		}
		if b.feed != nil {
			b.feed(fmt.Sprintf("[Board task %s finished by %s, poster %s not running] %s", t.ID, agentID, t.PostedBy, clipDetail(text, 500)))
		}
	}
	return t, nil
}

// RecordRun records a delegation's run on the board (adr §2.3: waited and
// posted work share one record). It creates the task queued under the parent
// and returns it; the child claims it and finishes it. source is
// "run_agent" for a waited call, "detached" for run_agent wait:false.
func (b boardControl) RecordRun(parentID, title, spec, source string) (memory.BoardTask, error) {
	if b.store == nil {
		return memory.BoardTask{}, fmt.Errorf("board is not enabled here")
	}
	t, err := b.store.BoardTaskCreate(title, spec, parentID, source, 0, 0)
	if err != nil {
		return memory.BoardTask{}, err
	}
	journalTransition(b.store, parentID, EvTaskPosted, map[string]any{
		"id": t.ID, "title": t.Title, "source": source, "status": t.Status,
	})
	return t, nil
}

// Cancel moves a task to cancelled; the poster (or, in phase 3, the operator)
// may cancel.
func (b boardControl) Cancel(agentID, id string) (memory.BoardTask, error) {
	if b.store == nil {
		return memory.BoardTask{}, fmt.Errorf("board is not enabled here")
	}
	t, ok, err := b.store.BoardTaskGet(id)
	if err != nil {
		return memory.BoardTask{}, err
	}
	if !ok {
		return memory.BoardTask{}, fmt.Errorf("board task %q not found (board_list shows the ids)", id)
	}
	if t.PostedBy != agentID {
		return memory.BoardTask{}, fmt.Errorf("board_cancel: %s was posted by %s; only the poster can cancel it", id, t.PostedBy)
	}
	ct, ok, err := b.store.BoardTaskCancel(id)
	if err != nil {
		return memory.BoardTask{}, err
	}
	if !ok {
		return memory.BoardTask{}, fmt.Errorf("board_cancel: %s is already in a terminal state (%s)", id, t.Status)
	}
	journalTransition(b.store, agentID, EvTaskCancelled, map[string]any{"id": ct.ID, "title": ct.Title})
	return ct, nil
}

// finishBoardTask closes the task a boardworker process claimed for its turn:
// done with the reply, failed with the error. Delivery to the poster rides
// finish() (§2.3).
func (r *StandingRunner) finishBoardTask(sessionID, taskID, reply string, turnErr error) {
	if r == nil || r.board == nil || taskID == "" {
		return
	}
	if turnErr != nil {
		if _, err := r.board.Fail(sessionID, taskID, turnErr.Error()); err != nil {
			slog.Warn("board: fail worker task", "task", taskID, "err", err)
		}
		return
	}
	if _, err := r.board.Complete(sessionID, taskID, reply); err != nil {
		slog.Warn("board: complete worker task", "task", taskID, "err", err)
	}
}
