package runtime

import (
	"fmt"
	"log/slog"
	"time"
)

// Idle backoff (docs/goal-sessions.md): a goal session's clock wakes it to work
// on its goal, and once the work is done a wake finds nothing to do. Each such
// wake still pays for a full model call over the session's context, so a goal
// left active burns its budget re-reading itself. The runner counts the clock
// turns in a row that only looked, and from the second one waits twice as long
// before the next tick, up to IdleBackoffCap. A turn that acts, or a message or
// event the process is given, brings the cadence back.

// IdleBackoffAfter is how many clock turns in a row may change nothing before
// the clock slows down.
const IdleBackoffAfter = 2

// IdleBackoffCap is the longest an idle goal session waits between ticks,
// unless its own interval is longer.
const IdleBackoffCap = 6 * time.Hour

// lookTools only read: a turn that calls nothing else changed nothing. A tool
// missing from this list counts as acting, so a new tool can only make the
// backoff rarer, never hide work.
var lookTools = map[string]bool{
	"goal_get": true, "goal_list": true,
	"memory_get": true, "memory_list": true, "memory_query": true,
	"read_file": true, "list_files": true, "file_search_text": true, "diff_file": true, "trash_list": true,
	"skill_list": true, "skill_read": true, "skill_search": true,
	"doc_read": true, "doc_search": true,
	"tool_list": true, "tool_search": true, "tool_read": true,
	"time": true, "job_check": true, "job_list": true,
	"workflow_list": true, "process_list": true,
	"queued_messages_get": true, "queued_messages_count": true, "queued_messages_unconsumed_count": true,
}

// turnActivity records whether a turn called a tool that is not lookTools.
type turnActivity struct{ acted bool }

func (a *turnActivity) onTool(name string) {
	if !lookTools[name] {
		a.acted = true
	}
}

// idleBackoff is the wait before the next tick after streak idle clock turns
// in a row: the interval until the streak reaches IdleBackoffAfter, then
// doubling with each idle turn, up to the cap.
func idleBackoff(interval time.Duration, streak int) time.Duration {
	if streak < IdleBackoffAfter {
		return interval
	}
	limit := max(IdleBackoffCap, interval)
	d := interval
	for i := IdleBackoffAfter; i <= streak; i++ {
		d *= 2
		if d >= limit {
			return limit
		}
	}
	return d
}

// backsOff reports whether a process's clock slows when it idles: a goal
// session on a fixed interval. A cron schedule is a cadence the operator set,
// and a process that is not bound to a goal may watch on purpose.
func backsOff(lp *liveProc) bool {
	return lp.row.GoalID != "" && lp.row.Schedule == "" && lp.row.IntervalSecs > 0
}

// noteTurn updates the idle streak after a turn the given trigger label
// caused, and when the clock should slow, moves the next tick back.
func (r *StandingRunner) noteTurn(lp *liveProc, label string, acted bool, now time.Time) {
	if !backsOff(lp) {
		return
	}
	lp.mu.Lock()
	switch {
	case acted || label != "idle":
		lp.idleStreak = 0
	default:
		lp.idleStreak++
	}
	streak := lp.idleStreak
	lp.mu.Unlock()
	if streak < IdleBackoffAfter {
		return
	}
	wait := idleBackoff(time.Duration(lp.row.IntervalSecs)*time.Second, streak)
	if err := r.store.ProcessSetNextAt(lp.row.ID, now.Add(wait)); err != nil {
		slog.Warn("live process: slow its idle clock", "id", lp.row.ID, "err", err)
		return
	}
	r.log.add(lp.row.ID, "idle", fmt.Sprintf("%d turns in a row changed nothing; next tick in %s", streak, wait))
}
