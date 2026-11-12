package runtime

import (
	"encoding/json"
	"fmt"

	"nine/internal/memory"
	"nine/internal/protocol"
)

// The operator's board CRUD over the wire (adr/orchestrator-board.md §2.6). The
// daemon owns the rules; every mutation is journaled (task_posted /
// task_updated / task_deleted) so the event stream and the UI see it.

func toWireTask(t memory.BoardTask) protocol.BoardTask {
	return protocol.BoardTask{
		ID: t.ID, Title: t.Title, Spec: t.Spec, PostedBy: t.PostedBy,
		ClaimedBy: t.ClaimedBy, Status: t.Status, Result: t.Result, Source: t.Source,
		Priority: t.Priority, Depth: t.Depth, CreatedAt: t.CreatedAt, ClaimedAt: t.ClaimedAt,
		FinishedAt: t.FinishedAt, UpdatedAt: t.UpdatedAt,
	}
}

func (d *Daemon) handleBoardList(enc *json.Encoder, status string) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("board is not enabled here")) //nolint:errcheck
		return
	}
	tasks, err := d.store.BoardTaskList(status, 200, 0)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	out := make([]protocol.BoardTask, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, toWireTask(t))
	}
	raw, _ := json.Marshal(out)
	enc.Encode(protocol.NewTextMsg(protocol.TypeBoardList, string(raw))) //nolint:errcheck
}

func (d *Daemon) handleBoardGet(enc *json.Encoder, id string) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("board is not enabled here")) //nolint:errcheck
		return
	}
	t, ok, err := d.store.BoardTaskGet(id)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("board task %q not found", id))) //nolint:errcheck
	default:
		raw, _ := json.Marshal(toWireTask(t))
		enc.Encode(protocol.NewTextMsg(protocol.TypeBoardGet, string(raw))) //nolint:errcheck
	}
}

func (d *Daemon) handleBoardCreate(enc *json.Encoder, title, spec string, priority int) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("board is not enabled here")) //nolint:errcheck
		return
	}
	t, err := d.store.BoardTaskCreate(title, spec, "operator", memory.BoardSourcePost, priority, 0)
	if err != nil {
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
		return
	}
	if d.sink != nil {
		raw, _ := json.Marshal(map[string]any{"id": t.ID, "title": t.Title, "posted_by": "operator"})
		d.sink.Append(memory.SessionEvent{AgentID: "operator", Type: EvTaskPosted, Payload: raw})
	}
	raw, _ := json.Marshal(toWireTask(t))
	enc.Encode(protocol.NewTextMsg(protocol.TypeBoardCreate, string(raw))) //nolint:errcheck
}

func (d *Daemon) handleBoardUpdate(enc *json.Encoder, id string, payload map[string]json.RawMessage) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("board is not enabled here")) //nolint:errcheck
		return
	}
	var p memory.BoardTaskPatch
	for k, v := range payload {
		switch k {
		case "title":
			var s string
			if json.Unmarshal(v, &s) == nil {
				p.Title = &s
			}
		case "spec":
			var s string
			if json.Unmarshal(v, &s) == nil {
				p.Spec = &s
			}
		case "status":
			var s string
			if json.Unmarshal(v, &s) == nil {
				p.Status = &s
			}
		case "result":
			var s string
			if json.Unmarshal(v, &s) == nil {
				p.Result = &s
			}
		case "priority":
			var n int
			if json.Unmarshal(v, &n) == nil {
				p.Priority = &n
			}
		case "clear_claim":
			var b bool
			if json.Unmarshal(v, &b) == nil && b {
				empty := ""
				p.Claimed = &empty
			}
		}
	}
	t, ok, err := d.store.BoardTaskUpdate(id, p)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("board task %q not found", id))) //nolint:errcheck
	default:
		if d.sink != nil {
			raw, _ := json.Marshal(map[string]any{"id": t.ID, "by": "operator"})
			d.sink.Append(memory.SessionEvent{AgentID: "operator", Type: EvTaskUpdated, Payload: raw})
		}
		raw, _ := json.Marshal(toWireTask(t))
		enc.Encode(protocol.NewTextMsg(protocol.TypeBoardUpdate, string(raw))) //nolint:errcheck
	}
}

func (d *Daemon) handleBoardDelete(enc *json.Encoder, id string) {
	if d.store == nil {
		enc.Encode(protocol.NewErrorMsg("board is not enabled here")) //nolint:errcheck
		return
	}
	ok, err := d.store.BoardTaskDelete(id)
	switch {
	case err != nil:
		enc.Encode(protocol.NewErrorMsg(err.Error())) //nolint:errcheck
	case !ok:
		enc.Encode(protocol.NewErrorMsg(fmt.Sprintf("board task %q not found", id))) //nolint:errcheck
	default:
		if d.sink != nil {
			raw, _ := json.Marshal(map[string]any{"id": id, "by": "operator"})
			d.sink.Append(memory.SessionEvent{AgentID: "operator", Type: EvTaskDeleted, Payload: raw})
		}
		enc.Encode(protocol.NewTextMsg(protocol.TypeBoardDelete, "deleted "+id)) //nolint:errcheck
	}
}
