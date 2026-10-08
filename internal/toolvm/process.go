package toolvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// A live process is a `js` tool started to run until it is stopped
// (adr/process-sessions.md): its instance survives across triggers, and it
// drives its session through nine:process — next() for the next trigger,
// turn() for a model turn, report() to a pipe. Everything else about it is an
// ordinary call: the same blob, the same grants, the same host functions.
//
// What differs is the bounds. A live instance has no call deadline; each host
// call it makes carries its own (an HTTP request its timeout, a model turn the
// turn's limits and the process's budget). Its work budget bounds what it does
// per trigger, refilled by next(). And it runs in its own pool, so a process
// waiting for a trigger never holds a slot an ordinary tool call needs.

// DefaultMaxLive is how many live instances may run at once when the operator
// sets no limit.
const DefaultMaxLive = 14

// Trigger is what next() returns to a live process.
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
	// Goal is the goal a goal-bound process works on, as it stands now.
	Goal *TriggerGoal `json:"goal,omitempty"`
}

// TriggerGoal is the goal a trigger carries to a goal-bound process.
type TriggerGoal struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// ProcessHandler is the host side of nine:process for one live process: the
// process runner implements it for the session the process drives.
type ProcessHandler interface {
	// Next blocks until the process's next trigger, or ctx ends.
	Next(ctx context.Context) (Trigger, error)
	// Turn runs one model turn in the process's session and returns the reply.
	Turn(ctx context.Context, text string) (string, error)
	// Report delivers text to the session the process pipes to, reporting
	// whether it was delivered.
	Report(ctx context.Context, text string) (bool, error)
}

// ErrStopped is what a ProcessHandler returns when the process has been
// stopped; the guest sees it as an E_STOPPED error.
var ErrStopped = errors.New("process stopped")

// ErrBudget is what a ProcessHandler returns from Turn when the process's
// budget is spent; the guest sees it as an E_BUDGET error.
var ErrBudget = errors.New("process budget spent")

// processKey carries a live instance's ProcessHandler into the host function.
// An ordinary call has none, which is what makes nine:process refuse it.
type processKey struct{}

type processRequest struct {
	Op   string `json:"op"`
	Text string `json:"text,omitempty"`
}

type processResponse struct {
	Trigger   *Trigger `json:"trigger,omitempty"`
	Reply     string   `json:"reply,omitempty"`
	Delivered bool     `json:"delivered,omitempty"`
	Error     string   `json:"error,omitempty"`
	Code      string   `json:"code,omitempty"`
}

// hostProcess is the guest's `nine:process`. Every op is refused unless this
// instance was started by StartLive: a tool the model calls must never block
// in next().
func (h *Host) hostProcess(ctx context.Context, mod api.Module, ptr, size uint32) uint64 {
	handler, _ := ctx.Value(processKey{}).(ProcessHandler)
	if handler == nil {
		return writeProcess(ctx, mod, processResponse{
			Error: "nine:process is for live processes; this tool was called, not started as one",
			Code:  "E_NOT_LIVE"})
	}
	raw, ok := mod.Memory().Read(ptr, size)
	if !ok {
		return writeProcess(ctx, mod, processResponse{Error: "unreadable request"})
	}
	var req processRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return writeProcess(ctx, mod, processResponse{Error: "malformed request: " + err.Error()})
	}

	switch req.Op {
	case "next":
		trig, err := handler.Next(ctx)
		if err != nil {
			return writeProcess(ctx, mod, processFailure(ctx, err))
		}
		// A new trigger refills the work budget: it bounds the work per
		// trigger, not over the instance's whole life.
		if reset := mod.ExportedFunction("nine_budget_reset"); reset != nil {
			if _, err := reset.Call(ctx); err != nil {
				slog.Warn("live process: work budget reset failed", "err", err)
			}
		}
		return writeProcess(ctx, mod, processResponse{Trigger: &trig})
	case "turn":
		reply, err := handler.Turn(ctx, req.Text)
		if err != nil {
			return writeProcess(ctx, mod, processFailure(ctx, err))
		}
		return writeProcess(ctx, mod, processResponse{Reply: reply})
	case "report":
		delivered, err := handler.Report(ctx, req.Text)
		if err != nil {
			return writeProcess(ctx, mod, processFailure(ctx, err))
		}
		return writeProcess(ctx, mod, processResponse{Delivered: delivered})
	default:
		return writeProcess(ctx, mod, processResponse{Error: fmt.Sprintf("unknown process op %q", req.Op)})
	}
}

// processFailure turns a handler error into the guest's error, with a code the
// guest can branch on. A stop shows as E_STOPPED whichever way it arrived.
func processFailure(ctx context.Context, err error) processResponse {
	switch {
	case errors.Is(err, ErrStopped) || ctx.Err() != nil:
		return processResponse{Error: "process stopped", Code: "E_STOPPED"}
	case errors.Is(err, ErrBudget):
		return processResponse{Error: err.Error(), Code: "E_BUDGET"}
	default:
		return processResponse{Error: err.Error(), Code: "E_PROCESS"}
	}
}

func writeProcess(ctx context.Context, mod api.Module, resp processResponse) uint64 {
	out, err := json.Marshal(resp)
	if err != nil {
		out = []byte(`{"error":"cannot encode the process response"}`)
	}
	return writeGuestBytes(ctx, mod, out)
}

// Live is one running live process.
type Live struct {
	cancel context.CancelFunc
	done   chan struct{}
	out    Output
	err    error
}

// Stop ends the process: its pending next() or turn() fails with E_STOPPED and
// the instance is closed. Stop does not wait; Wait does.
func (l *Live) Stop() { l.cancel() }

// Done is closed when the process has ended, by returning, failing or Stop.
func (l *Live) Done() <-chan struct{} { return l.done }

// Wait blocks until the process ends and returns what its program returned,
// or why it failed. A stopped process reports ErrStopped.
func (l *Live) Wait() (Output, error) {
	<-l.done
	return l.out, l.err
}

// StartLive starts tool name as a live process driven by handler, with args as
// its program's arguments. It returns once the instance is running; the program
// runs until it returns, fails, or Stop is called.
//
// It takes a slot of the live pool, not of the call pool, and refuses when the
// live pool is full rather than queuing: the process runner decides what waits.
func (h *Host) StartLive(ctx context.Context, name string, args json.RawMessage, handler ProcessHandler) (*Live, error) {
	t := h.Get(name)
	if t == nil {
		return nil, fmt.Errorf("unknown sandboxed tool %q", name)
	}
	if t.Kind != KindJS {
		return nil, fmt.Errorf("tool %q is not a js tool; only a js tool can run as a live process", name)
	}
	if handler == nil {
		return nil, errors.New("a live process needs a process handler")
	}
	select {
	case h.live <- struct{}{}:
	default:
		return nil, fmt.Errorf("cannot start %q: %d live processes are already running", name, cap(h.live))
	}

	// No deadline: the instance lives until it is stopped. Cancelling this
	// context is the stop — wazero closes the module out from under the guest.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ctx, printed := withCallContext(ctx, t)
	ctx = context.WithValue(ctx, processKey{}, handler)

	input, err := t.inputForJob(args, nil)
	if err != nil {
		cancel()
		<-h.live
		return nil, err
	}
	mod, err := h.rt.InstantiateModule(ctx, t.module, h.moduleConfig(t))
	if err != nil {
		cancel()
		<-h.live
		return nil, fmt.Errorf("live process %q failed to start: %w", name, err)
	}

	l := &Live{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(l.done)
		defer func() { <-h.live }()
		defer mod.Close(context.WithoutCancel(ctx)) //nolint:errcheck // teardown of an ended instance

		out, err := callGuest(ctx, mod, input, true)
		switch {
		case ctx.Err() != nil:
			l.err = ErrStopped
		case err != nil:
			l.err = withLogs(fmt.Errorf("live process %q: %w", name, err), printed)
		default:
			l.out, l.err = h.output(t, out, len(input), printed)
		}
		cancel()
	}()
	return l, nil
}
