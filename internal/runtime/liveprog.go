package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Live programs (adr/process-sessions.md §2, adr/modular_nine.md §9.10): the
// toolvm is gone, so the two shipped process programs — pursue and reflect —
// are Go loops here, driven through the same ProcessHandler the runner
// implements. A live program waits for triggers with Next, runs model turns
// with Turn, and hands results on with Report. Ordinary tool calls never see
// this: only programs started by the runner run.

// Trigger is what Next returns to a live program.
type Trigger struct {
	// Kind is "clock", "event" or "message".
	Kind string    `json:"kind"`
	At   time.Time `json:"at"`
	// Text is a message's text, or a pipe's report.
	Text string `json:"text,omitempty"`
	// Event is the journal event an event trigger carries.
	Event json.RawMessage `json:"event,omitempty"`
	// From names who sent a message: an operator, a session, or a process.
	From string `json:"from,omitempty"`
	// Piped marks a message a process's pipe delivered, as opposed to one a
	// person sent; the turn it causes is restricted. The program does not see it.
	Piped bool `json:"-"`
	// Depth is the trigger's lineage depth; the turns it causes journal one
	// deeper. The program does not see it.
	Depth int `json:"-"`
	// Goal is the goal a goal-bound process works on, as it stands now.
	Goal *TriggerGoal `json:"goal,omitempty"`
	// Activity is, for a reflect process's clock tick, a digest of what the
	// journal recorded since its last reflection.
	Activity string `json:"activity,omitempty"`
	// BoardTask is set on a boardworker process's clock tick when the runner
	// claimed a queued board task for it: the task id, so the turn's outcome
	// finishes the task (adr/orchestrator-board.md §5 P3).
	BoardTask string `json:"-"`
}

// TriggerGoal is the goal a trigger carries to a goal-bound process.
type TriggerGoal struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	// Schedule is how often a recurring goal runs; empty for a goal worked on
	// until it is done.
	Schedule string `json:"schedule,omitempty"`
	// Folder is the workspace folder for what the goal produces, ending in a
	// slash (GoalFolder).
	Folder string `json:"folder,omitempty"`
}

// ProcessHandler is the runner side of a live program: the session the program
// drives.
type ProcessHandler interface {
	// Next blocks until the process's next trigger, or ctx ends.
	Next(ctx context.Context) (Trigger, error)
	// Turn runs one model turn in the process's session and returns the reply.
	Turn(ctx context.Context, text string) (string, error)
	// Report delivers text to the session the process pipes to, reporting
	// whether it was delivered.
	Report(ctx context.Context, text string) (bool, error)
}

// ProcessToolCaller calls one tool by name on a process's behalf — a standing
// tool's tick. The daemon wires it to the core dispatcher (Go builtins) and the
// plugin manager (MCP plugins).
type ProcessToolCaller interface {
	CallProcessTool(ctx context.Context, tool string, args json.RawMessage) (string, error)
}

// ErrStopped is what a ProcessHandler returns when the process has been
// stopped.
var ErrStopped = errors.New("process stopped")

// Live is one running live program.
type Live struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// Stop ends the program: its pending Next or Turn fails with ErrStopped.
func (l *Live) Stop() { l.cancel() }

// Wait blocks until the program ends and returns why.
func (l *Live) Wait() error {
	<-l.done
	return l.err
}

// Program is one live program: a loop over Next/Turn until stopped.
type Program func(ctx context.Context, h ProcessHandler) error

// livePrograms is the registry of built-in live programs. pursue drives goal
// sessions and standing agents; reflect drives self-reflection. These were the
// two Live shipped tools; everything else that ran in the sandbox is gone with
// the toolvm (adr/modular_nine.md §9.10).
var livePrograms = map[string]Program{
	"pursue":       pursueProgram,
	"reflect":      reflectProgram,
	"boardworker":  boardWorkerProgram,
}

// LiveProgram reports whether name is a built-in live program.
func LiveProgram(name string) bool {
	_, ok := livePrograms[name]
	return ok
}

// startProgram runs name as a live program driven by h, returning once the
// program is running; it runs until it returns, fails, or Stop is called.
func startProgram(ctx context.Context, name string, h ProcessHandler) (*Live, error) {
	prog, ok := livePrograms[name]
	if !ok {
		return nil, fmt.Errorf("unknown live program %q", name)
	}
	if h == nil {
		return nil, errors.New("a live process needs a process handler")
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	l := &Live{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		err := prog(ctx, h)
		switch {
		case ctx.Err() != nil:
			l.err = ErrStopped
		case errors.Is(err, ErrStopped):
			l.err = ErrStopped
		case err != nil:
			l.err = fmt.Errorf("live process %q: %w", name, err)
		default:
			l.err = nil
		}
		cancel()
	}()
	return l, nil
}

// --- pursue — the process behind every goal session and standing agent
// (adr/process-sessions.md §4), ported from the shipped pursue.js. It is bound
// to its goal: Nine starts it while the goal is active and stops it otherwise,
// and hands it the goal with each clock tick. A tick asks the session to work
// on the goal; a message — a condition trigger's finding, piped to this
// session — is put to the session as it is.

func pursueProgram(ctx context.Context, h ProcessHandler) error {
	for {
		t, err := h.Next(ctx)
		if err != nil {
			return err
		}
		if t.Kind == "clock" && t.Goal != nil {
			if _, err := h.Turn(ctx, pursuePrompt(t.Goal)); err != nil {
				return err
			}
		} else if t.Kind == "message" && t.Text != "" {
			if _, err := h.Turn(ctx, t.Text); err != nil {
				return err
			}
		}
	}
}

func pursuePrompt(g *TriggerGoal) string {
	if g.Schedule != "" {
		return recurringGoalPrompt(g)
	}
	return ongoingGoalPrompt(g)
}

func goalFolderNote(g *TriggerGoal) string {
	if g.Folder == "" {
		return ""
	}
	return fmt.Sprintf(" Unless the goal names where its output goes, keep what it produces in %s: "+
		"look there first for what earlier turns wrote, and update those files rather than writing new copies elsewhere. "+
		"Earlier turns are not in your context: %snotes.md says what they did, if it exists. "+
		"Before you end, rewrite it in a few lines: what you did, what you found, what is left.", g.Folder, g.Folder)
}

func ongoingGoalPrompt(g *TriggerGoal) string {
	return fmt.Sprintf("Check on goal %s (%q) and its subtree (goal_get). "+
		"Take any useful action toward it, including spawning sub-goals or sub-agents. "+
		"If its status should change (e.g. done once attained, or paused if you're stuck), call goal_update_status.", g.ID, g.Description) +
		goalFolderNote(g)
}

func recurringGoalPrompt(g *TriggerGoal) string {
	cadence := fmt.Sprintf("on the cron schedule %q (UTC)", g.Schedule)
	if len(g.Schedule) > 0 && isSimpleCadence(g.Schedule) {
		cadence = "every " + g.Schedule
	}
	return fmt.Sprintf("This is a scheduled run of recurring goal %s (%q), which runs %s. "+
		"Do the whole job now: find what is new since the last run, then write or update its result. "+
		"Once the result is written and the notes below are updated, end the turn. "+
		"A recurring goal is never done: leave its status active, unless it cannot go on (paused) or should stop for good (archived).",
		g.ID, g.Description, cadence) + goalFolderNote(g)
}

// isSimpleCadence reports whether s is the <n><unit> form (120s, 30m, 12h).
func isSimpleCadence(s string) bool {
	if len(s) < 2 {
		return false
	}
	unit := s[len(s)-1]
	if unit != 's' && unit != 'm' && unit != 'h' {
		return false
	}
	for _, r := range s[:len(s)-1] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// --- reflect — the process behind self-reflection (adr/process-sessions.md
// §4), ported from the shipped reflect.js: on each clock tick, ask the session
// to reflect and update its self-model. Nine hands each tick a digest of what
// the journal recorded since the last reflection, and skips the tick when
// nothing happened.

func reflectProgram(ctx context.Context, h ProcessHandler) error {
	for {
		t, err := h.Next(ctx)
		if err != nil {
			return err
		}
		if t.Kind == "clock" {
			if _, err := h.Turn(ctx, reflectPrompt(t.Activity)); err != nil {
				return err
			}
		}
	}
}

// boardWorkerProgram is the process-backed board worker
// (adr/orchestrator-board.md §5 P3): a live process whose clock tick claims the
// oldest queued board task — the runner does the claiming in Next and stuffs
// the spec into the trigger's Text — and runs one turn on it. The outcome
// finishes the task (process_live.go Turn). With nothing queued, the tick is
// skipped and costs nothing.
func boardWorkerProgram(ctx context.Context, h ProcessHandler) error {
	for {
		t, err := h.Next(ctx)
		if err != nil {
			return err
		}
		if t.Text != "" {
			if _, err := h.Turn(ctx, t.Text); err != nil {
				return err
			}
		}
	}
}

func reflectPrompt(activity string) string {
	head := "Reflect on your recent sessions.\n"
	if activity != "" {
		head = "Since your last reflection, the journal recorded:\n" + activity + "\n"
	}
	return head +
		"Use memory_set to update:\n" +
		"- `self/capabilities`: a concise description of what you can currently do\n" +
		"- `self/learned`: append a short dated entry with what this activity teaches about how you work: " +
		"what failed and why, what wasted effort or tokens, what worked. Skip what earlier entries already say.\n" +
		"Be brief and factual. Do not ask questions."
}
