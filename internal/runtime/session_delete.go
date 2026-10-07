package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"nine/internal/memory"
)

// DefaultSessionRetentionDays is how long an abandoned session is kept when
// [daemon] session_retention_days is unset. Ten days is long enough that a
// conversation someone means to come back to on Monday survives a week away, and
// short enough that a daemon left running does not accumulate history forever.
const DefaultSessionRetentionDays = 10

// sessionReapInterval is how often the reaper looks. Retention is measured in
// days, so looking more than daily buys nothing.
const sessionReapInterval = 24 * time.Hour

// DeleteSession erases a session: it stops the running worker, then removes the
// conversation and everything keyed to it.
//
// This is deliberately a different verb from `session stop`, which ends a
// session and archives it — the transcript, the journal, and any notifications
// survive a stop. Delete is the one operation in Nine that destroys history, so
// it is separate, it is reported in full, and nothing calls it implicitly.
//
// Ordering matters. The worker is stopped first, so nothing writes a checkpoint
// or a journal row into the rows about to be removed; outstanding plugin jobs
// are cancelled next, because their goroutines live in another process and
// deleting a registry row cannot reach them; the cascade runs last, in one
// transaction.
func (d *Daemon) DeleteSession(ctx context.Context, agentID string) (memory.SessionDeleteCounts, error) {
	var counts memory.SessionDeleteCounts
	if d.store == nil {
		return counts, fmt.Errorf("session deletion requires a configured store")
	}
	id := d.resolveID(agentID)

	sum, found, err := d.store.SessionGet(id)
	if err != nil {
		return counts, err
	}
	if !found {
		return counts, fmt.Errorf("session %s not found", agentID)
	}

	// Stop the worker and drop it from the live maps before touching the rows.
	d.mu.Lock()
	w, running := d.sessions[id]
	delete(d.sessions, id)
	delete(d.names, id)
	d.mu.Unlock()
	if running {
		w.stop()
	}

	d.cancelPluginJobsFor(ctx, id)

	counts, err = d.store.SessionDelete(id)
	if err != nil {
		return counts, err
	}

	// Logged field by field rather than as a total: this is the audit record of
	// an irreversible act, and "deleted session X" does not say what was in it.
	slog.Info("session deleted",
		"id", id, "name", sum.Name, "age_seconds", sum.AgeSeconds,
		"events", counts.Events, "notifications", counts.Notifications,
		"user_notifications", counts.UserNotifications,
		"related", counts.Related, "human_requests", counts.HumanRequests,
		"tool_state", counts.ToolState, "jobs", counts.Jobs,
		"rows_total", counts.Total())
	return counts, nil
}

// cancelPluginJobsFor asks the plugins to stop any job this session owns.
//
// Best-effort by nature: a plugin job is a goroutine in another process, so the
// most the daemon can do is ask. A *tool* job needs nothing here — deleting its
// row is the cancellation, since the sweeper finds nothing to call.
func (d *Daemon) cancelPluginJobsFor(ctx context.Context, id string) {
	if d.mgr == nil {
		return
	}
	jobs, err := d.store.JobsRunning()
	if err != nil {
		slog.Warn("could not list jobs before deleting a session", "id", id, "err", err)
		return
	}
	for _, j := range jobs {
		if j.OwnerID != id || j.Backend != memory.JobBackendPlugin {
			continue
		}
		if p, ok := d.mgr.PluginByName(j.Plugin); ok {
			if err := d.mgr.JobCancel(ctx, p, j.BackendRef); err != nil {
				slog.Warn("could not cancel a plugin job for a deleted session",
					"id", id, "handle", j.Handle, "err", err)
			}
		}
	}
}

// RunSessionReaper deletes abandoned sessions once at startup and daily
// thereafter, until ctx is cancelled. retentionDays <= 0 disables it entirely.
//
// What it will not take is the point (memory.SessionsReapable): a session whose
// id matches an active goal, or one carrying an active session plan. Both are
// idle *by design* — a standing agent that wakes weekly looks abandoned after
// ten days precisely because it is working correctly.
func RunSessionReaper(ctx context.Context, d *Daemon, retentionDays int) {
	if d == nil || d.store == nil || retentionDays <= 0 {
		return
	}
	retention := time.Duration(retentionDays) * 24 * time.Hour

	d.reapSessions(ctx, retention)
	t := time.NewTicker(sessionReapInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.reapSessions(ctx, retention)
		}
	}
}

func (d *Daemon) reapSessions(ctx context.Context, retention time.Duration) {
	stale, err := d.store.SessionsReapable(retention)
	if err != nil {
		slog.Warn("session reap: list", "err", err)
		return
	}
	for _, s := range stale {
		// A session that came alive between the query and now is no longer
		// abandoned, and taking it would delete something a human is looking at.
		d.mu.RLock()
		_, running := d.sessions[s.ID]
		d.mu.RUnlock()
		if running {
			continue
		}
		if _, err := d.DeleteSession(ctx, s.ID); err != nil {
			slog.Warn("session reap: delete", "id", s.ID, "err", err)
		}
	}
}
