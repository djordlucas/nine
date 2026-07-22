package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"nine/internal/agent"
	"nine/internal/protocol"
)

// handleStatus responds with daemon uptime, active agents, and loaded plugins.
func (d *Daemon) handleStatus(enc *json.Encoder) {
	d.mu.RLock()
	agents := make([]protocol.AgentInfo, 0, len(d.sessions))
	for id, w := range d.sessions {
		agents = append(agents, protocol.AgentInfo{ID: id, Name: d.names[id], Role: w.role(), PlanMode: w.planMode()})
	}
	d.mu.RUnlock()

	plugins := []string{}
	if d.mgr != nil {
		plugins = d.mgr.ListRunning()
	}

	var subAgents []protocol.SubAgentInfo
	if d.listSubAgents != nil {
		subAgents = d.listSubAgents()
	}

	var queue *protocol.QueueStat
	if d.queueDepth != nil {
		pending, inflight, maxConcurrent := d.queueDepth()
		queue = &protocol.QueueStat{Pending: pending, Inflight: inflight, MaxConcurrent: maxConcurrent}
	}

	info := protocol.StatusInfo{
		Agents:    agents,
		SubAgents: subAgents,
		Plugins:   plugins,
		Uptime:    time.Since(d.startedAt).Truncate(time.Second).String(),
		LLMQueue:  queue,
	}
	data, _ := json.Marshal(info)
	enc.Encode(protocol.NewTextMsg("status", string(data))) //nolint:errcheck
}

// handleContext returns a JSON-encoded breakdown of a session's currently
// assembled context (docs/wire-protocol). It performs no LLM call — the worker
// snapshots the same deterministic assembly it runs before each turn. An idle
// but known conversation is revived from its checkpoint first, mirroring
// userTurn.
func (d *Daemon) handleContext(ctx context.Context, enc *json.Encoder, agentID string) {
	resolved := d.resolveID(agentID)
	d.mu.RLock()
	r, ok := d.sessions[resolved]
	d.mu.RUnlock()
	if !ok {
		if err := d.attach(resolved); err != nil {
			enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("conversation %s not found", agentID))) //nolint:errcheck
			return
		}
		d.mu.RLock()
		r = d.sessions[resolved]
		d.mu.RUnlock()
	}
	if r == nil {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("conversation %s not found", agentID))) //nolint:errcheck
		return
	}
	rep, err := r.InspectContext(ctx)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg("inspect context: " + err.Error())) //nolint:errcheck
		return
	}
	data, _ := json.Marshal(rep)
	enc.Encode(protocol.NewTextMsg("context", string(data))) //nolint:errcheck
}

// handleListGoals returns a JSON-encoded goal list.
func (d *Daemon) handleListGoals(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg("list_goals", `[]`)) //nolint:errcheck
		return
	}
	goals, err := d.store.GoalList()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	data, _ := json.Marshal(map[string]any{"goals": goals})
	enc.Encode(protocol.NewTextMsg("list_goals", string(data))) //nolint:errcheck
}

// handleListReflections returns a JSON-encoded reflection list.
func (d *Daemon) handleListReflections(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg("list_reflections", `[]`)) //nolint:errcheck
		return
	}
	refs, err := d.store.ReflectionList()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	data, _ := json.Marshal(map[string]any{"reflections": refs})
	enc.Encode(protocol.NewTextMsg("list_reflections", string(data))) //nolint:errcheck
}

// handleListNotifications returns the human-facing notification feed as JSON.
// By default it returns unseen entries and marks them seen (an inbox that
// drains as it is read); flag "--all" returns the full history and marks
// nothing.
func (d *Daemon) handleListNotifications(enc *json.Encoder, flag string) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg("list_notifications", `{"notifications":[]}`)) //nolint:errcheck
		return
	}
	all := flag == "--all"
	ns, err := d.store.UserNotificationList(!all)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if !all {
		for _, n := range ns {
			if merr := d.store.UserNotificationMarkSeen(n.ID); merr != nil {
				slog.Warn("mark notification seen", "id", n.ID, "err", merr)
			}
		}
	}
	data, _ := json.Marshal(map[string]any{"notifications": ns})
	enc.Encode(protocol.NewTextMsg("list_notifications", string(data))) //nolint:errcheck
}

// handleListWorkflows returns a JSON-encoded workflow list.
func (d *Daemon) handleListWorkflows(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg("list_workflows", `[]`)) //nolint:errcheck
		return
	}
	workflows, err := d.store.WorkflowList("")
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	data, _ := json.Marshal(map[string]any{"workflows": workflows})
	enc.Encode(protocol.NewTextMsg("list_workflows", string(data))) //nolint:errcheck
}

// newConversation creates a AgentWorker with a fresh agent loop and registers
// it. interactive marks the session HITL-eligible and persists that flag so it
// survives a daemon restart (R-HITL.1).
func (d *Daemon) newConversation(interactive bool) (string, error) {
	id := newUUID()
	if interactive && d.hitl != nil {
		if err := d.hitl.MarkInteractive(id); err != nil {
			slog.Warn("mark interactive failed", "id", id, "err", err)
		}
	}
	r := d.makeAgentWorker(id, nil, interactive)
	d.mu.Lock()
	d.sessions[id] = r
	d.mu.Unlock()
	slog.Info("conversation created", "id", id, "interactive", interactive)
	return id, nil
}

// handleHumanAnswer routes a human's answer to the blocked ask_human call for
// its RequestID. It is a top-level message, not a turn — it never starts a new
// agent loop (R-HITL.6).
func (d *Daemon) handleHumanAnswer(enc *json.Encoder, msg protocol.Msg) {
	if d.hitl == nil {
		enc.Encode(protocol.NewErrorMsg("human-in-the-loop not available")) //nolint:errcheck
		return
	}
	if msg.RequestID == "" {
		enc.Encode(protocol.NewErrorMsg("human_input_answer: request_id required")) //nolint:errcheck
		return
	}
	if !d.hitl.Answer(msg.RequestID, msg.Answer) {
		enc.Encode(protocol.NewErrorMsg("no pending question for that request")) //nolint:errcheck
		return
	}
	enc.Encode(protocol.NewTextMsg("human_input_answer", "ok")) //nolint:errcheck
}

// attach restores an existing conversation from its checkpoint if not already
// running. Returns an error if the conversation is not found.
func (d *Daemon) attach(agentID string) error {
	d.mu.RLock()
	_, ok := d.sessions[agentID]
	d.mu.RUnlock()
	if ok {
		slog.Debug("conversation already active", "id", agentID)
		return nil
	}

	if d.ckpt == nil {
		return fmt.Errorf("conversation %s not found", agentID)
	}
	data, found, err := d.ckpt.Load(agentID)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}
	if !found {
		return fmt.Errorf("conversation %s not found", agentID)
	}

	r := d.makeAgentWorker(agentID, data, d.isInteractive(agentID))

	var name string
	if nb, found, err := d.ckpt.Load("name:" + agentID); err == nil && found {
		name = string(nb)
	}

	d.mu.Lock()
	d.sessions[agentID] = r
	if name != "" {
		d.names[agentID] = name
	}
	d.mu.Unlock()
	slog.Info("conversation resumed from checkpoint", "id", agentID)
	return nil
}

// makeAgentWorker creates a AgentWorker for agentID, optionally restoring from
// checkpointData. interactive enables the loop's human-in-the-loop tools.
// The session's plan is loaded first so its profile picks the worker's role
// (active → orchestrator, idle-reflection → reflection, pursue → pursue;
// docs/roles.md §6).
func (d *Daemon) makeAgentWorker(id string, checkpointData []byte, interactive bool) *AgentWorker {
	plan, err := loadOrCreatePlan(context.Background(), d.plans, id, defaultProfile, false)
	if err != nil {
		slog.Warn("load session plan failed", "agent_id", id, "err", err)
		plan = nil
	}

	loop := d.factory(id, RoleParams{
		Role:        roleNameForPlan(plan),
		Interactive: interactive,
		OwnsGoal:    planOwnsGoal(plan),
		Delegates:   planDelegates(plan),
	})
	if checkpointData != nil {
		loop.LoadState(checkpointData) //nolint:errcheck
	}

	var saveFn func(string, []byte) error
	if d.ckpt != nil {
		ckpt := d.ckpt
		saveFn = func(id string, data []byte) error { return ckpt.Save(id, data) }
	}

	var notifFn func(string) ([]string, error)
	if d.notif != nil {
		notif := d.notif
		notifFn = func(id string) ([]string, error) { return notif.Fetch(id) }
	}

	r := newAgentWorker(id, loop, saveFn, notifFn, d.stall, plan, d.sink)
	if d.sup != nil {
		sup := d.sup
		r.onComplete = func(agentID string) {
			sup.Post(Event{Kind: EventAgentCompletes, AgentID: agentID})
		}
	}
	return r
}

// userTurn routes a user message to the AgentWorker for agentID, streaming any
// progress events to enc before sending the final response and done messages.
func (d *Daemon) userTurn(ctx context.Context, enc *json.Encoder, agentID, text string, forceThink bool) {
	d.mu.RLock()
	r, ok := d.sessions[agentID]
	existing := d.names[agentID]
	d.mu.RUnlock()
	if !ok {
		// A turn addressed to a known-but-idle conversation (e.g. after a daemon
		// restart) revives it from its checkpoint rather than erroring — attach
		// loads the checkpoint and registers the worker. It errors only when no
		// such conversation exists.
		if err := d.attach(agentID); err != nil {
			enc.Encode(protocol.NewAgentErrorMsg(agentID, fmt.Sprintf("conversation %s not found; use new_conversation first", agentID))) //nolint:errcheck
			return
		}
		d.mu.RLock()
		r = d.sessions[agentID]
		existing = d.names[agentID]
		d.mu.RUnlock()
	}

	if existing == "" {
		if name := nameFromPrompt(text); name != "" {
			d.mu.Lock()
			d.names[agentID] = name
			d.mu.Unlock()
			if d.ckpt != nil {
				d.ckpt.Save("name:"+agentID, []byte(name)) //nolint:errcheck
			}
			enc.Encode(protocol.NewSetNameMsg(agentID, name)) //nolint:errcheck
		}
	}

	progressCh := make(chan protocol.Msg, 256)
	r.setProgress(func(msg protocol.Msg) {
		select {
		case progressCh <- msg:
		default:
		}
	})
	defer r.setProgress(nil)

	respCh, err := r.turnAsync(ctx, text, forceThink)
	if err != nil {
		enc.Encode(protocol.NewAgentErrorMsg(agentID, err.Error())) //nolint:errcheck
		return
	}

	for {
		select {
		case evt := <-progressCh:
			enc.Encode(evt) //nolint:errcheck
		case res := <-respCh:
			// Drain any progress events that arrived before the response.
			for {
				select {
				case evt := <-progressCh:
					enc.Encode(evt) //nolint:errcheck
				default:
					if res.err != nil {
						enc.Encode(protocol.NewAgentErrorMsg(agentID, res.err.Error())) //nolint:errcheck
					} else {
						enc.Encode(protocol.NewResponseMsg(agentID, res.text)) //nolint:errcheck
						enc.Encode(protocol.NewDoneMsg(agentID))               //nolint:errcheck
					}
					return
				}
			}
		}
	}
}

// handleSessionStop terminates one session (or every session, when all is set):
// it stops the running worker, drops it from the active maps, and deletes its
// persisted state so it is neither revived on the next turn/attach nor resumed
// after a daemon restart. It is the operator's escape hatch for a misbehaving or
// accidentally-created session (e.g. one spawned by a mistyped command).
func (d *Daemon) handleSessionStop(enc *json.Encoder, agentID string, all bool) {
	if all {
		d.mu.Lock()
		workers := make([]*AgentWorker, 0, len(d.sessions))
		ids := make([]string, 0, len(d.sessions))
		for id, w := range d.sessions {
			workers = append(workers, w)
			ids = append(ids, id)
			delete(d.names, id)
		}
		d.sessions = make(map[string]*AgentWorker)
		d.mu.Unlock()

		for _, w := range workers {
			w.stop()
		}
		for _, id := range ids {
			d.forgetSession(id)
		}
		enc.Encode(protocol.NewTextMsg("session_stop", fmt.Sprintf("stopped %d session(s)", len(ids)))) //nolint:errcheck
		return
	}

	resolved := d.resolveID(agentID)
	d.mu.Lock()
	w, running := d.sessions[resolved]
	delete(d.sessions, resolved)
	delete(d.names, resolved)
	d.mu.Unlock()

	if running {
		w.stop()
	}
	// Delete persisted state even for an idle session (checkpoint only, no live
	// worker). existed reports whether any trace of the session was found, so a
	// stray id is reported as not-found rather than silently "stopped".
	existed := d.forgetSession(resolved)
	if !running && !existed {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("session %s not found", agentID))) //nolint:errcheck
		return
	}
	slog.Info("session stopped", "id", resolved)
	enc.Encode(protocol.NewTextMsg("session_stop", "stopped "+resolved)) //nolint:errcheck
}

// forgetSession deletes a session's durable state: its checkpoint and, when a
// plan store is configured, its session plan (archived so it is not resumed at
// startup). It reports whether any persisted trace of the session existed.
// Best-effort — persistence errors are logged, not returned.
func (d *Daemon) forgetSession(id string) bool {
	existed := false
	if d.ckpt != nil {
		if _, found, err := d.ckpt.Load(id); err == nil && found {
			existed = true
		}
		if err := d.ckpt.Delete(id); err != nil {
			slog.Warn("delete checkpoint failed", "id", id, "err", err)
		}
	}
	if d.plans != nil {
		if p, err := d.plans.SessionPlanGet(id); err == nil && p != nil {
			existed = true
			p.Status = "archived"
			if err := d.plans.SessionPlanSave(p); err != nil {
				slog.Warn("archive session plan failed", "id", id, "err", err)
			}
		}
	}
	return existed
}

// handleListTools returns all tool definitions from core and all loaded plugins.
func (d *Daemon) handleListTools(enc *json.Encoder) {
	var tools []protocol.ToolSummary
	if d.mgr != nil {
		for _, def := range agent.InterceptedDefs {
			tools = append(tools, protocol.ToolSummary{Plugin: "core", Name: def.Name, Description: def.Description})
		}
		for _, p := range d.mgr.Running() {
			for _, t := range p.Tools {
				tools = append(tools, protocol.ToolSummary{Plugin: p.Name, Name: t.Name, Description: t.Description})
			}
		}
	}
	if tools == nil {
		tools = []protocol.ToolSummary{}
	}
	data, _ := json.Marshal(tools)
	enc.Encode(protocol.NewTextMsg("list_tools", string(data))) //nolint:errcheck
}

// handlePluginCall routes a tool call directly to the owning plugin.
func (d *Daemon) handlePluginCall(ctx context.Context, enc *json.Encoder, toolName string, args json.RawMessage) {
	if d.mgr == nil {
		enc.Encode(protocol.NewErrorMsg("plugin caller not available")) //nolint:errcheck
		return
	}
	for _, p := range d.mgr.Running() {
		for _, t := range p.Tools {
			if t.Name == toolName {
				result, err := d.mgr.Call(ctx, p, toolName, args)
				if err != nil {
					enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
					return
				}
				enc.Encode(protocol.NewTextMsg("plugin_call", result.Output)) //nolint:errcheck
				return
			}
		}
	}
	enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("unknown tool: %s", toolName))) //nolint:errcheck
}

// handlePluginsList returns the plugin roster: built-in and user plugins that are
// running, plus user plugins that were skipped at load with the reason.
func (d *Daemon) handlePluginsList(enc *json.Encoder) {
	data, _ := json.Marshal(d.pluginStatuses())
	enc.Encode(protocol.NewTextMsg("plugins_list", string(data))) //nolint:errcheck
}

// handlePluginsReload re-scans the user-plugin directory (stopping and restarting
// only user plugins) and returns the resulting roster.
func (d *Daemon) handlePluginsReload(enc *json.Encoder) {
	if d.mgr == nil {
		enc.Encode(protocol.NewErrorMsg("plugin manager not available")) //nolint:errcheck
		return
	}
	d.mgr.ReloadUserPlugins()
	data, _ := json.Marshal(d.pluginStatuses())
	enc.Encode(protocol.NewTextMsg("plugins_reload", string(data))) //nolint:errcheck
}

// pluginStatuses assembles the plugin roster from the manager: every running
// plugin (built-in or user) with its tools, followed by user plugins that failed
// to load, each with its skip reason.
func (d *Daemon) pluginStatuses() []protocol.PluginStatus {
	out := []protocol.PluginStatus{}
	if d.mgr == nil {
		return out
	}
	for _, p := range d.mgr.Running() {
		source := "builtin"
		if p.User {
			source = "user"
		}
		names := make([]string, len(p.Tools))
		for i, t := range p.Tools {
			names[i] = t.Name
		}
		out = append(out, protocol.PluginStatus{Name: p.Name, Source: source, Loaded: true, Tools: names})
	}
	// Skipped user plugins are not in Running(); surface them with their reason.
	for _, st := range d.mgr.UserStatus() {
		if st.Loaded {
			continue
		}
		out = append(out, protocol.PluginStatus{Name: st.Name, Source: "user", Loaded: false, Error: st.Err})
	}
	return out
}

// resolveID maps a friendly name or UUID prefix to a full agent ID.
// Returns query unchanged if no match is found.
func (d *Daemon) resolveID(query string) string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	for id, name := range d.names {
		if name == query {
			return id
		}
	}
	for id := range d.sessions {
		if strings.HasPrefix(id, query) {
			return id
		}
	}
	return query
}

// setPlanMode changes agentID's reasoning mode live (the set_plan_mode command),
// with no daemon restart. Rejects unknown modes.
func (d *Daemon) setPlanMode(enc *json.Encoder, agentID, mode string) {
	switch mode {
	case agent.PlanModeOff, agent.PlanModePlanOnly, agent.PlanModeAlways:
	default:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("invalid plan mode %q (want off | plan-only | always)", mode))) //n
		return
	}
	d.mu.RLock()
	w, ok := d.sessions[agentID]
	d.mu.RUnlock()
	if !ok {
		enc.Encode(protocol.NewAgentErrorMsg(agentID, "conversation not found")) //nolint:errcheck
		return
	}
	w.setPlanMode(mode)
	enc.Encode(protocol.NewTextMsg("set_plan_mode", "plan mode: "+mode)) //nolint:errcheck
}
