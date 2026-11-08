package plugin

import (
	"context"
	"encoding/json"
	"fmt"

	"nine/internal/llm"
)

// A plugin is an MCP server (adr/modular_nine.md, R-PLUG.1): the wire contract is
// MCP's — initialize → notifications/initialized → tools/list → tools/call — over
// stdio or streamable HTTP on the plugin's Unix socket. The legacy
// plugin.describe/plugin.call envelope was removed at P4; protocol versioning rides
// the MCP initialize negotiation instead of a hand-stamped integer.

// ToolDefinition describes a single tool that a plugin exposes.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	DisplayName string          `json:"display_name,omitempty"` // optional human-friendly label for display
}

// ToLLMDef converts a ToolDefinition to the llm.ToolDef representation used
// by the agent loop and context builder.
func (t ToolDefinition) ToLLMDef() llm.ToolDef {
	return llm.ToolDef{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, DisplayName: t.DisplayName}
}

// ToolHandler handles a single tool call. The context is the inbound call's
// context, cancelled if the daemon cancels the call (e.g. task_timeout).
type ToolHandler func(ctx context.Context, args json.RawMessage) (string, error)

// Schema is a convenience wrapper for inline JSON schema literals.
func Schema(s string) json.RawMessage { return json.RawMessage(s) }

// RPCErr is a JSON-RPC 2.0 error a handler can return to set a code; over MCP it
// becomes an in-band isError tool result, which the client maps back to a Go
// error either way.
type RPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCErr) Error() string { return e.Message }

// Errorf creates an internal error (-32603).
func Errorf(format string, a ...any) *RPCErr {
	return &RPCErr{Code: -32603, Message: fmt.Sprintf(format, a...)}
}

// InvalidArgs creates an invalid-params error (-32602).
func InvalidArgs(format string, a ...any) *RPCErr {
	return &RPCErr{Code: -32602, Message: fmt.Sprintf(format, a...)}
}

// DescribeResult is what a plugin advertises: its tools, after the MCP handshake.
// The daemon fills it from tools/list; plugin authors never construct it directly
// (mcp.Serve does).
type DescribeResult struct {
	Tools []ToolDefinition `json:"tools"`
	// MaxConcurrent records the transport's limit: 0 (unbounded) for
	// unix-socket HTTP, 1 for stdio, which is serial by construction (R-PLUG.8).
	MaxConcurrent int `json:"max_concurrent,omitempty"`

	// AsyncJobs is always false now: the envelope's job methods are gone with it,
	// and MCP carries jobs in a `_meta` extension the manager does not consume yet
	// (adr/modular_nine.md §2). Job methods on a plugin fail closed.
	AsyncJobs bool `json:"async_jobs,omitempty"`
}

// CallRequest is the params for a plugin tool call (the MCP tools/call under the
// hood).
type CallRequest struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args,omitempty"`
}

// CallResult is the result of a plugin tool call.
type CallResult struct {
	Output string `json:"output"`

	// JobID is unused while plugins do not advertise async_jobs (see
	// DescribeResult.AsyncJobs); it stays so a future _meta extension slots in
	// without a shape change.
	JobID string `json:"job_id,omitempty"`
}
