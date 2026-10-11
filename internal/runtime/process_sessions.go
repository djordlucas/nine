package runtime

import (
	"context"
	"log/slog"
)

// ProcessTurn runs one turn in process session id and returns the reply,
// creating the session with p — or resuming it from its checkpoint — when it is
// not running. trigger labels the turn in the journal ("idle" for a clock,
// "condition" for a pipe's report), so a process's turns read as the routine
// turns they replace (adr/process-sessions.md §14, phase 1).
//
// opt sets the trigger label, a tool restriction, and the turn's depth.
func (d *Daemon) ProcessTurn(ctx context.Context, id string, p RoleParams, text string, opt TurnOptions) (string, int, error) {
	res := d.processWorker(id, p).turnCounted(ctx, text, opt)
	return res.text, res.tokens, res.err
}

// processWorker returns the running worker for id, starting one with p if
// there is none. The worker is built outside the lock, so a second caller
// racing the first keeps the worker that won and stops its own.
func (d *Daemon) processWorker(id string, p RoleParams) *AgentWorker {
	d.mu.RLock()
	w, ok := d.sessions[id]
	d.mu.RUnlock()
	if ok {
		return w
	}

	var data []byte
	if d.ckpt != nil {
		data, _, _ = d.ckpt.Load(id) //nolint:errcheck // a missing checkpoint starts the session fresh
	}
	w = d.buildWorker(id, data, p)

	d.mu.Lock()
	if existing, ok := d.sessions[id]; ok {
		d.mu.Unlock()
		w.stop()
		return existing
	}
	d.sessions[id] = w
	d.mu.Unlock()
	slog.Info("process session started", "id", id, "role", p.Role)
	return w
}
