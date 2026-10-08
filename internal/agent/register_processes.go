package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"nine/internal/llm"
)

// ProcessControl is how a conversation works with processes
// (adr/process-sessions.md §9). A live process is not request and response, so
// it is never a callable tool; these five tools are the asynchronous surface
// instead. A turn never waits on a process: the model sends, and the answer
// comes back through what the process writes.
type ProcessControl interface {
	// ProcessList returns every process's state, trigger and budget use.
	ProcessList() (any, error)
	// ProcessShow returns one process with its recent activity, or an error
	// naming an unknown id.
	ProcessShow(id string) (any, error)
	// ProcessSend gives the process a message as its next trigger, from
	// agentID. It is refused when the process cannot take it now.
	ProcessSend(id, text, agentID string) error
	// ProcessStart and ProcessStop act as a model, under the start rule.
	ProcessStart(id string) error
	ProcessStop(id string) error
}

// ProcessToolNames are the process tools, in the order they are advertised.
var ProcessToolNames = []string{"process_list", "process_show", "process_send", "process_start", "process_stop"}

const processIDSchema = `{"type":"object","required":["id"],"properties":{"id":{"type":"string","description":"The process id, as process_list shows it."}}}`

var processToolDefs = []llm.ToolDef{
	{
		Name:        "process_list",
		DisplayName: "List Processes",
		Description: "List the processes running in the background — goal sessions, standing agents, reflection, watchers, standing tools — with each one's state, trigger and budget use. Use it to see what runs between conversations.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "process_show",
		DisplayName: "Show Process",
		Description: "Show one process in detail: its state and who stopped it, its trigger, session, goal and pipe, its budget use and when that resets, its last error, and its recent activity.",
		InputSchema: json.RawMessage(processIDSchema),
	},
	{
		Name:        "process_send",
		DisplayName: "Send to Process",
		Description: "Send a message to a running live process; it receives it as its next trigger. It does not wait for an answer: the process answers through what it writes — files, memory, the notification feed. Refused when the process is stopped, busy, or a slice process with no session.",
		InputSchema: json.RawMessage(`{"type":"object","required":["id","text"],"properties":{"id":{"type":"string","description":"The process id, as process_list shows it."},"text":{"type":"string","description":"The message."}}}`),
	},
	{
		Name:        "process_start",
		DisplayName: "Start Process",
		Description: "Start a stopped process. It runs under its own grants, role and budget, so starting it grants nothing new. Refused for a process the operator stopped, one its goal stopped (reactivate the goal instead), one paused by its budget until the budget resets, and at the running-process cap.",
		InputSchema: json.RawMessage(processIDSchema),
	},
	{
		Name:        "process_stop",
		DisplayName: "Stop Process",
		Description: "Stop a process. A model may start it again later; one the operator stops only the operator can.",
		InputSchema: json.RawMessage(processIDSchema),
	},
}

// ProcessToolDefs returns the process tools' definitions.
func ProcessToolDefs() []llm.ToolDef { return processToolDefs }

// RegisterProcessTools registers the process tools named in names into d for
// agentID's loop; a tool the role does not grant is not registered at all.
func RegisterProcessTools(d *Dispatcher, pc ProcessControl, agentID string, names []string) {
	defer func() {
		for _, name := range ProcessToolNames {
			if !slices.Contains(names, name) {
				delete(d.handlers, name)
			}
		}
	}()
	d.handlers["process_list"] = func(_ context.Context, _ json.RawMessage) (string, error) {
		return processJSON(pc.ProcessList())
	}
	d.handlers["process_show"] = func(_ context.Context, args json.RawMessage) (string, error) {
		id, err := processID("process_show", args)
		if err != nil {
			return "", err
		}
		return processJSON(pc.ProcessShow(id))
	}
	d.handlers["process_send"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("process_send: %w", err)
		}
		if strings.TrimSpace(req.ID) == "" || strings.TrimSpace(req.Text) == "" {
			return "", fmt.Errorf("process_send: id and text are required")
		}
		if err := pc.ProcessSend(req.ID, req.Text, agentID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Sent to process %s; it takes the message as its next trigger.", req.ID), nil
	}
	d.handlers["process_start"] = func(_ context.Context, args json.RawMessage) (string, error) {
		id, err := processID("process_start", args)
		if err != nil {
			return "", err
		}
		if err := pc.ProcessStart(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("Started process %s.", id), nil
	}
	d.handlers["process_stop"] = func(_ context.Context, args json.RawMessage) (string, error) {
		id, err := processID("process_stop", args)
		if err != nil {
			return "", err
		}
		if err := pc.ProcessStop(id); err != nil {
			return "", err
		}
		return fmt.Sprintf("Stopped process %s.", id), nil
	}
}

func processID(tool string, args json.RawMessage) (string, error) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	if strings.TrimSpace(req.ID) == "" {
		return "", fmt.Errorf("%s: id is required", tool)
	}
	return req.ID, nil
}

func processJSON(v any, err error) (string, error) {
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
