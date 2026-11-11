package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"nine/internal/llm"
)

// agent_stop (adr/orchestrator-board.md §2.4): force-stop a misbehaving
// worker's session mid-task. Operators could already end any session over the
// API; this puts the same stop in a model's hands, on the roles whose allowlist
// grants it (the orchestrator by default).

// AgentStopName is the force-stop tool's name.
const AgentStopName = "agent_stop"

// SessionControl stops a session by id. Implemented in internal/runtime over
// the daemon.
type SessionControl interface {
	Stop(callerID, id string) error
}

var agentStopDef = llm.ToolDef{
	Name:        AgentStopName,
	DisplayName: "Stop Agent",
	Description: "Force-stop a running agent session (a worker sub-agent stuck in a loop, for example) by its id. The turn in flight is cancelled and the session ends; its transcript and journal are kept. Refused for an unknown id.",
	InputSchema: json.RawMessage(`{"type":"object","required":["id"],"properties":{"id":{"type":"string","description":"The session id to stop, as shown in sub_agent events or the conversation list."}}}`),
}

// sessionToolDefs are the session control tools' definitions.
var sessionToolDefs = []llm.ToolDef{agentStopDef}

// RegisterSessionTools registers agent_stop into d when the role grants it.
func RegisterSessionTools(d *Dispatcher, sc SessionControl, callerID string, names []string) {
	defer func() {
		if !slices.Contains(names, AgentStopName) {
			delete(d.handlers, AgentStopName)
		}
	}()
	if !slices.Contains(names, AgentStopName) {
		return
	}
	d.handlers[AgentStopName] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("%s: %w", AgentStopName, err)
		}
		if strings.TrimSpace(req.ID) == "" {
			return "", fmt.Errorf("%s: id is required", AgentStopName)
		}
		if err := sc.Stop(callerID, req.ID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Stopped session %s.", req.ID), nil
	}
}
