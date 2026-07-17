package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/llm"
)

// AskHumanDef is the ask_human tool definition. It is registered only for
// interactive sessions (see internal/runtime/builder.go), so it is kept out of
// InterceptedDefs and appended to the tool list conditionally.
var AskHumanDef = llm.ToolDef{
	Name:        "ask_human",
	DisplayName: "Ask Human",
	Description: "Pause and request input from the human operator before proceeding.",
	InputSchema: json.RawMessage(`{"type":"object","required":["question"],"properties":{"question":{"type":"string"},"options":{"type":"array","items":{"type":"string"},"description":"Optional multiple-choice options to present"}}}`),
}

// AskHumanFn blocks until the human answers, the turn is cancelled, or the
// request times out. It returns the answer text, or an error the model can
// reason about (e.g. a timeout).
type AskHumanFn func(ctx context.Context, agentID, question string, options []string) (string, error)

// RegisterAskHuman registers the ask_human tool for agentID, delegating the
// blocking wait to fn.
func RegisterAskHuman(d *Dispatcher, agentID string, fn AskHumanFn) {
	d.handlers["ask_human"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("ask_human: %w", err)
		}
		if strings.TrimSpace(req.Question) == "" {
			return "", fmt.Errorf("ask_human: question is required")
		}
		return fn(ctx, agentID, req.Question, req.Options)
	}
}
