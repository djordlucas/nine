// slowplugin is a test fixture for concurrency and crash-isolation tests. It
// exposes a "sleep" tool (fixed delay), a "crash" tool (exits without replying),
// a "traceid" tool, and a "slowjob" async job (a detached sleep, honouring
// cancellation and reporting progress). Its advertised max_concurrent is read
// from SLOW_MAX_CONCURRENT so a test can spawn it both unbounded (unset/0) and
// serial (1); the same cap bounds concurrent jobs.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"nine/internal/plugin"
)

const sleepDur = 150 * time.Millisecond

func main() {
	n, _ := strconv.Atoi(os.Getenv("SLOW_MAX_CONCURRENT"))

	plugin.Serve(
		[]plugin.ToolDefinition{
			{Name: "sleep", Description: "sleeps a fixed duration", InputSchema: plugin.Schema(`{"type":"object"}`)},
			{Name: "crash", Description: "exits the process without replying", InputSchema: plugin.Schema(`{"type":"object"}`)},
			{Name: "traceid", Description: "returns the trace ID carried on the call context", InputSchema: plugin.Schema(`{"type":"object"}`)},
			{Name: "slowjob", Description: "runs a detached sleep as a job", InputSchema: plugin.Schema(`{"type":"object"}`)},
		},
		map[string]plugin.ToolHandler{
			"sleep": func(context.Context, json.RawMessage) (string, error) {
				time.Sleep(sleepDur)
				return "ok", nil
			},
			"crash": func(context.Context, json.RawMessage) (string, error) {
				os.Exit(1)
				return "", nil
			},
			"traceid": func(ctx context.Context, _ json.RawMessage) (string, error) {
				return plugin.RequestIDFromContext(ctx), nil
			},
		},
		plugin.WithMaxConcurrent(n),
		plugin.WithJobHandlers(map[string]plugin.JobHandler{
			"slowjob": func(context.Context, json.RawMessage) (plugin.Job, error) {
				return plugin.Job{
					Ack: "started slow job",
					Run: func(ctx context.Context) (string, error) {
						plugin.SetProgress(ctx, "sleeping")
						select {
						case <-time.After(sleepDur):
							return "job done", nil
						case <-ctx.Done():
							return "", ctx.Err()
						}
					},
				}, nil
			},
		}),
	)
}
