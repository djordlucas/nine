package main

import (
	"context"
	"encoding/json"
	"time"

	"nine/internal/plugin"
)

func main() {
	plugin.Serve(
		[]plugin.ToolDefinition{{
			Name:        "time",
			DisplayName: "Get Time",
			Description: "Return the current date and time, including the local timezone.",
			InputSchema: plugin.Schema(`{"type":"object","properties":{}}`),
		}},
		map[string]plugin.ToolHandler{"time": now},
	)
}

func now(_ context.Context, _ json.RawMessage) (string, error) {
	t := time.Now()
	out, _ := json.Marshal(map[string]string{
		"iso8601":  t.Format(time.RFC3339),
		"human":    t.Format("Monday, January 2, 2006 3:04:05 PM MST"),
		"timezone": t.Location().String(),
	})
	return string(out), nil
}
