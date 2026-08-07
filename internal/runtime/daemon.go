// Package runtime implements the Nine daemon: a Unix-socket server that
// routes user turns to per-conversation agent loops and persists state via
// checkpoints. See nine/internal/protocol for the wire protocol and the
// client used by the CLI/TUI to connect to it.
package runtime

import (
	"bufio"
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
	// Role is the role name to resolve (docs/roles.md §6).
	Role string
	// Interactive enables human-in-the-loop tools; effective only for
	// HITL-eligible roles (R-HITL.1). AND-ed with the role's Interactive flag.
	Interactive bool
	// OwnsGoal marks a pursue-shell session that steers its own goal, granting
	// the goal self-management tools regardless of the role's allowlist
	// (docs/predefined-agents.md §3.1).
	OwnsGoal bool
	// Delegates opts a standing agent into sub-agent fan-out; OR-ed with the
	// resolved role's own Delegates flag (docs/predefined-agents.md §3.1).
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

// NotifStore retrieves (and marks as delivered) pending notifications for an agent.
type NotifStore interface {
	Fetch(agentID string) ([]string, error)
}

// queryBackend is the slice of the memory store that message handlers query
// on the client's behalf (list_goals, list_workflows, workflow_stop/fail,
// etc.). The daemon doesn't own or reason about this data — it's a thin proxy
// between the Unix-socket clients and the shared backend.
type queryBackend interface {
	GoalList() ([]memory.Goal, error)
	ReflectionList() ([]memory.Reflection, error)
	WorkflowList(agentID string) ([]workflow.Workflow, error)
	WorkflowCancel(id string) (int, error)
	WorkflowFail(id string, all bool) (int, error)
	UserNotificationList(unseenOnly bool) ([]memory.UserNotification, error)
	UserNotificationMarkSeen(id string) error
	SessionEventsByAgent(agentID string) ([]memory.SessionEvent, error)
	// Journal subscription read surface (satisfies subscribe.Store) so the
	// daemon can host cursor-backed subscribers (docs/reactive-events.md).
	SessionEventsAfter(afterSeq int64, limit int) ([]memory.SessionEvent, error)
	EventCursorGet(subscriberID string) (int64, error)
	EventCursorSet(subscriberID string, seq int64) error
}

// pluginRegistry provides the plugin access needed by message handlers.
type pluginRegistry interface {
	ListRunning() []string
	Running() []*plugin.Plugin
	Call(ctx context.Context, p *plugin.Plugin, toolName string, args json.RawMessage) (plugin.CallResult, error)
	ReloadUserPlugins()
	UserStatus() []plugin.UserPluginStatus
}

// Daemon accepts connections on a Unix socket and routes messages to
// per-conversation session workers.
type Daemon struct {
	socketPath string
	factory    LoopFactory
	ckpt       CheckpointStore
	notif      NotifStore
	stall      StallConfig
	startedAt  time.Time
	names      map[string]string // agentID → display name (set on first turn)
	instName   string            // instance display name shown in the TUI top bar (guarded by mu)

	mgr   pluginRegistry
	tools *toolvm.Host      // sandboxed-tool host (see ConfigureSandboxedTools); nil = disabled
	core  *agent.Dispatcher // core-intercepted tools, for plugin_call (see ConfigureCoreTools)
	store queryBackend
	plans PlanStore
	sup   *Supervisor
	hitl  *HITL
	sink  EventSink // durable session-event journal for new workers (nil = disabled)

	maxGoalSessions int // see SetMaxGoalSessions

	listSubAgents func() []protocol.SubAgentInfo
	queueDepth    func() (pending, inflight, maxConcurrent int)

	subs []*subscribe.Subscription // optional journal subscribers (docs/reactive-events.md)

	mu       sync.RWMutex
	sessions map[string]*AgentWorker
	listener net.Listener
}

// New creates a Daemon. ckpt and notif may be nil (disables persistence and
// notifications respectively).
func New(socketPath string, factory LoopFactory, ckpt CheckpointStore, notif NotifStore) *Daemon {
	return &Daemon{
		socketPath: socketPath,
		factory:    factory,
		ckpt:       ckpt,
		notif:      notif,
		sessions:   make(map[string]*AgentWorker),
		names:      make(map[string]string),
		startedAt:  time.Now(),
	}
}

// SetStallConfig configures stall detection applied to all new AgentWorkers.
// Must be called before the first conversation is created.
func (d *Daemon) SetStallConfig(cfg StallConfig) { d.stall = cfg }

// SetEventSink registers the durable session-event journal handed to every new
// AgentWorker (docs/event-log.md). Must be called before the first conversation
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
// ring is empty — e.g. a session revived after a daemon restart (docs/event-log.md
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
				msgs = append(msgs, protocol.NewToolStartMsg(agentID, p.Name, "", p.Input))
			}
		case "tool_end":
			var p toolEndPayload
			if json.Unmarshal(e.Payload, &p) == nil {
				msgs = append(msgs, protocol.NewToolEndMsg(agentID, p.Name, "", p.Input, p.Output))
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

// journalHistory reconstructs the full multi-turn transcript for agentID from
// the durable journal — user prompts, tool/sub-agent activity, and assistant
// responses, in chronological order — as the ordered protocol messages a
// reattaching client renders. This makes the reconnected conversation look as
// it did before the client detached, rather than blank. Only the most recent
// historyMaxTurns turns are included.
func (d *Daemon) journalHistory(agentID string) []protocol.Msg {
	events, err := d.store.SessionEventsByAgent(agentID)
	if err != nil || len(events) == 0 {
		return nil
	}
	minTurn := events[len(events)-1].Turn - historyMaxTurns + 1
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
			m = protocol.NewToolStartMsg(agentID, p.Name, "", p.Input)
		case "tool_end":
			var p toolEndPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			m = protocol.NewToolEndMsg(agentID, p.Name, "", p.Input, p.Output)
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
	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)

	for scanner.Scan() {
		var msg protocol.Msg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			enc.Encode(protocol.NewErrorMsg("invalid JSON: " + err.Error())) //nolint:errcheck
			continue
		}
		d.dispatch(ctx, enc, msg)
	}
}

// dispatch routes one client message to the appropriate handler.
func (d *Daemon) dispatch(ctx context.Context, enc *json.Encoder, msg protocol.Msg) {
	switch msg.Type {
	case "new_conversation":
		id, err := d.newConversation(msg.Interactive)
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

	case "attach":
		resolved := d.resolveID(msg.AgentID)
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
		// (docs/event-log.md §7.6).
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

	case "set_plan_mode":
		d.setPlanMode(enc, msg.AgentID, msg.Text)

	case "user_turn":
		d.userTurn(ctx, enc, msg.AgentID, msg.Text, msg.ForceThink)

	case "status":
		d.handleStatus(enc)

	case "context":
		d.handleContext(ctx, enc, msg.AgentID)

	case "session_stop":
		d.handleSessionStop(enc, msg.AgentID, msg.Text == "--all")

	case "list_goals":
		d.handleListGoals(enc)

	case "list_reflections":
		d.handleListReflections(enc)

	case "list_notifications":
		d.handleListNotifications(enc, msg.Text)

	case "list_workflows":
		d.handleListWorkflows(enc)

	case "workflow_stop":
		if d.store == nil {
			enc.Encode(protocol.NewErrorMsg("workflow stop not available")) //nolint:errcheck
			return
		}
		if _, err := d.store.WorkflowCancel(msg.Text); err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		} else {
			enc.Encode(protocol.NewTextMsg("workflow_stop", "stopped")) //nolint:errcheck
		}

	case "workflow_fail":
		if d.store == nil {
			enc.Encode(protocol.NewErrorMsg("workflow fail not available")) //nolint:errcheck
			return
		}
		all := msg.Text == "--all"
		id := ""
		if !all {
			id = msg.Text
		}
		if _, err := d.store.WorkflowFail(id, all); err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		} else {
			enc.Encode(protocol.NewTextMsg("workflow_fail", "failed")) //nolint:errcheck
		}

	case "list_tools":
		d.handleListTools(enc)

	case "plugin_call":
		d.handlePluginCall(ctx, enc, msg.ToolName, msg.ToolInput)

	case "plugins_list":
		d.handlePluginsList(enc)

	case "plugins_reload":
		d.handlePluginsReload(enc)

	case "tools_list":
		d.handleToolsList(enc)

	case "tools_reload":
		d.handleToolsReload(enc)

	case "human_input_answer":
		d.handleHumanAnswer(enc, msg)

	default:
		enc.Encode(protocol.NewErrorMsg("unknown message type: " + msg.Type)) //nolint:errcheck
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

// ConfigurePlanStore stores the backend used to load, create, and resume
// session_plans rows.
func (d *Daemon) ConfigurePlanStore(plans PlanStore) {
	d.plans = plans
}

// ResumeSessions starts session workers for every session_plans row with
// status "active" and at least one active, idle-capable stage (Pilot 4's
// general resume rule), so background sessions survive a daemon restart.
// Ordinary [active] conversations have no idle-capable stage and stay
// attach-on-demand. Safe to call once at startup, after ConfigurePlanStore.
func (d *Daemon) ResumeSessions(ctx context.Context) error {
	if d.plans == nil {
		return nil
	}
	plans, err := d.plans.SessionPlanListActive()
	if err != nil {
		return fmt.Errorf("list active session plans: %w", err)
	}
	for _, p := range plans {
		if !planNeedsResume(p) {
			continue
		}
		d.mu.RLock()
		_, running := d.sessions[p.ID]
		d.mu.RUnlock()
		if running {
			continue
		}

		var data []byte
		if d.ckpt != nil {
			data, _, _ = d.ckpt.Load(p.ID) //nolint:errcheck // best-effort; makeAgentWorker handles a missing checkpoint
		}
		r := d.makeAgentWorker(p.ID, data, false)
		d.mu.Lock()
		d.sessions[p.ID] = r
		d.mu.Unlock()
		slog.Info("session resumed at startup", "id", p.ID)
	}
	return nil
}

// AddSubscriber registers an optional journal subscriber (docs/reactive-events.md).
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
func (d *Daemon) ConfigureSandboxedTools(h *toolvm.Host) {
	d.tools = h
}

// ConfigureCoreTools stores the dispatcher carrying the core-intercepted tools
// (memory/file/skill/doc — the ones handled in-process rather than by a
// subprocess plugin) so `plugin_call` can reach them. `list_tools` advertises
// them under the `core` plugin, so without this a client can see them but not
// call them. Passing nil leaves plugin_call plugin-only.
func (d *Daemon) ConfigureCoreTools(disp *agent.Dispatcher) {
	d.core = disp
}

// ConfigureSupervisor stores the supervisor and wires stall detection and
// turn-completion events. Stall fires after 5 consecutive turns with no tool calls.
func (d *Daemon) ConfigureSupervisor(sup *Supervisor) {
	d.sup = sup
	d.stall = StallConfig{
		Limit: 5,
		OnStall: func(agentID string) {
			sup.Post(Event{Kind: EventGoalStalls, AgentID: agentID})
		},
	}
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
