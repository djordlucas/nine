package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"nine/internal/memory"
	"nine/internal/protocol"
)

// Revision (adr/process-sessions.md §9, phase 6). Every write of a tool Nine
// wrote keeps the version it replaces (memory.GeneratedToolWrite). A rewrite of
// a process's program puts it on probation: the running instance is stopped and
// started on the new version, and if that version fails its first
// rollback_after runs before one succeeds, the previous version is restored —
// journaled, reported to the human feed, and told to the session that wrote it
// when that is a process.

const (
	evProcessRevised    = "process_revised"
	evProcessRolledBack = "process_rolled_back"

	// DefaultRollbackAfter is [processes] rollback_after when unset.
	DefaultRollbackAfter = 3

	// ByRollback writes a version restored by a rollback.
	ByRollback = "rollback"
)

// SetRollbackAfter sets [processes] rollback_after; <= 0 is the default.
func (r *StandingRunner) SetRollbackAfter(n int) {
	if r != nil {
		r.rollbackAfter = n
	}
}

func (r *StandingRunner) rollbackAfterOrDefault() int {
	if r.rollbackAfter > 0 {
		return r.rollbackAfter
	}
	return DefaultRollbackAfter
}

// restartLive stops a running live process's instance so the runner starts it
// again, on the tool's current version. A process not running live is left
// alone: it starts on the current version whenever it starts.
func (r *StandingRunner) restartLive(id string) {
	if r == nil {
		return
	}
	r.liveMu.Lock()
	lp := r.lives[id]
	if lp != nil {
		delete(r.lives, id)
	}
	r.liveMu.Unlock()
	if lp != nil {
		lp.live.Stop()
		r.log.add(id, "restarted", "new version")
	}
}

// proveVersion ends a process's probation once its program has run: a live
// one has handled a trigger and come back for the next, a slice one has
// completed a call.
func (r *StandingRunner) proveVersion(st memory.Process) {
	if !st.Generated {
		return
	}
	if err := r.store.GeneratedToolProve(st.Tool); err != nil {
		slog.Warn("process: end probation", "id", st.ID, "err", err)
	}
}

// rollback restores the previous version of a process on probation whose
// failures reached rollback_after. It reports whether it rolled back; a
// process not on probation, or with no previous version, is left to the
// ordinary failure path.
func (r *StandingRunner) rollback(st memory.Process, failures int, cause error) bool {
	if !st.Generated || failures < r.rollbackAfterOrDefault() || r.restore == nil {
		return false
	}
	t, ok, err := r.store.GeneratedToolGet(st.Tool)
	if err != nil || !ok || !t.Probation {
		return false
	}
	versions, err := r.store.GeneratedToolVersions(st.Tool)
	if err != nil || len(versions) == 0 {
		return false
	}
	prev := versions[0]
	if err := r.restore(context.Background(), st.Tool, prev.Version, ByRollback); err != nil {
		slog.Warn("process: rollback failed", "id", st.ID, "tool", st.Tool, "err", err)
		return false
	}
	reason := clipDetail(cause.Error(), 500)
	slog.Warn("process rolled back", "id", st.ID, "tool", st.Tool, "from", t.Version, "to", prev.Version, "failures", failures)
	journalTransition(r.store, st.ID, evProcessRolledBack, map[string]any{
		"tool": st.Tool, "from": t.Version, "to": prev.Version, "failures": failures,
		"error": reason, "written_by": t.WrittenBy,
	})
	msg := fmt.Sprintf("Process %s: version %d of %s failed its first %d runs and was rolled back to version %d. Last error: %s",
		st.ID, t.Version, st.Tool, failures, prev.Version, reason)
	r.notifyHuman(msg)
	// The session that wrote the failed version learns why, when it is a
	// process that can take a message: a reviser.
	if t.WrittenBy != "" && t.WrittenBy != st.SessionID {
		if _, delivered := r.Deliver(t.WrittenBy, msg, st.ID, false, 0); !delivered {
			slog.Info("process: the rollback was not delivered to its writer", "writer", t.WrittenBy)
		}
	}
	return true
}

// Restore makes version of a tool Nine wrote current again, as a new version,
// with the process definition it ran under. A running process restarts on it.
// by names who restored it: the operator, or a rollback.
func (g *generatedTools) Restore(ctx context.Context, name string, version int, by string) error {
	cur, ok, err := g.store.GeneratedToolGet(name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no tool %q written by Nine: not found", name)
	}
	v, ok, err := g.store.GeneratedToolVersion(name, version)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tool %q has no version %d (it is at version %d; see its versions)", name, version, cur.Version)
	}
	id := standingIDFor(name)
	curProcess := ""
	if p, ok, err := g.store.ProcessGet(id); err == nil && ok {
		b, _ := json.Marshal(p)
		curProcess = string(b)
	}
	if _, err := g.store.GeneratedToolWrite(memory.GeneratedTool{
		Name: name, Description: v.Description, InputSchema: v.InputSchema, Source: v.Source,
		Capabilities: v.Capabilities, Lockfile: v.Lockfile, Resumable: v.Resumable, Live: v.Live,
		WrittenBy: by,
	}, curProcess); err != nil {
		return err
	}
	if v.Process != "" {
		var p memory.Process
		if err := json.Unmarshal([]byte(v.Process), &p); err != nil {
			return fmt.Errorf("version %d's process definition: %w", version, err)
		}
		if err := g.store.ProcessUpsertDefinition(p); err != nil {
			return err
		}
	}
	// The restored version starts clean: its failures belonged to the other one.
	if p, ok, err := g.store.ProcessGet(id); err == nil && ok {
		if err := g.store.ProcessRecovered(id); err != nil {
			return err
		}
		if p.State == memory.ProcessFailing {
			if _, err := g.store.ProcessSetState(id, memory.ProcessRunning); err != nil {
				return err
			}
		}
	}
	g.reload(ctx)
	if g.runner != nil {
		g.runner.restartLive(id)
	}
	slog.Info("generated tool restored", "tool", name, "version", version, "by", by)
	return nil
}

// Versions returns a tool Nine wrote and the versions it keeps.
func (g *generatedTools) Versions(name string) (protocol.ToolVersionsInfo, error) {
	cur, ok, err := g.store.GeneratedToolGet(name)
	if err != nil {
		return protocol.ToolVersionsInfo{}, err
	}
	if !ok {
		return protocol.ToolVersionsInfo{}, fmt.Errorf("no tool %q written by Nine: not found", name)
	}
	vs, err := g.store.GeneratedToolVersions(name)
	if err != nil {
		return protocol.ToolVersionsInfo{}, err
	}
	out := protocol.ToolVersionsInfo{Name: name, Version: cur.Version, Probation: cur.Probation,
		WrittenBy: cur.WrittenBy, UpdatedAt: cur.UpdatedAt, Previous: []protocol.ToolVersionInfo{}}
	for _, v := range vs {
		out.Previous = append(out.Previous, protocol.ToolVersionInfo{Version: v.Version, WrittenBy: v.WrittenBy,
			WrittenAt: v.WrittenAt, Lines: strings.Count(v.Source, "\n") + 1})
	}
	return out, nil
}

// toolReadout is what tool_read returns for one tool Nine wrote.
type toolReadout struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Source       string          `json:"source"`
	Capabilities json.RawMessage `json:"capabilities,omitempty"`
	Version      int             `json:"version"`
	Probation    bool            `json:"probation,omitempty"`
	Process      *processReadout `json:"process,omitempty"`
}

// processReadout is the process a tool runs, as tool_read shows it.
type processReadout struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	StoppedBy string `json:"stopped_by,omitempty"`
	Every     string `json:"every,omitempty"`
	Schedule  string `json:"schedule,omitempty"`
	Role      string `json:"role,omitempty"`
	ReportTo  string `json:"report_to,omitempty"`
	On        string `json:"on,omitempty"`
	Failures  int    `json:"failures,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// toolListing is one line of tool_read's list.
type toolListing struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     int    `json:"version"`
	Probation   bool   `json:"probation,omitempty"`
	Process     string `json:"process,omitempty"`
	State       string `json:"state,omitempty"`
	LastError   string `json:"last_error,omitempty"`
}

// Read is tool_read: one tool Nine wrote with its process, or with an empty
// name the list of them.
func (g *generatedTools) Read(name string) (any, error) {
	if name == "" {
		rows, err := g.store.GeneratedToolList()
		if err != nil {
			return nil, err
		}
		out := []toolListing{}
		for _, t := range rows {
			l := toolListing{Name: t.Name, Description: t.Description, Version: t.Version, Probation: t.Probation}
			if p, ok, err := g.store.ProcessGet(standingIDFor(t.Name)); err == nil && ok {
				l.Process, l.State, l.LastError = p.ID, p.State, clipDetail(p.LastError, 300)
			}
			out = append(out, l)
		}
		return out, nil
	}
	t, ok, err := g.store.GeneratedToolGet(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("no tool %q written by Nine; tool_read with no name lists them", name)
	}
	out := toolReadout{Name: t.Name, Description: t.Description, Source: t.Source,
		Capabilities: t.Capabilities, Version: t.Version, Probation: t.Probation}
	if p, ok, err := g.store.ProcessGet(standingIDFor(name)); err == nil && ok {
		pr := &processReadout{ID: p.ID, State: p.State, StoppedBy: p.StoppedBy, Schedule: p.Schedule,
			Role: p.Role, ReportTo: p.ReportTo, On: p.OnEvents, Failures: p.Failures, LastError: clipDetail(p.LastError, 1000)}
		if p.IntervalSecs > 0 {
			pr.Every = (time.Duration(p.IntervalSecs) * time.Second).String()
		}
		out.Process = pr
	}
	return out, nil
}

// isRevisingRole reports whether a role can rewrite tools: it holds tool_write.
func isRevisingRole(tools []string, all bool) bool {
	return all || containsFold(tools, "tool_write")
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
