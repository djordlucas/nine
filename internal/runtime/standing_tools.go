package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"nine/internal/config"
	"nine/internal/cron"
	"nine/internal/memory"
	"nine/internal/toolvm"
)

// Health bounds for a standing run.
const (
	// StandingFailureThreshold is how many consecutive failures trip the breaker
	// into `failing`. Three rather than one: a watcher whose endpoint blips should
	// not need an operator, and three in a row is no longer a blip.
	StandingFailureThreshold = 3

	// StandingMaxBackoff caps the backed-off cadence. A broken tool should stop
	// burning the cadence it asked for, but it should also still be retrying when
	// someone fixes the thing it depends on.
	StandingMaxBackoff = 30 * time.Minute
)

// StandingRunner drives standing tools: resumable tools the daemon runs
// indefinitely on their own cadence (adr/standing-tools.md).
//
// It is the same engine as the job driver — call, take the cursor, wait, call
// again — under a different lifecycle. The differences that matter:
//
//   - **A cycle, not a job.** Calls run until the tool returns a result instead
//     of asking to continue. That completes one *cycle*: the cursor resets and
//     the trigger decides when the next cycle starts. So there are two cadences,
//     the trigger between cycles and after_ms within one.
//   - **No terminal state.** A standing run is stopped or it is going.
//   - **Nobody's turn.** There is no conversation to notify, so a cycle that
//     produces output posts to the human feed and one that returns nothing is
//     silent — which is what makes a watcher usable rather than a notification
//     storm.
type StandingRunner struct {
	store    *memory.Store
	host     *toolvm.Host
	minDelay time.Duration
	workers  int
}

// NewStandingRunner builds the driver, or nil when there is no sandboxed-tool
// host — which keeps the feature additive for a deployment with [tools] unset.
func NewStandingRunner(store *memory.Store, host *toolvm.Host, minDelayMS, workers int) *StandingRunner {
	if store == nil || host == nil {
		return nil
	}
	if minDelayMS <= 0 {
		minDelayMS = DefaultJobMinDelayMS
	}
	if workers <= 0 {
		workers = DefaultJobWorkers
	}
	return &StandingRunner{
		store:    store,
		host:     host,
		minDelay: time.Duration(minDelayMS) * time.Millisecond,
		workers:  workers,
	}
}

// ReconcileStandingTools brings the store's definitions in line with the file.
//
// Config owns the definition; the runtime owns the run state. So this writes
// tool, args and trigger, and deliberately does not touch state, cursor, or
// failures — editing nine.toml adjusts what a standing tool does without
// restarting one an operator stopped, exactly as a standing agent's config edit
// does not reactivate a finished goal (docs/predefined-agents.md).
//
// A block whose `args` changed has its cycle restarted: the cursor it holds was
// produced under the old arguments, and resuming with it would be incoherent.
//
// Removing a block stops Nine reconciling it; it does not delete the row, so its
// history stays readable and an operator who deleted a line by accident has not
// lost anything.
func ReconcileStandingTools(store *memory.Store, blocks []config.StandingToolConfig) {
	if store == nil {
		return
	}
	for _, b := range blocks {
		args, err := json.Marshal(orEmptyArgs(b.Args))
		if err != nil {
			slog.Error("standing tool: cannot encode args", "id", b.ID, "err", err)
			continue
		}
		interval := 0
		if b.Interval != "" {
			d, err := time.ParseDuration(b.Interval)
			if err != nil {
				// Validate already refused this; reaching here means a caller skipped it.
				slog.Error("standing tool: bad interval", "id", b.ID, "interval", b.Interval)
				continue
			}
			interval = int(d.Seconds())
		}

		prev, existed, err := store.StandingToolGet(b.ID)
		if err != nil {
			slog.Warn("standing tool: read", "id", b.ID, "err", err)
			continue
		}
		if err := store.StandingToolUpsertDefinition(memory.StandingTool{
			ID: b.ID, Tool: b.Tool, Args: string(args),
			IntervalSecs: interval, Schedule: b.Schedule,
		}); err != nil {
			slog.Warn("standing tool: reconcile", "id", b.ID, "err", err)
			continue
		}

		switch {
		case !existed:
			// New: honour the block's own enabled flag.
			state := memory.StandingRunning
			if !b.IsEnabled() {
				state = memory.StandingStopped
			}
			if _, err := store.StandingToolSetState(b.ID, state); err != nil {
				slog.Warn("standing tool: initial state", "id", b.ID, "err", err)
			}
			slog.Info("standing tool declared", "id", b.ID, "tool", b.Tool, "state", state)
		case prev.Args != string(args):
			// The cursor belongs to the old arguments; start the next cycle clean.
			if prev.State != memory.StandingStopped {
				if _, err := store.StandingToolSetState(b.ID, memory.StandingRunning); err != nil {
					slog.Warn("standing tool: restart after args change", "id", b.ID, "err", err)
				}
			}
			slog.Info("standing tool arguments changed; its cycle restarts", "id", b.ID)
		}
	}
}

func orEmptyArgs(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// RunStandingTools drives every due standing tool until ctx is cancelled.
//
// It shares the sweeper's cadence and its worker budget rather than taking its
// own: what both bound is the same scarce thing — concurrent wasm instantiations,
// each holding up to [tools] memory_mb.
func RunStandingTools(ctx context.Context, r *StandingRunner, interval time.Duration) {
	if r == nil {
		return
	}
	if interval <= 0 {
		interval = DefaultJobPollSeconds * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.runDue(ctx)
		}
	}
}

// runDue makes one call for each due standing tool, up to `workers` at a time.
//
// It waits for the batch, for the reason the job driver does: the ticker drops
// ticks while a sweep runs, so waiting is what stops a second call of the same
// standing tool starting against a cursor the first has not finished with.
func (r *StandingRunner) runDue(ctx context.Context) {
	due, err := r.store.StandingToolsDue()
	if err != nil {
		slog.Warn("standing tools: list due", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}

	sem := make(chan struct{}, r.workers)
	var wg sync.WaitGroup
	for _, st := range due {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(st memory.StandingTool) {
			defer wg.Done()
			defer func() { <-sem }()
			r.runOnce(ctx, st)
		}(st)
	}
	wg.Wait()
}

// runOnce makes a single call of one standing tool and writes back what happened.
func (r *StandingRunner) runOnce(ctx context.Context, st memory.StandingTool) {
	out, err := r.host.CallJob(ctx, st.Tool, json.RawMessage(argsOrEmptyString(st.Args)),
		toolvm.JobContext{Cursor: st.Cursor, Call: st.Calls + 1})
	if err != nil {
		r.recordFailure(st, err)
		return
	}

	// Still working: schedule the next call at the delay it asked for.
	if out.Continue != nil {
		next := time.Now().Add(max(time.Duration(out.Continue.AfterMS)*time.Millisecond, r.minDelay))
		if _, err := r.store.StandingToolAdvance(st.ID, out.Continue.Cursor, next); err != nil {
			slog.Warn("standing tool: advance", "id", st.ID, "err", err)
		}
		return
	}

	// The cycle finished. The trigger decides when the next one starts.
	next := r.nextCycleAt(st, time.Now())
	if _, err := r.store.StandingToolCompleteCycle(st.ID, next); err != nil {
		slog.Warn("standing tool: complete cycle", "id", st.ID, "err", err)
		return
	}
	r.report(st, out)

	if st.State == memory.StandingFailing {
		// Recovered: worth one line, on the edge rather than per call.
		slog.Info("standing tool recovered", "id", st.ID, "tool", st.Tool)
		r.notifyHuman(fmt.Sprintf("Standing tool %s recovered and is running normally again.", st.ID))
	}
}

// report delivers a cycle's output.
//
// **Empty output means nothing to report, and stays silent.** That convention is
// what makes a watcher usable: a tool that runs every ten seconds and speaks only
// when it finds something is useful, and one that announces every pass is a
// notification storm nobody reads.
//
// The human feed is the only destination in v1. A standing tool cannot address an
// agent, which keeps a deterministic tool from steering an autonomous one with
// no human in between; and it cannot wake anything, which is R-SUB.3's
// enrich-don't-interject applied unchanged.
func (r *StandingRunner) report(st memory.StandingTool, out toolvm.Output) {
	text := out.Text
	if out.Bytes != nil {
		slog.Warn("standing tool returned bytes, which the human feed cannot carry",
			"id", st.ID, "bytes", len(out.Bytes))
		return
	}
	if text == "" {
		return
	}
	r.notifyHuman(fmt.Sprintf("[%s] %s", st.ID, text))
}

func (r *StandingRunner) notifyHuman(msg string) {
	if err := r.store.UserNotificationCreate(memory.NewID(), "", msg); err != nil {
		slog.Warn("standing tool: notify", "err", err)
	}
}

// recordFailure applies the backoff and the circuit breaker.
//
// The failure that actually happens with this feature is not a crash: it is a
// tool that throws on every call for a week while nobody notices. So failures
// back off (a broken tool stops burning the cadence it asked for), the breaker
// makes `failing` visible in the roster, and the transition — not every failure —
// posts to the human feed, so a flapping tool cannot produce a storm.
func (r *StandingRunner) recordFailure(st memory.StandingTool, cause error) {
	failures := st.Failures + 1
	state := st.State
	tripped := failures >= StandingFailureThreshold && st.State != memory.StandingFailing
	if failures >= StandingFailureThreshold {
		state = memory.StandingFailing
	}

	next := time.Now().Add(r.backoff(st, failures))
	if _, err := r.store.StandingToolFail(st.ID, cause.Error(), next, state); err != nil {
		slog.Warn("standing tool: record failure", "id", st.ID, "err", err)
		return
	}
	slog.Warn("standing tool call failed",
		"id", st.ID, "tool", st.Tool, "consecutive", failures, "err", cause)

	if tripped {
		r.notifyHuman(fmt.Sprintf(
			"Standing tool %s has failed %d times in a row and is backing off. Last error: %s",
			st.ID, failures, oneLine(cause.Error())))
	}
}

// backoff doubles the tool's own cadence per consecutive failure, capped.
func (r *StandingRunner) backoff(st memory.StandingTool, failures int) time.Duration {
	base := time.Duration(st.IntervalSecs) * time.Second
	if base <= 0 {
		base = time.Minute // a cron-triggered tool has no interval to double
	}
	// 2^(failures-1), guarded against overflow on a long-broken tool.
	shift := min(failures-1, 20)
	d := base * time.Duration(math.Pow(2, float64(shift)))
	return min(max(d, r.minDelay), StandingMaxBackoff)
}

// nextCycleAt reduces the trigger to an instant, reusing the parsing standing
// agents use (docs/scheduling.md) so the two cannot disagree about what a cron
// expression means.
func (r *StandingRunner) nextCycleAt(st memory.StandingTool, now time.Time) time.Time {
	if st.Schedule != "" {
		sched, err := cron.Parse(st.Schedule)
		if err != nil {
			slog.Warn("standing tool: bad schedule", "id", st.ID, "schedule", st.Schedule, "err", err)
			return now.Add(time.Hour)
		}
		next := sched.Next(now)
		if next.IsZero() {
			// A schedule that can never fire. Park it rather than spin.
			return now.Add(24 * time.Hour)
		}
		return next
	}
	d := time.Duration(st.IntervalSecs) * time.Second
	return now.Add(max(d, r.minDelay))
}

func argsOrEmptyString(args string) string {
	if args == "" {
		return "{}"
	}
	return args
}
