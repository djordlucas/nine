package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"nine/internal/llm"
)

// notifyToolDefs is the notify_user tool: the human-output surface for a
// background agent that has no interactive conversation to speak into
// (docs/predefined-agents.md §5 piece 2).
var notifyToolDefs = []llm.ToolDef{
	{
		Name:        "notify_user",
		DisplayName: "Notify User",
		Description: "Post a concise finding or alert to the human's notification feed. You are a background agent with no live conversation, so this is how a human learns what you found. Use it only when something genuinely warrants human attention; keep it short and specific. Readable with `nine notifications`.",
		InputSchema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string","description":"The message to show the human. One or two sentences."}}}`),
	},
}

// RegisterNotifyUser registers the notify_user handler into d. agentID
// attributes the notification to its posting agent; add appends it to the
// human-facing feed.
func RegisterNotifyUser(d *Dispatcher, agentID string, add func(agentID, text string)) {
	d.handlers["notify_user"] = func(_ context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("notify_user: %w", err)
		}
		if strings.TrimSpace(req.Text) == "" {
			return "", fmt.Errorf("notify_user: text is required")
		}
		add(agentID, req.Text)
		return "notification posted to the user's feed", nil
	}
}
