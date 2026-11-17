package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"nine/internal/backup"
	"nine/internal/memory"
	"nine/internal/workflow"
)

// agentStoreRouter routes the daemon's store surface over the per-agent layout
// (adr/agents-concept.md A4, Q31): cross-agent state (processes, board, agent
// definitions, skills catalog, session index) lives in the registry db; each
// agent's own state lives in its own db, opened lazily by the manager
// (LRU-capped, Q35). The router implements queryBackend (plus the kv and
// conversation interfaces the handlers type-assert) by delegating to the right
// store per call: session-scoped calls route through the session index,
// daemon-wide reads fan out across every agent db and merge.

type agentStoreRouter struct {
	mgr *memory.AgentStores
}

func newAgentStoreRouter(mgr *memory.AgentStores) *agentStoreRouter {
	return &agentStoreRouter{mgr: mgr}
}

// RegistryStore exposes the registry store (daemon sets it for journaling).
func (r *agentStoreRouter) RegistryStore() *memory.Store { return r.mgr.Registry() }

// AgentStores exposes the manager (agent export reads the agent's db file).
func (r *agentStoreRouter) AgentStores() *memory.AgentStores { return r.mgr }

// storeFor routes one session id to its agent's store.
func (r *agentStoreRouter) storeFor(sessionID string) (*memory.Store, error) {
	return r.mgr.StoreForSession(sessionID)
}

// eachAgentStore opens every agent db on disk and calls fn on each (fan-out
// reads; the LRU cap means stores reopen on demand afterwards).
func (r *agentStoreRouter) eachAgentStore(fn func(*memory.Store) error) error {
	ids, err := r.mgr.AgentIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		st, err := r.mgr.AgentStore(id)
		if err != nil {
			return err
		}
		if err := fn(st); err != nil {
			return err
		}
	}
	return nil
}

// routeFind runs fn on every store (agent dbs plus the registry) until one
// reports found.
func (r *agentStoreRouter) routeFind(fn func(*memory.Store) (bool, error)) (bool, error) {
	found, err := fn(r.mgr.Registry())
	if err != nil || found {
		return found, err
	}
	var anyFound bool
	err = r.eachAgentStore(func(st *memory.Store) error {
		if anyFound {
			return nil
		}
		f, err := fn(st)
		if err != nil {
			return err
		}
		anyFound = f
		return nil
	})
	return anyFound, err
}

// --- registry-scoped: delegate ---

func (r *agentStoreRouter) SkillList() ([]memory.Skill, error)          { return r.mgr.Registry().SkillList() }
func (r *agentStoreRouter) SkillGet(n string) (memory.Skill, bool, error) {
	return r.mgr.Registry().SkillGet(n)
}
func (r *agentStoreRouter) SkillUpsert(sk memory.Skill) error { return r.mgr.Registry().SkillUpsert(sk) }
func (r *agentStoreRouter) SkillDelete(n string) error        { return r.mgr.Registry().SkillDelete(n) }

// kvWriter is the store's key-value write; operator memory_set/memory_delete
// land in the registry db, which daemon-wide reads consult first.
func (r *agentStoreRouter) Set(key, value string) error { return r.mgr.Registry().Set(key, value) }
func (r *agentStoreRouter) Delete(key string) error      { return r.mgr.Registry().Delete(key) }
func (r *agentStoreRouter) BoardTaskCreate(t, s, p, src, role string, pri, d int) (memory.BoardTask, error) {
	return r.mgr.Registry().BoardTaskCreate(t, s, p, src, role, pri, d)
}
func (r *agentStoreRouter) BoardTaskList(s string, l, o int) ([]memory.BoardTask, error) {
	return r.mgr.Registry().BoardTaskList(s, l, o)
}
func (r *agentStoreRouter) BoardTaskGet(id string) (memory.BoardTask, bool, error) {
	return r.mgr.Registry().BoardTaskGet(id)
}
func (r *agentStoreRouter) BoardTaskUpdate(id string, p memory.BoardTaskPatch) (memory.BoardTask, bool, error) {
	return r.mgr.Registry().BoardTaskUpdate(id, p)
}
func (r *agentStoreRouter) BoardTaskDelete(id string) (bool, error) {
	return r.mgr.Registry().BoardTaskDelete(id)
}
func (r *agentStoreRouter) AgentList() ([]memory.Agent, error) { return r.mgr.Registry().AgentList() }
func (r *agentStoreRouter) AgentGet(id string) (memory.Agent, bool, error) {
	return r.mgr.Registry().AgentGet(id)
}
func (r *agentStoreRouter) AgentCreate(n, ro, sk, sm, re string) (memory.Agent, error) {
	return r.mgr.Registry().AgentCreate(n, ro, sk, sm, re)
}
func (r *agentStoreRouter) AgentCreateWithStatus(n, ro, sk, sm, re, st string) (memory.Agent, error) {
	return r.mgr.Registry().AgentCreateWithStatus(n, ro, sk, sm, re, st)
}
func (r *agentStoreRouter) AgentUpdate(id string, p memory.AgentPatch) (memory.Agent, bool, error) {
	return r.mgr.Registry().AgentUpdate(id, p)
}
func (r *agentStoreRouter) AgentDelete(id string) (bool, error) { return r.mgr.Registry().AgentDelete(id) }
func (r *agentStoreRouter) AgentVersions(id string) ([]memory.AgentVersion, error) {
	return r.mgr.Registry().AgentVersions(id)
}
func (r *agentStoreRouter) AgentApprove(id string) (memory.Agent, bool, error) {
	return r.mgr.Registry().AgentApprove(id)
}
func (r *agentStoreRouter) EventCursorGet(sub string) (int64, error) {
	return r.mgr.Registry().EventCursorGet(sub)
}
func (r *agentStoreRouter) EventCursorSet(sub string, seq int64) error {
	return r.mgr.Registry().EventCursorSet(sub, seq)
}

// --- session-scoped: route through the session index ---

// Workflows are registry-scoped: their writes journal state events, and the
// journal is registry-global.
func (r *agentStoreRouter) WorkflowList(agentID string) ([]workflow.Workflow, error) {
	return r.mgr.Registry().WorkflowList(agentID)
}
func (r *agentStoreRouter) WorkflowCancel(id string) (int, error) {
	return r.mgr.Registry().WorkflowCancel(id)
}
func (r *agentStoreRouter) WorkflowFail(id string, all bool) (int, error) {
	return r.mgr.Registry().WorkflowFail(id, all)
}

// SessionEventsByAgent reads the registry journal: session_events stay
// daemon-wide there (single seq cursor, R-PROTO.15), not in agent dbs.
func (r *agentStoreRouter) SessionEventsByAgent(agentID string) ([]memory.SessionEvent, error) {
	return r.mgr.Registry().SessionEventsByAgent(agentID)
}
func (r *agentStoreRouter) SessionGet(id string) (memory.SessionSummary, bool, error) {
	st, err := r.storeFor(id)
	if err != nil {
		return memory.SessionSummary{}, false, err
	}
	return st.SessionGet(id)
}
func (r *agentStoreRouter) SessionDelete(id string) (memory.SessionDeleteCounts, error) {
	st, err := r.storeFor(id)
	if err != nil {
		return memory.SessionDeleteCounts{}, err
	}
	counts, err := st.SessionDelete(id)
	if err != nil {
		return counts, err
	}
	// The registry holds the journal and the coordination tables: clear the
	// session's rows there too, and merge the counts.
	regCounts, regErr := r.mgr.Registry().SessionDelete(id)
	if regErr == nil {
		counts.Conversations += regCounts.Conversations
		counts.Events += regCounts.Events
		counts.Notifications += regCounts.Notifications
		counts.UserNotifications += regCounts.UserNotifications
		counts.Related += regCounts.Related
		counts.HumanRequests += regCounts.HumanRequests
		counts.InteractiveMarkers += regCounts.InteractiveMarkers
		counts.ToolState += regCounts.ToolState
		counts.Jobs += regCounts.Jobs
	}
	_ = r.mgr.Registry().SessionIndexDelete(id)
	return counts, err
}
func (r *agentStoreRouter) QueueMessage(agentID, message string) error {
	st, err := r.storeFor(agentID)
	if err != nil {
		return err
	}
	return st.QueueMessage(agentID, message)
}
func (r *agentStoreRouter) UnconsumedMessagesCount(agentID string) (int, error) {
	st, err := r.storeFor(agentID)
	if err != nil {
		return 0, err
	}
	return st.UnconsumedMessagesCount(agentID)
}
func (r *agentStoreRouter) DrainQueuedMessage(agentID string) (string, error) {
	st, err := r.storeFor(agentID)
	if err != nil {
		return "", err
	}
	return st.DrainQueuedMessage(agentID)
}
// Goals are daemon-wide (registry), like processes and board_tasks: their
// writes journal state events (goal_created/…), and the wire journal follow
// carries a single seq cursor that only one db can honour.
func (r *agentStoreRouter) GoalGet(id string) (*memory.Goal, error) {
	return r.mgr.Registry().GoalGet(id)
}
func (r *agentStoreRouter) GoalCreate(id, desc, parentID, parentType string) error {
	return r.mgr.Registry().GoalCreate(id, desc, parentID, parentType)
}
func (r *agentStoreRouter) GoalDelete(id string) ([]string, error) {
	return r.mgr.Registry().GoalDelete(id)
}
func (r *agentStoreRouter) GoalUpdateStatus(id, status string) error {
	return r.mgr.Registry().GoalUpdateStatus(id, status)
}

func (r *agentStoreRouter) UserNotificationMarkSeen(id string) error {
	return r.mgr.Registry().UserNotificationMarkSeen(id)
}

// --- daemon-wide reads: fan out and merge ---

func (r *agentStoreRouter) GoalList() ([]memory.Goal, error) {
	return r.mgr.Registry().GoalList()
}

func (r *agentStoreRouter) SessionList() ([]memory.SessionSummary, error) {
	index, err := r.mgr.Registry().SessionIndexList()
	if err != nil {
		return nil, err
	}
	out := make([]memory.SessionSummary, 0, len(index))
	for _, e := range index {
		st, err := r.mgr.StoreForSession(e.ID)
		if err != nil {
			return nil, err
		}
		sum, ok, err := st.SessionGet(e.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			// Index row without a conversation row (already deleted): keep the
			// index entry's own fields so the roster still lists it.
			sum = memory.SessionSummary{ID: e.ID, Status: e.Status, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
		}
		// The journal and the goals/processes that guard a session live in the
		// registry, so the event count and the protected flag come from there.
		reg := r.mgr.Registry()
		if evs, err := reg.SessionEventsByAgent(e.ID); err == nil {
			sum.Events = len(evs)
		}
		protected := false
		if g, err := reg.GoalGet(e.ID); err == nil && g != nil && g.Status == "active" {
			protected = true
		}
		if procs, err := reg.ProcessesOfSession(e.ID); err == nil && len(procs) > 0 {
			protected = true
		}
		sum.Protected = protected
		out = append(out, sum)
	}
	return out, nil
}

func (r *agentStoreRouter) SessionsReapable(maxAge time.Duration) ([]memory.SessionSummary, error) {
	var out []memory.SessionSummary
	if err := r.eachAgentStore(func(st *memory.Store) error {
		s, err := st.SessionsReapable(maxAge)
		if err != nil {
			return err
		}
		out = append(out, s...)
		return nil
	}); err != nil {
		return nil, err
	}
	s, err := r.mgr.Registry().SessionsReapable(maxAge)
	if err != nil {
		return nil, err
	}
	return append(out, s...), nil
}

// SessionEventsAfter reads the registry journal only: the follow cursor is one
// daemon-wide seq, which per-db sequences cannot honour.
func (r *agentStoreRouter) SessionEventsAfter(afterSeq int64, limit int) ([]memory.SessionEvent, error) {
	return r.mgr.Registry().SessionEventsAfter(afterSeq, limit)
}

func (r *agentStoreRouter) SessionEventsBounds() (int64, int64, error) {
	return r.mgr.Registry().SessionEventsBounds()
}

// UserNotificationList reads the registry: notification writes journal state
// events, and the journal is registry-global.
func (r *agentStoreRouter) UserNotificationList(unseenOnly bool) ([]memory.UserNotification, error) {
	return r.mgr.Registry().UserNotificationList(unseenOnly)
}

// HumanRequestListPending reads the registry (HITL writes journal state events).
func (r *agentStoreRouter) HumanRequestListPending() ([]memory.HumanRequest, error) {
	return r.mgr.Registry().HumanRequestListPending()
}

func (r *agentStoreRouter) JobsRunning() ([]memory.Job, error) {
	var out []memory.Job
	if err := r.eachAgentStore(func(st *memory.Store) error {
		j, err := st.JobsRunning()
		if err != nil {
			return err
		}
		out = append(out, j...)
		return nil
	}); err != nil {
		return nil, err
	}
	j, err := r.mgr.Registry().JobsRunning()
	if err != nil {
		return nil, err
	}
	return append(out, j...), nil
}

// --- kv (per-agent now; daemon-wide reads merge) ---

func (r *agentStoreRouter) KVGetString(key string) string {
	v, ok, err := r.Get(key)
	if err != nil || !ok {
		return ""
	}
	return v
}

func (r *agentStoreRouter) KVEntries() ([]memory.KVEntry, error) {
	var out []memory.KVEntry
	if err := r.eachAgentStore(func(st *memory.Store) error {
		e, err := st.KVEntries()
		if err != nil {
			return err
		}
		out = append(out, e...)
		return nil
	}); err != nil {
		return nil, err
	}
	e, err := r.mgr.Registry().KVEntries()
	if err != nil {
		return nil, err
	}
	return append(out, e...), nil
}

func (r *agentStoreRouter) Get(key string) (string, bool, error) {
	if v, ok, err := r.mgr.Registry().Get(key); err == nil && ok {
		return v, true, nil
	}
	found := false
	var val string
	err := r.eachAgentStore(func(st *memory.Store) error {
		if found {
			return nil
		}
		v, ok, err := st.Get(key)
		if err != nil {
			return err
		}
		if ok {
			val, found = v, true
		}
		return nil
	})
	return val, found, err
}

// --- conversation creation (session index first, then the row in that store) ---

func (r *agentStoreRouter) ConversationCreate(id string) error {
	st, err := r.storeFor(id)
	if err != nil {
		return err
	}
	return st.ConversationCreate(id)
}

// ConversationSetAgent points a session at a stored agent: the session index
// moves, and the conversation row's agent_id is updated in the session's store.
func (r *agentStoreRouter) ConversationSetAgent(id, agentID string) error {
	if err := r.mgr.Registry().SessionIndexSet(id, agentID, "active"); err != nil {
		return err
	}
	st, err := r.mgr.StoreForSession(id)
	if err != nil {
		return err
	}
	return st.ConversationSetAgent(id, agentID)
}

// SessionIndexSet records a session's agent in the registry index (called when a
// conversation is created).
func (r *agentStoreRouter) SessionIndexSet(id, agentID string) error {
	return r.mgr.Registry().SessionIndexSet(id, agentID, "active")
}

// --- backup: every db, each VACUUM INTO its own file (Q33) ---

// BackupDBs snapshots the registry db and every agent db into dir, returning
// the archive entries (name → path). Each file comes from VACUUM INTO, so each
// is consistent by construction; the set is not cross-db transactional.
func (r *agentStoreRouter) BackupDBs(dir string) ([]backup.DBFile, error) {
	var out []backup.DBFile
	regPath := filepath.Join(dir, backup.DBEntry)
	if _, err := r.mgr.Registry().BackupTo(regPath); err != nil {
		return nil, err
	}
	out = append(out, backup.DBFile{Name: backup.DBEntry, Path: regPath})
	ids, err := r.mgr.AgentIDs()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		st, err := r.mgr.AgentStore(id)
		if err != nil {
			return nil, err
		}
		name := "agents/" + id + ".db"
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, err
		}
		if _, err := st.BackupTo(p); err != nil {
			return nil, fmt.Errorf("snapshot agent %s: %w", id, err)
		}
		out = append(out, backup.DBFile{Name: name, Path: p})
	}
	return out, nil
}

// BackupTo keeps the queryBackend signature: a single-file snapshot of the
// registry db (the full multi-db snapshot is BackupDBs, used by writeBackup).
func (r *agentStoreRouter) BackupTo(path string) (string, error) {
	return r.mgr.Registry().BackupTo(path)
}
