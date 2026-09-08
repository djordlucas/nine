package plugin

import (
	"encoding/json"

	"nine/internal/llm"
)

// ProtocolVersion is the version of the native plugin wire contract: the
// plugin.describe / plugin.call envelope over HTTP on a Unix socket, its
// methods, and their semantics. The daemon and a plugin must agree on it.
// Bump this on any breaking change to that contract; the daemon rejects a
// plugin built against an incompatible version at startup (see
// Manager.Start / checkProtocolVersion). It is independent of the release
// version — most releases will not touch it. See docs/versioning.md.
//
// v2 adds long-running jobs (docs/plugin-capabilities.md §5): the async_jobs
// describe flag, a job_id on plugin.call, and the plugin.job_status /
// plugin.job_cancel methods. It is additive — a v1 plugin remains fully
// functional except that it cannot start jobs — so the daemon supports both
// (checkProtocolVersion).
const ProtocolVersion = 2

// ToolDefinition describes a single tool that a plugin exposes.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	DisplayName string          `json:"display_name,omitempty"` // optional human-friendly label for TUI display
}

// ToLLMDef converts a ToolDefinition to the llm.ToolDef representation used
// by the agent loop and context builder.
func (t ToolDefinition) ToLLMDef() llm.ToolDef {
	return llm.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, DisplayName: t.DisplayName}
}

// DescribeResult is the result of plugin.describe.
type DescribeResult struct {
	// ProtocolVersion is the wire-contract version the plugin was built against
	// (see ProtocolVersion). plugin.Serve stamps it automatically; the daemon
	// rejects a plugin whose version it does not support. Absent (0) means the
	// plugin predates protocol versioning.
	ProtocolVersion int `json:"protocol_version,omitempty"`

	Tools []ToolDefinition `json:"tools"`
	// MaxConcurrent caps how many calls the daemon will have in flight against
	// this plugin at once, mapped onto Transport.MaxConnsPerHost. The zero value
	// (0) means unbounded — the right default for stateless handlers. A plugin
	// with shared mutable state must advertise a finite cap (browser uses 1).
	MaxConcurrent int `json:"max_concurrent,omitempty"`

	// AsyncJobs reports that the plugin can run detached work and answer
	// plugin.job_status / plugin.job_cancel (docs/plugin-capabilities.md §5).
	// plugin.Serve sets it when job handlers are registered. The daemon rejects a
	// job_id from a plugin that did not advertise it — fail-closed against skew.
	AsyncJobs bool `json:"async_jobs,omitempty"`
}

// CallRequest is the params for plugin.call.
type CallRequest struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args,omitempty"`
}

// CallResult is the result of plugin.call.
type CallResult struct {
	Output string `json:"output"`

	// JobID is set when the call started detached work instead of producing a
	// result (docs/plugin-capabilities.md §5): Output then carries a one-line
	// acknowledgement and the daemon polls plugin.job_status for the outcome.
	// Empty for an ordinary synchronous call.
	JobID string `json:"job_id,omitempty"`
}

// rpcRequest is a JSON-RPC 2.0 request.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// rpcError is the error object in a JSON-RPC 2.0 response.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string {
	return e.Message
}
