package runtime

import (
	"fmt"
	"log/slog"
	"time"

	"nine/internal/config"
	"nine/internal/memory"
	"nine/internal/toolvm"
)

// Budgets (adr/process-sessions.md §5, §6). Every process has one: the model
// turns its live program runs through turn() over a rolling day, and the
// tokens those turns spend. A turn the budget no longer covers is refused with
// E_BUDGET, the process is paused — stopped by its budget — and the pause
// reaches the human feed. The day starts at the first counted turn; when it is
// over, the process runs again on its own, since the pause was the budget's
// and not anyone's decision.

// budgetDay is how long a budget's period lasts from its first counted turn.
const budgetDay = 24 * time.Hour

// Journal event types of a budget's pause and resume, alongside the standing
// transitions (standing_observe.go).
const (
	evProcessPaused  = "process_paused"
	evProcessResumed = "process_resumed"
	evProcessDeleted = "process_deleted"
)

// SetBudget sets [processes] budget, the ceiling every process's own budget
// sits under. Unset fields take their defaults.
func (r *StandingRunner) SetBudget(b config.BudgetConfig) {
	if r != nil {
		r.budget = config.ProcessesConfig{Budget: b}.BudgetOrDefault()
	}
}

// budgetOf returns p's limits: what its definition sets, under the ceiling.
func (r *StandingRunner) budgetOf(p memory.Process) config.BudgetConfig {
	ceiling := config.ProcessesConfig{Budget: r.budget}.BudgetOrDefault()
	return config.BudgetConfig{TurnsPerDay: p.BudgetTurns, TokensPerDay: p.BudgetTokens}.Within(ceiling)
}

// budgetResetAt returns when p's budget day ends, and false when none is under
// way.
func budgetResetAt(p memory.Process) (time.Time, bool) {
	if p.UsageSince == "" {
		return time.Time{}, false
	}
	since, err := time.Parse(time.RFC3339Nano, p.UsageSince)
	if err != nil {
		return time.Time{}, false
	}
	return since.Add(budgetDay), true
}

// spendTurn decides whether process id may run one more turn. A day that is
// over starts again from zero. A process its budget no longer covers is paused
// and the turn refused with toolvm.ErrBudget.
func (r *StandingRunner) spendTurn(id string, now time.Time) error {
	p, ok, err := r.store.ProcessGet(id)
	if err != nil || !ok {
		// A read failure must not stop a process, nor charge it.
		return nil
	}
	if at, ok := budgetResetAt(p); ok && !at.After(now) {
		if err := r.store.ProcessUsageReset(id); err != nil {
			slog.Warn("process budget: start a new day", "id", id, "err", err)
		}
		return nil
	}
	lim := r.budgetOf(p)
	if p.UsageTurns < lim.TurnsPerDay && p.UsageTokens < lim.TokensPerDay {
		return nil
	}
	r.pauseForBudget(p, lim)
	return toolvm.ErrBudget
}

// chargeTurn counts one turn of process id and the tokens it spent.
func (r *StandingRunner) chargeTurn(id string, tokens int) {
	if err := r.store.ProcessUsageAdd(id, tokens); err != nil {
		slog.Warn("process budget: count a turn", "id", id, "err", err)
	}
}

// pauseForBudget stops p for its budget and tells the human feed when it runs
// again. The next pass closes its instance.
func (r *StandingRunner) pauseForBudget(p memory.Process, lim config.BudgetConfig) {
	if _, err := r.store.ProcessStop(p.ID, "budget"); err != nil {
		slog.Warn("process budget: pause", "id", p.ID, "err", err)
		return
	}
	resume := "when its day is over"
	if at, ok := budgetResetAt(p); ok {
		resume = "at " + at.UTC().Format(time.RFC3339)
	}
	journalTransition(r.store, p.ID, evProcessPaused, map[string]any{
		"tool": p.Tool, "by": "budget",
		"turns": p.UsageTurns, "turns_per_day": lim.TurnsPerDay,
		"tokens": p.UsageTokens, "tokens_per_day": lim.TokensPerDay,
	})
	r.notifyHuman(fmt.Sprintf(
		"Process %s spent its budget for the day (%d of %d turns, %d of %d tokens) and is paused. It runs again %s.",
		p.ID, p.UsageTurns, lim.TurnsPerDay, p.UsageTokens, lim.TokensPerDay, resume))
	r.log.add(p.ID, "paused", "budget spent")
	slog.Info("process paused by its budget", "id", p.ID,
		"turns", p.UsageTurns, "tokens", p.UsageTokens)
	r.Wake()
}

// resumeBudgets runs again every process its budget paused whose day is over.
func (r *StandingRunner) resumeBudgets(now time.Time) {
	paused, err := r.store.ProcessesStoppedBy("budget")
	if err != nil {
		slog.Warn("process budget: list paused", "err", err)
		return
	}
	for _, p := range paused {
		if at, ok := budgetResetAt(p); ok && at.After(now) {
			continue
		}
		if err := r.store.ProcessUsageReset(p.ID); err != nil {
			slog.Warn("process budget: start a new day", "id", p.ID, "err", err)
			continue
		}
		if _, err := r.store.ProcessSetState(p.ID, memory.ProcessRunning); err != nil {
			slog.Warn("process budget: resume", "id", p.ID, "err", err)
			continue
		}
		journalTransition(r.store, p.ID, evProcessResumed, map[string]any{"tool": p.Tool, "by": "budget"})
		r.notifyHuman(fmt.Sprintf("Process %s has a new budget day and is running again.", p.ID))
		r.log.add(p.ID, "resumed", "new budget day")
	}
}
