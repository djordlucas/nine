package runtime

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"nine/internal/memory"
	"nine/internal/protocol"
)

// errHumanTimeout is returned to the model when a question goes unanswered. It
// is a tool failure, not turn cancellation — a later tool call may still run.
var errHumanTimeout = errors.New("no response from human (timed out)")

// HITLStore is the slice of memory.Store the HITL coordinator persists through.
type HITLStore interface {
	HumanRequestCreate(id, agentID, question string, options []string, expiresAt time.Time) error
	HumanRequestGetPending(agentID string) (*memory.HumanRequest, error)
	HumanRequestAnswer(id, answer string) error
	HumanRequestMarkTimedOut(id string) error
	HumanRequestExpireStale(now time.Time) error
	InteractiveSessionAdd(id string) error
	InteractiveSessionExists(id string) (bool, error)
}

// HITL coordinates human-in-the-loop requests. It bridges the blocking
// ask_human tool call (running on a session worker goroutine) and the
// human_input_answer message (arriving on a separate connection): ask_human
// registers a one-shot answer slot and blocks; the answer dispatch finds the
// slot by request ID and delivers into it.
type HITL struct {
	store   HITLStore
	timeout time.Duration

	mu      sync.Mutex
	pending map[string]chan string // requestID → answer slot
	emit    func(agentID string, msg protocol.Msg)
}

// NewHITL returns a HITL coordinator. timeout is how long an unanswered
// question waits before timing out; values <= 0 default to 5 minutes. Call
// SetEmit before any interactive session can run.
func NewHITL(store HITLStore, timeout time.Duration) *HITL {
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	return &HITL{
		store:   store,
		timeout: timeout,
		pending: make(map[string]chan string),
	}
}

// SetEmit registers the function used to push human_input_required onto a
// session's progress stream. Wired to Daemon.EmitProgress after construction.
func (h *HITL) SetEmit(fn func(agentID string, msg protocol.Msg)) { h.emit = fn }

// MarkInteractive records agentID as an interactive (HITL-eligible) session.
func (h *HITL) MarkInteractive(agentID string) error {
	return h.store.InteractiveSessionAdd(agentID)
}

// IsInteractive reports whether agentID was started interactively.
func (h *HITL) IsInteractive(agentID string) (bool, error) {
	return h.store.InteractiveSessionExists(agentID)
}

// ExpireStale marks any pending request past its deadline as timed_out. Called
// once at daemon startup.
func (h *HITL) ExpireStale() error {
	return h.store.HumanRequestExpireStale(time.Now())
}

// Ask raises a question and blocks until the human answers, the turn is
// cancelled, or the deadline passes (R-HITL.3). A pending row left over from a
// prior attempt is reused rather than duplicated.
func (h *HITL) Ask(ctx context.Context, agentID, question string, options []string) (string, error) {
	id, q, opts, deadline, err := h.openRequest(agentID, question, options)
	if err != nil {
		return "", err
	}

	ch := make(chan string, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.pending, id)
		h.mu.Unlock()
	}()
	slog.Info("hitl ask waiting", "agent_id", agentID, "request_id", id)

	if h.emit != nil {
		h.emit(agentID, protocol.NewHumanInputRequiredMsg(agentID, id, q, opts, int(h.timeout.Seconds())))
	}

	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()

	select {
	case ans := <-ch:
		slog.Info("hitl ask answered", "agent_id", agentID, "request_id", id)
		if err := h.store.HumanRequestAnswer(id, ans); err != nil {
			return "", err
		}
		return ans, nil
	case <-timer.C:
		slog.Warn("hitl ask timed out", "agent_id", agentID, "request_id", id, "deadline", deadline)
		h.store.HumanRequestMarkTimedOut(id) //nolint:errcheck // best-effort; the row stays pending and is expired on next boot
		return "", errHumanTimeout
	case <-ctx.Done():
		slog.Warn("hitl ask cancelled", "agent_id", agentID, "request_id", id, "err", ctx.Err())
		h.store.HumanRequestMarkTimedOut(id) //nolint:errcheck
		return "", ctx.Err()
	}
}

// openRequest reuses an existing pending row for agentID or creates a new one,
// returning the request ID, question, options, and expiry to wait on.
func (h *HITL) openRequest(agentID, question string, options []string) (id, q string, opts []string, deadline time.Time, err error) {
	if existing, gerr := h.store.HumanRequestGetPending(agentID); gerr == nil && existing != nil {
		return existing.ID, existing.Question, existing.Options, existing.ExpiresAt, nil
	}
	id = newUUID()
	deadline = time.Now().Add(h.timeout)
	if err = h.store.HumanRequestCreate(id, agentID, question, options, deadline); err != nil {
		return "", "", nil, time.Time{}, err
	}
	return id, question, options, deadline, nil
}

// Answer delivers an answer to the ask_human call waiting on requestID and
// reports whether a waiter was found. Called from the human_input_answer
// dispatch.
func (h *HITL) Answer(requestID, answer string) bool {
	h.mu.Lock()
	ch, ok := h.pending[requestID]
	keys := make([]string, 0, len(h.pending))
	for k := range h.pending {
		keys = append(keys, k)
	}
	h.mu.Unlock()
	if !ok {
		slog.Warn("hitl answer: no waiter", "request_id", requestID, "pending", keys)
		return false
	}
	select {
	case ch <- answer:
		return true
	default:
		return false
	}
}
