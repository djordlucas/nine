package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"nine/internal/agent"
	"nine/internal/memory"
	"nine/internal/plugin"
	"nine/internal/protocol"
	"nine/internal/toolvm"
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
	enc.Encode(protocol.NewTextMsg(protocol.TypeStatus, string(data))) //nolint:errcheck
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
	enc.Encode(protocol.NewTextMsg(protocol.TypeContext, string(data))) //nolint:errcheck
}

// handleListGoals returns a JSON-encoded goal list.
func (d *Daemon) handleListGoals(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg(protocol.TypeListGoals, `[]`)) //nolint:errcheck
		return
	}
	goals, err := d.store.GoalList()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	data, _ := json.Marshal(map[string]any{"goals": goals})
	enc.Encode(protocol.NewTextMsg(protocol.TypeListGoals, string(data))) //nolint:errcheck
}

// handleListNotifications returns the human-facing notification feed as JSON.
// By default it returns unseen entries and marks them seen (an inbox that
// drains as it is read); flag "--all" returns the full history and marks
// nothing.
func (d *Daemon) handleListNotifications(enc *json.Encoder, all bool) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg(protocol.TypeListNotifications, `{"notifications":[]}`)) //nolint:errcheck
		return
	}
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
	enc.Encode(protocol.NewTextMsg(protocol.TypeListNotifications, string(data))) //nolint:errcheck
}

// handleListWorkflows returns a JSON-encoded workflow list.
func (d *Daemon) handleListWorkflows(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewTextMsg(protocol.TypeListWorkflows, `[]`)) //nolint:errcheck
		return
	}
	workflows, err := d.store.WorkflowList("")
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	data, _ := json.Marshal(map[string]any{"workflows": workflows})
	enc.Encode(protocol.NewTextMsg(protocol.TypeListWorkflows, string(data))) //nolint:errcheck
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
func (d *Daemon) handleHumanAnswer(enc *json.Encoder, req protocol.HumanAnswerReq) {
	if d.hitl == nil {
		enc.Encode(protocol.NewErrorMsg("human-in-the-loop not available")) //nolint:errcheck
		return
	}
	if !d.hitl.Answer(req.RequestID, req.Answer) {
		enc.Encode(protocol.NewErrorMsg("no pending question for that request")) //nolint:errcheck
		return
	}
	enc.Encode(protocol.NewTextMsg(protocol.TypeHumanInputAnswer, "ok")) //nolint:errcheck
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
// adr/roles-design.md §6).
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
		enc.Encode(protocol.NewTextMsg(protocol.TypeSessionStop, fmt.Sprintf("stopped %d session(s)", len(ids)))) //nolint:errcheck
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
	enc.Encode(protocol.NewTextMsg(protocol.TypeSessionStop, "stopped "+resolved)) //nolint:errcheck
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
	// Sandboxed tools are listed under their own pseudo-plugin, so plugin_call's
	// reach matches what this advertises (R-PROTO.5) and an operator can tell at a
	// glance which tools run in the wasm sandbox rather than a subprocess.
	if d.tools != nil {
		for _, t := range d.tools.Tools() {
			tools = append(tools, protocol.ToolSummary{Plugin: "sandboxed", Name: t.Name, Description: t.Description})
		}
	}
	if tools == nil {
		tools = []protocol.ToolSummary{}
	}
	data, _ := json.Marshal(tools)
	enc.Encode(protocol.NewTextMsg(protocol.TypeListTools, string(data))) //nolint:errcheck
}

// handlePluginCall routes a tool call directly to its handler: the core
// dispatcher for core-intercepted tools, otherwise the owning plugin.
//
// Core comes first because those tools are not backed by any subprocess — the
// memory/file/skill/doc handlers live in-process (spec/contracts/plugin.md
// R-PLUG.5) — yet list_tools advertises them under the `core` plugin, so a
// plugin-only lookup would report a tool the client can see as unknown. It also
// cannot collide with a plugin tool: the builder registers both onto one
// dispatcher per loop, so a duplicate name is already a name clash there.
//
// Sandboxed tools are resolved against the host itself rather than through a
// dispatcher, because the host is the live authority: `nine tools reload` swaps
// its registry, and a dispatcher built once at assembly would keep answering
// from the tool set that existed at boot. Agent loops have no such problem —
// they are rebuilt per turn, which is exactly the next-turn visibility R-TVM.11
// specifies — but this surface is built once and must not go stale.
func (d *Daemon) handlePluginCall(ctx context.Context, enc *json.Encoder, toolName string, args json.RawMessage) {
	if d.tools != nil && d.tools.Get(toolName) != nil {
		out, err := d.tools.Call(ctx, toolName, args)
		if err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
			return
		}
		enc.Encode(protocol.NewTextMsg(protocol.TypePluginCall, out)) //nolint:errcheck
		return
	}
	if d.core != nil && d.core.Has(toolName) {
		result, err := d.core.Dispatch(ctx, toolName, args)
		if err != nil {
			enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
			return
		}
		enc.Encode(protocol.NewTextMsg(protocol.TypePluginCall, result.Output)) //nolint:errcheck
		return
	}
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
				enc.Encode(protocol.NewTextMsg(protocol.TypePluginCall, result.Output)) //nolint:errcheck
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
	enc.Encode(protocol.NewTextMsg(protocol.TypePluginsList, string(data))) //nolint:errcheck
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
	enc.Encode(protocol.NewTextMsg(protocol.TypePluginsReload, string(data))) //nolint:errcheck
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
		out = append(out, protocol.PluginStatus{
			Name: st.Name, Source: "user", Loaded: false, Error: st.Err,
			Disabled: st.Err == plugin.ErrPluginDisabled.Error(),
		})
	}
	// Default plugins the operator switched off. They are not in Running() and
	// have no UserStatus, so without this they would simply be absent — and an
	// operator debugging a missing tool would have nothing to read.
	for _, name := range d.mgr.DisabledSkipped() {
		out = append(out, protocol.PluginStatus{
			Name: name, Source: "builtin", Loaded: false,
			Error: plugin.ErrPluginDisabled.Error(), Disabled: true,
		})
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
	enc.Encode(protocol.NewTextMsg(protocol.TypeSetPlanMode, "plan mode: "+mode)) //nolint:errcheck
}

// handleToolsList returns the sandboxed-tool roster: every tool loaded from
// [tools].user_dir with its resolved capabilities, plus every candidate that was
// skipped at load with the reason (spec/contracts/toolvm.md).
//
// The skipped entries are the reason this message exists. A capability mismatch
// is deliberately a load failure rather than a degraded tool, and that promise
// is only kept if the operator can read the failure somewhere.
func (d *Daemon) handleToolsList(enc *json.Encoder) {
	data, _ := json.Marshal(d.sandboxedToolStatuses())
	enc.Encode(protocol.NewTextMsg(protocol.TypeToolsList, string(data))) //nolint:errcheck
}

// handleToolsReload re-scans the sandboxed-tool directory and returns the
// resulting roster. Newly-loaded tools are picked up by subsequently-built agent
// loops; turns already in flight keep the tool set they started with, which is
// what keeps a turn replayable (docs/sandboxed-tools.md §9.1).
func (d *Daemon) handleToolsReload(enc *json.Encoder) {
	if d.tools == nil {
		enc.Encode(protocol.NewErrorMsg("sandboxed tools are not enabled ([tools] enabled)")) //nolint:errcheck
		return
	}
	ReloadSandboxedTools(context.Background(), d.tools, d.mgr)
	data, _ := json.Marshal(d.sandboxedToolStatuses())
	enc.Encode(protocol.NewTextMsg(protocol.TypeToolsReload, string(data))) //nolint:errcheck
}

// sandboxedToolStatuses assembles the roster from the host, folding the loaded
// tools' descriptions in so `nine tools` reads as a catalog rather than a
// diagnostic. Returns an empty slice when the subsystem is off, which the CLI
// reports as "not enabled" rather than as an empty directory.
func (d *Daemon) sandboxedToolStatuses() []protocol.SandboxedToolStatus {
	out := []protocol.SandboxedToolStatus{}
	if d.tools == nil {
		return out
	}
	descriptions := map[string]string{}
	timeouts := map[string]string{}
	for _, t := range d.tools.Tools() {
		descriptions[t.Name] = t.Description
		if t.Timeout > 0 {
			timeouts[t.Name] = t.Timeout.String()
		}
	}
	// A generated tool's dependency lockfile lives in the store, not the host, so
	// join it in by name for the roster (R-TVM.14). Best-effort: a store read
	// failure drops the deps column, never the roster.
	depsByTool := map[string][]string{}
	if d.store != nil {
		if rows, err := d.store.GeneratedToolList(); err == nil {
			for _, r := range rows {
				depsByTool[r.Name] = lockfilePackages(r.Lockfile)
			}
		}
	}
	for _, st := range d.tools.Status() {
		out = append(out, protocol.SandboxedToolStatus{
			Name:         st.Name,
			Kind:         st.Kind,
			Loaded:       st.Loaded,
			Generated:    st.Generated,
			Capabilities: st.Capabilities,
			Description:  descriptions[st.Name],
			ManifestPath: st.ManifestPath,
			Error:        st.Err,
			Timeout:      timeouts[st.Name],
			Deps:         depsByTool[st.Name],
		})
	}
	return out
}

// lockfilePackages renders a stored lockfile as "name@version" entries for the
// roster. A tool with no external dependencies (the common case) yields nil.
func lockfilePackages(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var lf struct {
		Packages []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(raw, &lf); err != nil || len(lf.Packages) == 0 {
		return nil
	}
	out := make([]string, len(lf.Packages))
	for i, p := range lf.Packages {
		out[i] = p.Name + "@" + p.Version
	}
	return out
}

// handleSessionsList returns the session roster: every conversation with its
// age, journal size, and whether the retention reaper may take it.
//
// Attached is filled from the daemon's live map rather than the store, because
// "running right now" is not something the database knows.
func (d *Daemon) handleSessionsList(enc *json.Encoder) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("sessions require a configured store")) //nolint:errcheck
		return
	}
	sessions, err := d.store.SessionList()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}

	d.mu.RLock()
	live := make(map[string]bool, len(d.sessions))
	for id := range d.sessions {
		live[id] = true
	}
	names := make(map[string]string, len(d.names))
	for id, n := range d.names {
		names[id] = n
	}
	d.mu.RUnlock()

	out := make([]protocol.SessionInfo, 0, len(sessions))
	for _, s := range sessions {
		name := s.Name
		if name == "" {
			name = names[s.ID]
		}
		out = append(out, protocol.SessionInfo{
			ID:         s.ID,
			Name:       name,
			Status:     s.Status,
			AgeSeconds: s.AgeSeconds,
			Events:     s.Events,
			Protected:  s.Protected,
			Attached:   live[s.ID],
		})
	}
	payload, err := json.Marshal(out)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	enc.Encode(protocol.NewTextMsg(protocol.TypeSessionsList, string(payload))) //nolint:errcheck
}

// handleSessionDelete erases a session and reports what it removed.
//
// The reply names the counts rather than saying "deleted": this is the one
// operation in Nine that destroys history, and an operator who has just run it
// should be told what went, not reassured.
func (d *Daemon) handleSessionDelete(enc *json.Encoder, agentID string) {
	// Resolved before the delete, not after: resolveID prefix-matches against the
	// live session map, and by the time DeleteSession returns the entry is gone —
	// so resolving afterwards would echo back the operator's prefix instead of
	// the session it actually removed.
	resolved := d.resolveID(agentID)

	counts, err := d.DeleteSession(context.Background(), resolved)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	msg := fmt.Sprintf(
		"deleted %s — %d rows: %d journal events, %d notifications, %d tool-state keys, %d jobs",
		resolved, counts.Total(), counts.Events,
		counts.Notifications+counts.UserNotifications, counts.ToolState, counts.Jobs)
	enc.Encode(protocol.NewTextMsg(protocol.TypeSessionDelete, msg)) //nolint:errcheck
}

// handleStandingList returns the standing-run roster.
func (d *Daemon) handleStandingList(enc *json.Encoder) {
	if d.standing == nil {
		enc.Encode(protocol.NewTextMsg(protocol.TypeStandingList, "[]")) //nolint:errcheck
		return
	}
	status, err := d.standing.Status()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	out := make([]protocol.StandingInfo, 0, len(status))
	for _, s := range status {
		out = append(out, standingInfoOf(s))
	}
	payload, err := json.Marshal(out)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	enc.Encode(protocol.NewTextMsg(protocol.TypeStandingList, string(payload))) //nolint:errcheck
}

// handleStandingShow returns one standing run with its recent activity.
func (d *Daemon) handleStandingShow(enc *json.Encoder, id string, limit int) {
	if d.standing == nil {
		enc.Encode(protocol.NewErrorMsg("no standing tools are configured here")) //nolint:errcheck
		return
	}
	if limit <= 0 {
		limit = 20
	}
	s, found, err := d.standing.StatusOf(id, limit)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if !found {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("no standing tool %q", id))) //nolint:errcheck
		return
	}
	payload, err := json.Marshal(standingInfoOf(s))
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	enc.Encode(protocol.NewTextMsg(protocol.TypeStandingShow, string(payload))) //nolint:errcheck
}

// handleStandingControl stops or starts a standing run.
func (d *Daemon) handleStandingControl(enc *json.Encoder, id, action string) {
	if d.standing == nil {
		enc.Encode(protocol.NewErrorMsg("no standing tools are configured here")) //nolint:errcheck
		return
	}
	var state string
	switch action {
	case "stop":
		state = memory.StandingStopped
	case "start":
		state = memory.StandingRunning
	default:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("standing control action %q is not \"stop\" or \"start\"", action))) //nolint:errcheck
		return
	}
	ok, err := d.standing.SetState(id, state)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if !ok {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("no standing tool %q", id))) //nolint:errcheck
		return
	}
	verb := "stopped"
	if action == "start" {
		verb = "started; its first call is scheduled now"
	}
	enc.Encode(protocol.NewTextMsg(protocol.TypeStandingControl, fmt.Sprintf("%s %s", id, verb))) //nolint:errcheck
}

func standingInfoOf(s StandingStatus) protocol.StandingInfo {
	out := protocol.StandingInfo{
		ID: s.ID, Tool: s.Tool, State: s.State, Trigger: s.Trigger,
		Calls: s.Calls, Cycles: s.Cycles, Failures: s.Failures,
		LastError: s.LastError, LastCallAt: s.LastCallAt, NextAt: s.NextAt,
		Generated: s.Generated,
	}
	for _, e := range s.Recent {
		out.Recent = append(out.Recent, protocol.StandingLogLine{
			At: e.At.UTC().Format(time.RFC3339), Outcome: e.Outcome, Detail: e.Detail,
		})
	}
	return out
}

// handleToolCall runs one sandboxed tool once, for testing, and returns the
// whole envelope — including a `continue` a resumable tool produced, because
// that is exactly what is being debugged.
//
// Unless liveState is set, the call runs against a scratch state namespace that
// is discarded afterwards. The trap this avoids is real: a test call sharing a
// live standing run's store could overwrite its cursor or its bookkeeping, and
// the operator who "just tested it" would have silently corrupted the production
// run. Reach is NOT isolated — the tool's actual grants apply, because a test
// that cannot make the tool's real calls tests nothing.
func (d *Daemon) handleToolCall(enc *json.Encoder, tool string, args json.RawMessage, liveState bool) {
	if d.tools == nil {
		enc.Encode(protocol.NewErrorMsg("sandboxed tools are not enabled here")) //nolint:errcheck
		return
	}
	ctx := context.Background()
	scratch := ""
	if !liveState {
		// The sandbox override, not WithStateScope: a tool-scoped grant ignores
		// the conversation on the context — its namespace is shared by
		// construction — so only a forced override actually keeps a test call out
		// of a live standing run's store.
		scratch = "test:" + memory.NewID()
		ctx = toolvm.WithStateSandbox(ctx, scratch)
	}
	// A resumable tool is handed a first-call job context, because that is what
	// its first call actually looks like in production. Without it the tool sees
	// `undefined` where it expects {cursor, call} — so `nine tool call` would fail
	// on precisely the tools it exists to help debug.
	var out toolvm.Output
	var err error
	if t := d.tools.Get(tool); t != nil && t.Resumable {
		out, err = d.tools.CallJob(ctx, tool, args, toolvm.JobContext{Call: 1})
	} else {
		out, err = d.tools.CallOutput(ctx, tool, args)
	}
	// Scratch state is discarded whatever the outcome; a failed test call must
	// not leave debris either.
	if scratch != "" && d.store != nil {
		if t := d.tools.Get(tool); t != nil {
			d.dropScratchState(t.Name, scratch)
		}
	}
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	payload, err := json.Marshal(toolCallEnvelope(out))
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	enc.Encode(protocol.NewTextMsg(protocol.TypeToolCall, string(payload))) //nolint:errcheck
}

// dropScratchState reclaims a test call's scratch namespace. Best-effort — the
// TTL-less rows would otherwise sit until someone noticed, but nothing breaks if
// this fails beyond a few orphan rows an operator can see in the table.
func (d *Daemon) dropScratchState(tool, scratch string) {
	type scopeDropper interface {
		ToolStateDropScope(tool, scopeKey string) error
	}
	if sd, ok := d.store.(scopeDropper); ok {
		if err := sd.ToolStateDropScope(tool, scratch); err != nil {
			slog.Warn("tool call: drop scratch state", "tool", tool, "err", err)
		}
	}
}

// toolCallEnvelope renders a call's outcome in the guest's own vocabulary, so
// what the operator sees is what the tool actually returned.
func toolCallEnvelope(out toolvm.Output) map[string]any {
	env := map[string]any{"ok": true}
	switch {
	case out.Continue != nil:
		env["continue"] = map[string]any{
			"cursor":   out.Continue.Cursor,
			"progress": out.Continue.Progress,
			"after_ms": out.Continue.AfterMS,
		}
	case out.Bytes != nil:
		env["bytes"] = len(out.Bytes)
		env["media_type"] = out.MediaType
	default:
		env["output"] = out.Text
	}
	return env
}
