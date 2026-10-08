// Package runtime implements the Nine daemon: a Unix-socket server that
// routes user turns to per-conversation agent loops and persists state via
// checkpoints. See nine/internal/protocol for the wire protocol and the
// client used by the CLI/TUI to connect to it.
package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/protocol"
	"nine/internal/subscribe"
	"nine/internal/toolvm"
	"nine/internal/workflow"
)

// LoopFactory creates a fresh agent loop for the given conversation ID from
// the resolved RoleParams. The daemon derives the params from the session's
// plan (roleNameForPlan / planOwnsGoal / planDelegates); unknown role names
// degrade to the default leaf role. The factory is called once per
// new_conversation and once per attach.
type LoopFactory func(agentID string, params RoleParams) *agent.Loop

// RoleParams carries the per-session role-resolution inputs the daemon derives
// from a session plan and passes to the loop factory.
type RoleParams struct {
	// Role is the role name to resolve (adr/roles-design.md §6).
	Role string
	// Interactive enables human-in-the-loop tools; effective only for
	// HITL-eligible roles (R-HITL.1). AND-ed with the role's Interactive flag.
	Interactive bool
	// OwnsGoal marks a pursue-shell session that steers its own goal, granting
	// the goal self-management tools regardless of the role's allowlist
	// (adr/predefined-agents-design.md §3.1).
	OwnsGoal bool
	// Delegates opts a standing agent into sub-agent fan-out; OR-ed with the
	// resolved role's own Delegates flag (adr/predefined-agents-design.md §3.1).
	Delegates bool
}

// CheckpointStore persists and retrieves serialised loop state keyed by agent ID.
type CheckpointStore interface {
	Save(agentID string, data []byte) error
	Load(agentID string) (data []byte, found bool, err error)
	// Delete removes the persisted state for agentID. Deleting a missing entry
	// is not an error, so terminating an already-gone session is idempotent.
	Delete(agentID string) error
}

// NotifStore stores and retrieves (and marks as delivered) pending notifications for an agent.
type NotifStore interface {
	Add(agentID, text string)
	Fetch(agentID string) ([]string, error)
}

// queryBackend is the slice of the memory store that message handlers query
// on the client's behalf (list_goals, list_workflows, workflow_stop/fail,
// etc.). The daemon doesn't own or reason about this data — it's a thin proxy
// between the Unix-socket clients and the shared backend.
type queryBackend interface {
	GoalList() ([]memory.Goal, error)
	// Goal create and delete back the operator's goal verbs (goal_create,
	// goal_delete) — the same rows the agent's goal tools write.
	GoalGet(id string) (*memory.Goal, error)
	GoalCreate(id, description, parentID, parentType string) error
	GoalDelete(id string) ([]string, error)
	SkillList() ([]memory.Skill, error)
	WorkflowList(agentID string) ([]workflow.Workflow, error)
	WorkflowCancel(id string) (int, error)
	WorkflowFail(id string, all bool) (int, error)
	UserNotificationList(unseenOnly bool) ([]memory.UserNotification, error)
	UserNotificationMarkSeen(id string) error
	// GeneratedToolList backs the lockfile column of the `nine tools` roster: the
	// host holds a generated tool's code, but its dependency lockfile lives in the
	// store (spec/contracts/toolvm.md R-TVM.14).
	GeneratedToolList() ([]memory.GeneratedTool, error)
	SessionEventsByAgent(agentID string) ([]memory.SessionEvent, error)
	// Session listing and deletion. Deletion is the one operation here that
	// destroys history rather than bounding it, which is why it reports what it
	// removed rather than a bare error (spec/contracts/memory-store.md R-MEM.11).
	SessionList() ([]memory.SessionSummary, error)
	SessionGet(id string) (memory.SessionSummary, bool, error)
	SessionsReapable(maxAge time.Duration) ([]memory.SessionSummary, error)
	SessionDelete(id string) (memory.SessionDeleteCounts, error)
	// JobsRunning backs the best-effort plugin-job cancel a session delete does
	// before removing its rows.
	JobsRunning() ([]memory.Job, error)
	// Journal subscription read surface (satisfies subscribe.Store) so the
	// daemon can host cursor-backed subscribers (adr/reactive-events.md).
	SessionEventsAfter(afterSeq int64, limit int) ([]memory.SessionEvent, error)
	EventCursorGet(subscriberID string) (int64, error)
	EventCursorSet(subscriberID string, seq int64) error
	// Queued message support: the daemon queues a user message when the
	// session is mid-turn rather than blocking until the turn completes.
	QueueMessage(agentID, message string) error
	UnconsumedMessagesCount(agentID string) (int, error)
	DrainQueuedMessage(agentID string) (string, error)
}

// pluginRegistry provides the plugin access needed by message handlers.
type pluginRegistry interface {
	ListRunning() []string
	Running() []*plugin.Plugin
	Call(ctx context.Context, p *plugin.Plugin, toolName string, args json.RawMessage) (plugin.CallResult, error)
	ReloadUserPlugins()
	UserStatus() []plugin.UserPluginStatus
	// DisabledSkipped names default plugins that config switched off. They are
	// in neither Running() nor UserStatus(), so the roster needs them from here
	// or they read as simply absent (R-PLUG.14).
	DisabledSkipped() []string
	// PluginByName and JobCancel let a session delete stop the plugin jobs it
	// owned. Best-effort by nature: a plugin job is a goroutine in another
	// process, so asking is the most the daemon can do.
	PluginByName(name string) (*plugin.Plugin, bool)
	JobCancel(ctx context.Context, p *plugin.Plugin, jobID string) error
}

// Daemon accepts connections on a Unix socket and routes messages to
// per-conversation session workers.
type Daemon struct {
	socketPath string
	agent      Agent
	ckpt       CheckpointStore
	startedAt  time.Time
	names      map[string]string // agentID → display name (set on first turn)
	instName   string            // instance display name shown in the TUI top bar (guarded by mu)

	mgr   pluginRegistry
	tools *toolvm.Host // sandboxed-tool host (see ConfigureSandboxedTools); nil = disabled
	// generated is the store of tools Nine wrote, for the operator's delete (see
	// ConfigureGeneratedTools); nil when tool writing is off.
	generated agent.GeneratedToolStore
	// standing drives standing tools (see ConfigureStandingTools); nil = none.
	standing *StandingRunner

	// grants answers and settles capability requests (see ConfigureCapabilities);
	// nil on a daemon with no store, which has no ceiling to report or change.
	grants *CapabilityService
	core   *agent.Dispatcher // core-intercepted tools, for plugin_call (see ConfigureCoreTools)
	store  queryBackend
	hitl   *HITL
	sink   EventSink // durable session-event journal for new workers (nil = disabled)

	maxRunning int // see SetMaxRunning

	// procs is the process store (see ConfigureProcesses); procWake asks the
	// process runner for a pass now. Both nil on a daemon with no processes.
	procs    ProcessBackend
	procWake func()

	listSubAgents func() []protocol.SubAgentInfo
	queueDepth    func() (pending, inflight, maxConcurrent int)

	subs []*subscribe.Subscription // optional journal subscribers (adr/reactive-events.md)

	mu       sync.RWMutex
	sessions map[string]*AgentWorker
	listener net.Listener
}

// New creates a Daemon whose sessions a builds. ckpt may be nil, which
// disables persistence.
func New(socketPath string, a Agent, ckpt CheckpointStore) *Daemon {
	return &Daemon{
		socketPath: socketPath,
		agent:      a,
		ckpt:       ckpt,
		sessions:   make(map[string]*AgentWorker),
		names:      make(map[string]string),
		startedAt:  time.Now(),
	}
}

// SetEventSink registers the durable session-event journal handed to every new
// AgentWorker (adr/event-log.md). Must be called before the first conversation
// is created. Passing nil disables journaling.
func (d *Daemon) SetEventSink(sink EventSink) { d.sink = sink }

// SetSubAgentLister registers a function that returns the currently-running
// sub-agents. Called when handling a "status" request.
func (d *Daemon) SetSubAgentLister(fn func() []protocol.SubAgentInfo) { d.listSubAgents = fn }

// SetQueueStatFn registers a function that reports the shared LLM queue's load
// (pending, inflight, maxConcurrent). Called when handling a "status" request.
func (d *Daemon) SetQueueStatFn(fn func() (pending, inflight, maxConcurrent int)) { d.queueDepth = fn }

// journalReplay reconstructs a reattach snapshot from the durable journal: the
// most recent turn's tool and sub-agent events (as protocol.Msgs, matching what
// the in-memory ring holds) plus that turn's final response. Used when the live
// ring is empty — e.g. a session revived after a daemon restart (adr/event-log.md
// §7.6). Best-effort: any error yields an empty snapshot.
func (d *Daemon) journalReplay(agentID string) ([]protocol.Msg, string) {
	events, err := d.store.SessionEventsByAgent(agentID)
	if err != nil || len(events) == 0 {
		return nil, ""
	}
	// Scope to the last turn recorded.
	lastTurn := events[len(events)-1].Turn
	var (
		msgs     []protocol.Msg
		response string
	)
	for _, e := range events {
		if e.Turn != lastTurn {
			continue
		}
		switch e.Type {
		case "tool_start":
			var p toolStartPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				msgs = append(msgs, protocol.NewToolStartMsg(agentID, p.Name, "", "", p.Input))
			}
		case "tool_end":
			var p toolEndPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				msgs = append(msgs, protocol.NewToolEndMsg(agentID, p.Name, "", "", p.Input, p.Output))
			}
		case "sub_agent_start":
			var p subAgentPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				msgs = append(msgs, protocol.NewSubAgentStartMsg(agentID, p.SubID, p.Task, p.Role))
			}
		case "sub_agent_end":
			var p subAgentPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				msgs = append(msgs, protocol.NewSubAgentEndMsg(agentID, p.SubID, p.Task, p.Status, p.Role))
			}
		case "turn_end":
			var p turnEndPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				response = p.Result
			}
		}
	}
	// Match the live ring's reattach cap so a long turn doesn't flood the client.
	if len(msgs) > replayMaxSend {
		msgs = msgs[len(msgs)-replayMaxSend:]
	}
	return msgs, response
}

// historyMaxTurns bounds how many recent turns are reconstructed for a
// reattaching client, so a very long session doesn't flood the transcript.
const historyMaxTurns = 100

// journalHistory is the reattach transcript: journalHistoryTurns capped at
// historyMaxTurns.
func (d *Daemon) journalHistory(agentID string) []protocol.Msg {
	return d.journalHistoryTurns(agentID, historyMaxTurns)
}

// journalHistoryTurns reconstructs the full multi-turn transcript for agentID from
// the durable journal — user prompts, tool/sub-agent activity, and assistant
// responses, in chronological order — as the ordered protocol messages a
// reattaching client renders. This makes the reconnected conversation look as
// it did before the client detached, rather than blank. Only the most recent
// maxTurns turns are included; maxTurns <= 0 includes every turn.
func (d *Daemon) journalHistoryTurns(agentID string, maxTurns int) []protocol.Msg {
	events, err := d.store.SessionEventsByAgent(agentID)
	if err != nil || len(events) == 0 {
		return nil
	}
	minTurn := 0
	if maxTurns > 0 {
		minTurn = events[len(events)-1].Turn - maxTurns + 1
	}
	var out []protocol.Msg
	for _, e := range events {
		if e.Turn < minTurn {
			continue
		}
		ts := e.TS.UnixMilli()
		var m protocol.Msg
		switch e.Type {
		case "turn_start":
			var p turnStartPayload
			// Only user-initiated turns get a prompt bubble; idle/background turns
			// have no human input, but their activity and response still show.
			if json.Unmarshal(e.Payload, &p) != nil || p.Trigger != "user" || p.Input == "" {
				continue
			}
			m = protocol.NewHistoryUserMsg(agentID, p.Input)
		case "tool_start":
			var p toolStartPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			m = protocol.NewToolStartMsg(agentID, p.Name, "", "", p.Input)
		case "tool_end":
			var p toolEndPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			m = protocol.NewToolEndMsg(agentID, p.Name, "", "", p.Input, p.Output)
		case "sub_agent_start":
			var p subAgentPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			m = protocol.NewSubAgentStartMsg(agentID, p.SubID, p.Task, p.Role)
		case "sub_agent_end":
			var p subAgentPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			m = protocol.NewSubAgentEndMsg(agentID, p.SubID, p.Task, p.Status, p.Role)
		case "turn_end":
			var p turnEndPayload
			if json.Unmarshal(e.Payload, &p) != nil || p.Result == "" {
				continue
			}
			m = protocol.NewResponseMsg(agentID, p.Result)
		default:
			continue
		}
		m.Timestamp = ts
		m.Turn = e.Turn
		out = append(out, m)
	}
	return out
}

// EmitProgress forwards msg to agentID's progress stream, if that
// conversation has an active session and a turn in progress listening for
// progress events. Safe to call from any goroutine; no-op if agentID has no
// active session.
func (d *Daemon) EmitProgress(agentID string, msg protocol.Msg) {
	d.mu.RLock()
	r, ok := d.sessions[agentID]
	d.mu.RUnlock()
	if ok {
		r.emitEvent(msg)
	}
}

// InstanceName returns the daemon's current display name (the instance name
// shown in the TUI top bar). Empty until ResolveInstanceName has run.
func (d *Daemon) InstanceName() string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.instName
}

// SetInstanceName sets the daemon's display name and, when it changes,
// broadcasts a set_instance_name event to every active session so a connected
// client's header updates live (e.g. after the async naming call resolves).
// A blank name is ignored. Safe from any goroutine.
func (d *Daemon) SetInstanceName(name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	d.mu.Lock()
	if d.instName == name {
		d.mu.Unlock()
		return
	}
	d.instName = name
	workers := make([]*AgentWorker, 0, len(d.sessions))
	for _, w := range d.sessions {
		workers = append(workers, w)
	}
	d.mu.Unlock()

	msg := protocol.NewSetInstanceNameMsg(name)
	for _, w := range workers {
		w.emitEvent(msg)
	}
}

// Start removes any stale socket file, opens the Unix socket, and begins
// accepting connections. It blocks until ctx is cancelled or a fatal error
// occurs.
func (d *Daemon) Start(ctx context.Context) error {
	// Refuse to start if another daemon is already serving this socket. Removing
	// and rebinding a live socket would hijack it from the running daemon and
	// split state across two processes — e.g. a turn's HITL pending request would
	// live in one daemon while the answer routes to the other ("no pending
	// question for that request"). Only a stale socket (no listener) is cleared.
	if protocol.CanConnect(d.socketPath) {
		return fmt.Errorf("daemon already listening on %s", d.socketPath)
	}
	os.Remove(d.socketPath) //nolint:errcheck // best-effort stale socket removal
	ln, err := net.Listen("unix", d.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", d.socketPath, err)
	}
	d.mu.Lock()
	d.listener = ln
	d.mu.Unlock()

	// Start any registered journal subscribers (off the turn path).
	d.startSubscribers(ctx)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("accept: %w", err)
			}
		}
		go d.handleConn(ctx, conn)
	}
}

// CheckpointAll triggers a checkpoint for all active sessions, forcing
// their current state to be persisted. This is called during graceful shutdown
// to prevent message loss. It does not wait for workers to finish processing.
func (d *Daemon) CheckpointAll() {
	d.mu.RLock()
	for _, w := range d.sessions {
		w.checkpoint()
	}
	d.mu.RUnlock()
}

// Stop closes the listener and terminates all AgentWorkers gracefully.
func (d *Daemon) Stop() {
	d.mu.Lock()
	if d.listener != nil {
		d.listener.Close()
	}
	workers := make([]*AgentWorker, 0, len(d.sessions))
	for _, w := range d.sessions {
		workers = append(workers, w)
	}
	d.mu.Unlock()

	for _, r := range workers {
		r.stop()
	}
	os.Remove(d.socketPath) //nolint:errcheck // best-effort cleanup on shutdown
}

// handleConn processes all messages from one client connection.
func (d *Daemon) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	scanner := protocol.NewScanner(conn)
	enc := json.NewEncoder(conn)

	for scanner.Scan() {
		var msg protocol.Msg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			enc.Encode(protocol.NewErrorMsg("invalid JSON: " + err.Error())) //nolint:errcheck
			continue
		}
		d.dispatch(ctx, conn, enc, msg)
	}
}

// dispatch routes one client message to the appropriate handler.
func (d *Daemon) dispatch(ctx context.Context, conn net.Conn, enc *json.Encoder, msg protocol.Msg) {
	// Decode the flat wire union into the typed request it represents. This both
	// validates presence (R-PROTO.11 — an attach naming no session, a user_turn
	// with no text) and rejects an unroutable type, so every branch below reads
	// a struct carrying exactly its own fields, already populated.
	req, err := protocol.DecodeRequest(msg)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	switch r := req.(type) {
	case protocol.NewConversationReq:
		id, err := d.newConversation(r.Interactive)
		if err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
			return
		}
		cid := protocol.NewConversationIDMsg(id)
		d.mu.RLock()
		if w := d.sessions[id]; w != nil {
			cid.Role = w.role()
		}
		cid.InstanceName = d.instName
		d.mu.RUnlock()
		enc.Encode(cid) //nolint:errcheck

	case protocol.AttachReq:
		resolved := d.resolveID(r.AgentID)
		if err := d.attach(resolved); err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
			return
		}
		d.mu.RLock()
		name := d.names[resolved]
		worker := d.sessions[resolved]
		instName := d.instName
		d.mu.RUnlock()
		var replayEvents []protocol.Msg
		var pendingResponse string
		if worker != nil {
			replayEvents, pendingResponse = worker.replay.snapshot()
		}
		// After a daemon restart the worker's in-memory ring is empty; source the
		// reattach replay from the durable journal so it still shows real history
		// (adr/event-log.md §7.6).
		if len(replayEvents) == 0 && pendingResponse == "" && d.store != nil {
			replayEvents, pendingResponse = d.journalReplay(resolved)
		}
		// The full transcript (all turns, incl. user prompts) is sourced from the
		// durable journal so reattaching shows the whole conversation, not a blank
		// screen. It supersedes the single-turn replay when available.
		ok := protocol.NewOKMsgWithReplay(resolved, name, replayEvents, pendingResponse)
		ok.InstanceName = instName
		if worker != nil {
			ok.Role = worker.role()
		}
		if d.store != nil {
			ok.History = d.journalHistory(resolved)
		}
		enc.Encode(ok) //nolint:errcheck

	case protocol.SetPlanModeReq:
		d.setPlanMode(enc, r.AgentID, r.Mode)

	case protocol.UserTurnReq:
		d.userTurn(ctx, enc, r.AgentID, r.Text, r.ForceThink)

	case protocol.ContextReq:
		d.handleContext(ctx, enc, r.AgentID)

	case protocol.SessionStopReq:
		d.handleSessionStop(enc, r.AgentID, r.All)

	case protocol.SessionDeleteReq:
		d.handleSessionDelete(enc, r.AgentID)

	case protocol.StandingShowReq:
		d.handleStandingShow(enc, r.ID, r.Limit)

	case protocol.StandingControlReq:
		d.handleStandingControl(enc, r.ID, r.Action)

	case protocol.ToolCallReq:
		d.handleToolCall(enc, r.Tool, r.Args, r.LiveState)
	case protocol.ToolDeleteReq:
		d.handleToolDelete(enc, r.Tool)

	case protocol.ListNotificationsReq:
		d.handleListNotifications(enc, r.All)

	case protocol.WorkflowStopReq:
		if d.store == nil {
			enc.Encode(protocol.NewErrorMsg("workflow stop not available")) //nolint:errcheck
			return
		}
		if _, err := d.store.WorkflowCancel(r.ID); err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		} else {
			enc.Encode(protocol.NewTextMsg(protocol.TypeWorkflowStop, "stopped")) //nolint:errcheck
		}

	case protocol.WorkflowFailReq:
		if d.store == nil {
			enc.Encode(protocol.NewErrorMsg("workflow fail not available")) //nolint:errcheck
			return
		}
		if _, err := d.store.WorkflowFail(r.ID, r.All); err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		} else {
			enc.Encode(protocol.NewTextMsg(protocol.TypeWorkflowFail, "failed")) //nolint:errcheck
		}

	case protocol.PluginCallReq:
		d.handlePluginCall(ctx, enc, r.Tool, r.Args)

	case protocol.HumanAnswerReq:
		d.handleHumanAnswer(enc, r)

	case protocol.GoalCreateReq:
		d.handleGoalCreate(ctx, enc, r)

	case protocol.GoalDeleteReq:
		d.handleGoalDelete(ctx, enc, r.ID)

	case protocol.SessionHistoryReq:
		d.handleSessionHistory(enc, r.AgentID)

	case protocol.SessionEventsReq:
		d.handleSessionEvents(enc, r.AgentID, r.Turn)

	case protocol.WatchReq:
		// Terminal: the watch owns the connection until it ends, then closes it.
		d.watch(ctx, conn, enc, r.AgentID)

	// The field-less verbs. They share one request type, so the inner switch is
	// on which verb rather than on which shape — there is only one shape.
	case protocol.QueryReq:
		switch r.Kind {
		case protocol.TypeStatus:
			d.handleStatus(enc)
		case protocol.TypeListGoals:
			d.handleListGoals(enc)
		case protocol.TypeListWorkflows:
			d.handleListWorkflows(enc)
		case protocol.TypeListTools:
			d.handleListTools(enc)
		case protocol.TypeListSkills:
			d.handleListSkills(enc)
		case protocol.TypePluginsList:
			d.handlePluginsList(enc)
		case protocol.TypePluginsReload:
			d.handlePluginsReload(enc)
		case protocol.TypeSessionsList:
			d.handleSessionsList(enc)
		case protocol.TypeStandingList:
			d.handleStandingList(enc)
		case protocol.TypeToolsList:
			d.handleToolsList(enc)
		case protocol.TypeToolsReload:
			d.handleToolsReload(enc)
		case protocol.TypeGrantsList:
			d.handleGrantsList(enc)
		}

	case protocol.GrantsDecideReq:
		d.handleGrantsDecide(enc, r)
	}
}

// ConfigureMemory stores the memory backend used by goal, reflection, and workflow handlers.
func (d *Daemon) ConfigureMemory(store queryBackend) {
	d.store = store
}

// ConfigureHITL wires the human-in-the-loop coordinator used to route
// human_input_answer messages and resolve session interactivity on attach.
func (d *Daemon) ConfigureHITL(h *HITL) {
	d.hitl = h
}

// isInteractive reports whether agentID was started as an interactive session.
// Defaults to false when no HITL coordinator is configured (e.g. in tests).
func (d *Daemon) isInteractive(agentID string) bool {
	if d.hitl == nil {
		return false
	}
	ok, err := d.hitl.IsInteractive(agentID)
	if err != nil {
		slog.Warn("interactive lookup failed", "agent_id", agentID, "err", err)
		return false
	}
	return ok
}

// AddSubscriber registers an optional journal subscriber (adr/reactive-events.md).
// Subscribers are hosted off the turn path and started by Start; a nil store
// (unconfigured) makes this a no-op. Call before Start.
func (d *Daemon) AddSubscriber(h subscribe.Handler) {
	if d.store == nil {
		slog.Warn("cannot add subscriber without a configured store", "subscriber", h.ID())
		return
	}
	d.mu.Lock()
	d.subs = append(d.subs, subscribe.New(d.store, h))
	d.mu.Unlock()
}

// NotifySubscribers wakes every registered subscriber to drain. Wired to the
// event sink's flush so subscribers react promptly to newly journaled events.
// Safe from any goroutine.
func (d *Daemon) NotifySubscribers() {
	d.mu.RLock()
	subs := d.subs
	d.mu.RUnlock()
	for _, s := range subs {
		s.Notify()
	}
}

// startSubscribers launches each registered subscriber's run loop.
func (d *Daemon) startSubscribers(ctx context.Context) {
	d.mu.RLock()
	subs := d.subs
	d.mu.RUnlock()
	for _, s := range subs {
		go s.Run(ctx)
	}
}

// ConfigurePlugins stores the plugin manager used by tool listing and direct plugin calls.
func (d *Daemon) ConfigurePlugins(mgr *plugin.Manager) {
	d.mgr = mgr
}

// ConfigureSandboxedTools stores the sandboxed-tool host, so `tools_list` and
// `tools_reload` can reach it. Passing nil (the default, when [tools] enabled is
// unset) leaves both messages answering that the subsystem is disabled.
// WakeAgent runs a turn on agentID now, with text as its input — the delivery
// half of a condition trigger (docs/scheduling.md).
//
// It reports whether the agent took it. A session that is not running cannot be
// woken, and one that is mid-turn declines: both are cases where the honest
// answer is "not now" rather than a queue, since a predicate's finding is about
// the world rather than about a request that must not be lost. The caller falls
// back to the human feed, so a refusal is never a dropped finding.
func (d *Daemon) WakeAgent(agentID, text string) bool {
	d.mu.RLock()
	w := d.sessions[agentID]
	d.mu.RUnlock()
	if w == nil {
		return false
	}
	return w.Wake(text)
}

// ConfigureStandingTools stores the standing-run driver so the operator surface
// (standing_list / standing_show / standing_control) can reach it. Nil leaves
// those messages answering that no standing tools exist here.
func (d *Daemon) ConfigureStandingTools(r *StandingRunner) { d.standing = r }

func (d *Daemon) ConfigureSandboxedTools(h *toolvm.Host) {
	d.tools = h
}

// ConfigureCapabilities installs the capability surface behind `grants_list` and
// `grants_decide`. Nil leaves those messages answering that no store is present.
func (d *Daemon) ConfigureCapabilities(s *CapabilityService) { d.grants = s }

// Capabilities exposes the capability surface so the API server can serve the same
// decision path the CLI and TUI use rather than reimplementing it.
func (d *Daemon) Capabilities() *CapabilityService { return d.grants }

// ConfigureGeneratedTools stores the generated-tool store, so the operator can
// delete a tool Nine wrote. Nil leaves the operator's delete refusing.
func (d *Daemon) ConfigureGeneratedTools(g agent.GeneratedToolStore) { d.generated = g }

// ConfigureCoreTools stores the dispatcher carrying the core-intercepted tools
// (memory/file/skill/doc — the ones handled in-process rather than by a
// subprocess plugin) so `plugin_call` can reach them. `list_tools` advertises
// them under the `core` plugin, so without this a client can see them but not
// call them. Passing nil leaves plugin_call plugin-only.
func (d *Daemon) ConfigureCoreTools(disp *agent.Dispatcher) {
	d.core = disp
}

// newUUID returns a random UUID v4.
func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b) //nolint:errcheck
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
