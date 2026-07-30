package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"nine/internal/llm"
)

// JobToolNames are the model-facing background-job tools
// (docs/plugin-capabilities.md §5). Granted whenever the job registry is wired,
// regardless of the role allowlist — like tool_list — so an agent that started a
// job can always follow up on it.
var JobToolNames = []string{"job_wait", "job_check", "job_list", "job_cancel"}

// defaultJobWaitSeconds is job_wait's timeout when the model names none, and the
// clamp bounds keep a model from blocking a turn open indefinitely or asking for
// a nonsensical wait. The turn's own budget (ctx) caps it further.
const (
	defaultJobWaitSeconds = 60
	maxJobWaitSeconds     = 600
)

var jobToolDefs = []llm.ToolDef{
	{
		Name:        "job_wait",
		DisplayName: "Wait for Job",
		Description: "Wait for a background job (one a long-running tool started) to finish, then return its result. If it has not finished in time, this returns its current progress instead — that is NOT an error: you can simply move on and check again later. Use the handle from when the job started, or from job_list.",
		InputSchema: json.RawMessage(`{"type":"object","required":["handle"],"properties":{"handle":{"type":"string","description":"The job handle, e.g. job_7f3a."},"timeout_seconds":{"type":"integer","description":"How long to wait before returning. Default 60."}}}`),
	},
	{
		Name:        "job_check",
		DisplayName: "Check Job",
		Description: "Return a background job's current state and progress — and its result if it has finished — without waiting. Use it to poll a job you are not blocking on.",
		InputSchema: json.RawMessage(`{"type":"object","required":["handle"],"properties":{"handle":{"type":"string","description":"The job handle, e.g. job_7f3a."}}}`),
	},
	{
		Name:        "job_list",
		DisplayName: "List Jobs",
		Description: "List your outstanding background jobs — handle, tool, age, and progress. Use it to see what is still running.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
	},
	{
		Name:        "job_cancel",
		DisplayName: "Cancel Job",
		Description: "Request cancellation of a background job. Best-effort; the job stops as soon as the plugin notices.",
		InputSchema: json.RawMessage(`{"type":"object","required":["handle"],"properties":{"handle":{"type":"string","description":"The job handle, e.g. job_7f3a."}}}`),
	},
}

// JobTools backs the model-facing job tools. It is implemented in the runtime,
// which owns the registry store and the plugin manager; every method returns the
// exact string the model sees. Wait never returns an error for a timeout — a
// still-running job is a normal answer, not a failure.
type JobTools interface {
	Wait(ctx context.Context, handle string, timeout time.Duration) (string, error)
	Check(ctx context.Context, handle string) (string, error)
	List(ctx context.Context) (string, error)
	Cancel(ctx context.Context, handle string) (string, error)
}

// RegisterJobTools registers the four job tools into d, dispatching to ops.
func RegisterJobTools(d *Dispatcher, ops JobTools) {
	d.handlers["job_wait"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		var req struct {
			Handle         string `json:"handle"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(args, &req); err != nil {
			return "", fmt.Errorf("job_wait: %w", err)
		}
		if req.Handle == "" {
			return "", fmt.Errorf("job_wait: handle is required")
		}
		secs := req.TimeoutSeconds
		if secs <= 0 {
			secs = defaultJobWaitSeconds
		}
		secs = min(secs, maxJobWaitSeconds)
		return ops.Wait(ctx, req.Handle, time.Duration(secs)*time.Second)
	}

	d.handlers["job_check"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		handle, err := jobHandleArg(args, "job_check")
		if err != nil {
			return "", err
		}
		return ops.Check(ctx, handle)
	}

	d.handlers["job_list"] = func(ctx context.Context, _ json.RawMessage) (string, error) {
		return ops.List(ctx)
	}

	d.handlers["job_cancel"] = func(ctx context.Context, args json.RawMessage) (string, error) {
		handle, err := jobHandleArg(args, "job_cancel")
		if err != nil {
			return "", err
		}
		return ops.Cancel(ctx, handle)
	}
}

func jobHandleArg(args json.RawMessage, tool string) (string, error) {
	var req struct {
		Handle string `json:"handle"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("%s: %w", tool, err)
	}
	if req.Handle == "" {
		return "", fmt.Errorf("%s: handle is required", tool)
	}
	return req.Handle, nil
}
