package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"nine/internal/memory"
	"nine/internal/protocol"
)

// Stored agent definitions over the wire (adr/agents-concept.md A5, phase 1):
// the agent_* verbs compose, read, change and remove an agent definition in the
// daemon's store, and agent_run starts a conversation from one. Per-agent
// scoping of the definition's parts lands in phase 2 (A3); phase 3 (A4) moves
// the rows into per-agent databases.

// toWireAgent converts a store row to its wire form; the JSON-text columns
// become the definition's raw fields.
func toWireAgent(a memory.Agent) protocol.Agent {
	def := protocol.AgentDefinition{Name: a.Name, Role: a.Role}
	if a.Skills != "" {
		_ = json.Unmarshal([]byte(a.Skills), &def.Skills)
	}
	if a.SelfModel != "" {
		def.SelfModel = json.RawMessage(a.SelfModel)
	}
	if a.Reflection != "" {
		def.Reflection = json.RawMessage(a.Reflection)
	}
	return protocol.Agent{
		ID: a.ID, Version: a.Version, Status: a.Status, Definition: def,
		CreatedAt: a.CreatedAt, UpdatedAt: a.UpdatedAt,
	}
}

func agentStoreErr(enc *json.Encoder) {
	enc.Encode(protocol.NewErrorMsg("agent store is not enabled here")) //nolint:errcheck
}

func (d *Daemon) handleAgentList(enc *json.Encoder) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	agents, err := d.store.AgentList()
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	out := make([]protocol.Agent, 0, len(agents))
	for _, a := range agents {
		out = append(out, toWireAgent(a))
	}
	raw, _ := json.Marshal(out)
	enc.Encode(protocol.NewTextMsg(protocol.TypeAgentList, string(raw))) //nolint:errcheck
}

func (d *Daemon) handleAgentGet(enc *json.Encoder, id string) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	a, ok, err := d.store.AgentGet(id)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent %q not found", id))) //nolint:errcheck
	default:
		raw, _ := json.Marshal(toWireAgent(a))
		enc.Encode(protocol.NewTextMsg(protocol.TypeAgentGet, string(raw))) //nolint:errcheck
	}
}

func (d *Daemon) handleAgentCreate(enc *json.Encoder, payload json.RawMessage) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	var def protocol.AgentDefinition
	if err := json.Unmarshal(payload, &def); err != nil {
		enc.Encode(protocol.NewErrorMsg("agent_create: invalid definition JSON: " + err.Error())) //nolint:errcheck
		return
	}
	skills, _ := json.Marshal(def.Skills)
	selfModel, _ := json.Marshal(def.SelfModel)
	reflection, _ := json.Marshal(def.Reflection)
	a, err := d.store.AgentCreate(def.Name, def.Role, string(skills), string(selfModel), string(reflection))
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	raw, _ := json.Marshal(toWireAgent(a))
	enc.Encode(protocol.NewTextMsg(protocol.TypeAgentCreate, string(raw))) //nolint:errcheck
}

func (d *Daemon) handleAgentUpdate(enc *json.Encoder, id string, payload json.RawMessage) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		enc.Encode(protocol.NewErrorMsg("agent_update: invalid patch JSON: " + err.Error())) //nolint:errcheck
		return
	}
	var p memory.AgentPatch
	for k, v := range fields {
		switch k {
		case "name", "role":
			var s string
			if json.Unmarshal(v, &s) == nil {
				if k == "name" {
					p.Name = &s
				} else {
					p.Role = &s
				}
			}
		case "skills", "self_model", "reflection":
			s := string(v)
			switch k {
			case "skills":
				p.Skills = &s
			case "self_model":
				p.SelfModel = &s
			case "reflection":
				p.Reflection = &s
			}
		}
	}
	a, ok, err := d.store.AgentUpdate(id, p)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent %q not found", id))) //nolint:errcheck
	default:
		raw, _ := json.Marshal(toWireAgent(a))
		enc.Encode(protocol.NewTextMsg(protocol.TypeAgentUpdate, string(raw))) //nolint:errcheck
	}
}

func (d *Daemon) handleAgentDelete(enc *json.Encoder, id string) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	ok, err := d.store.AgentDelete(id)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent %q not found", id))) //nolint:errcheck
	default:
		enc.Encode(protocol.NewTextMsg(protocol.TypeAgentDelete, "deleted "+id)) //nolint:errcheck
	}
}

// handleAgentRun starts a conversation from the stored agent's definition: the
// agent's role names the conversation's role (Q12 — running an agent is a
// conversation created from its bundle, no new session kind). A staged (imported,
// unapproved) agent is refused (Q30).
func (d *Daemon) handleAgentRun(enc *json.Encoder, id string, interactive bool) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	a, ok, err := d.store.AgentGet(id)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent %q not found", id))) //nolint:errcheck
		return
	case a.Status == memory.AgentStaged:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent %q is staged (imported); approve it with agent_approve first", id))) //nolint:errcheck
		return
	}
	cid, err := d.newConversationFromAgent(interactive, a.Role, id)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	role := a.Role
	d.mu.RLock()
	if w := d.sessions[cid]; w != nil {
		role = w.role()
	}
	d.mu.RUnlock()
	raw, _ := json.Marshal(protocol.AgentRunResult{AgentID: id, ConversationID: cid, Role: role})
	enc.Encode(protocol.NewTextMsg(protocol.TypeAgentRun, string(raw))) //nolint:errcheck
}

// handleAgentExport renders the agent as a bundle tar (manifest only in phase 2;
// the agent's database joins the bundle in phase 3, Q6).
func (d *Daemon) handleAgentExport(enc *json.Encoder, id string) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	a, ok, err := d.store.AgentGet(id)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent %q not found", id))) //nolint:errcheck
		return
	}
	// Phase 3+ bundles carry the agent's db too (Q6): checkpoint its WAL and
	// read the file, so the bundle is self-contained.
	var dbBytes []byte
	if mgrs, ok := d.store.(interface{ AgentStores() *memory.AgentStores }); ok {
		if st, err := mgrs.AgentStores().AgentStore(id); err == nil {
			if err := st.CheckpointWAL(); err == nil {
				dbBytes, _ = os.ReadFile(mgrs.AgentStores().AgentDBPath(id))
			}
		}
	}
	tarBytes, err := memory.AgentBundleTarWithDB(a, dbBytes)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	raw, _ := json.Marshal(protocol.AgentExportResult{
		Filename:  a.Name + ".agent.tar",
		TarBase64: base64.StdEncoding.EncodeToString(tarBytes),
	})
	enc.Encode(protocol.NewTextMsg(protocol.TypeAgentExport, string(raw))) //nolint:errcheck
}

// handleAgentImport imports a bundle tar: the manifest is validated and the
// agent is created staged (Q30) — it cannot run until agent_approve. The import
// is journaled so the audit trail shows what entered the daemon.
func (d *Daemon) handleAgentImport(enc *json.Encoder, payload json.RawMessage) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	var req protocol.AgentImportRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		enc.Encode(protocol.NewErrorMsg("agent_import: invalid payload JSON: " + err.Error())) //nolint:errcheck
		return
	}
	tarBytes, err := base64.StdEncoding.DecodeString(req.TarBase64)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg("agent_import: tar_base64 is not base64: " + err.Error())) //nolint:errcheck
		return
	}
	manifest, err := memory.ParseAgentBundleTar(tarBytes)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if manifest.Role != "" && !d.knownRole(manifest.Role) {
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent bundle: unknown role %q (roles_list shows the registry)", manifest.Role))) //nolint:errcheck
		return
	}
	skills := string(manifest.Skills)
	if skills == "" {
		skills = "[]"
	}
	a, err := d.store.AgentCreateWithStatus(manifest.Name, manifest.Role, skills,
		string(manifest.SelfModel), string(manifest.Reflection), memory.AgentStaged)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if d.sink != nil {
		raw, _ := json.Marshal(map[string]any{"id": a.ID, "name": a.Name, "role": a.Role, "source_agent": manifest.SourceAgentID})
		d.sink.Append(memory.SessionEvent{AgentID: "operator", Type: "agent_imported", Payload: raw})
	}
	raw, _ := json.Marshal(toWireAgent(a))
	enc.Encode(protocol.NewTextMsg(protocol.TypeAgentImport, string(raw))) //nolint:errcheck
}

// handleAgentApprove moves a staged (imported) agent to active (Q30) and journals it.
func (d *Daemon) handleAgentApprove(enc *json.Encoder, id string) {
	if d.store == nil {
		agentStoreErr(enc)
		return
	}
	a, ok, err := d.store.AgentApprove(id)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("agent %q not found", id))) //nolint:errcheck
	default:
		if d.sink != nil {
			raw, _ := json.Marshal(map[string]any{"id": a.ID, "name": a.Name})
			d.sink.Append(memory.SessionEvent{AgentID: "operator", Type: "agent_approved", Payload: raw})
		}
		raw, _ := json.Marshal(toWireAgent(a))
		enc.Encode(protocol.NewTextMsg(protocol.TypeAgentApprove, string(raw))) //nolint:errcheck
	}
}
