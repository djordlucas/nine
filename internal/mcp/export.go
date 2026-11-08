package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// The export direction (adr/modular_nine.md §6, P6): Nine as an MCP server for
// external clients — `nine mcp export` over stdio, and the API server's /mcp
// endpoint — adapting the daemon's list_tools/plugin_call (R-PROTO.5). One
// adapter serves both: tools/list is answered from a registry synced from the
// daemon, tools/call proxies each call back to it. Sampling and elicitation
// are never advertised, so the SDK refuses them; the exported surface sits
// behind the API's authentication like every other API path.

// ExportTool is one daemon tool as the export proxy sees it — the shape of
// protocol.ToolSummary, kept local so this package does not depend on the
// wire package.
type ExportTool struct {
	Plugin      string          `json:"plugin"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ExportClient is the daemon surface the export proxies: the roster read and
// the direct tool call (R-PROTO.5).
type ExportClient interface {
	ListTools() ([]ExportTool, error)
	CallTool(tool string, args json.RawMessage, liveState bool) (string, error)
}

// ExportServer builds an MCP server whose tools are the daemon's current
// roster, each call proxied to cl.CallTool. Sampling and elicitation are not
// advertised, so they are refused.
func ExportServer(name, version string, cl ExportClient) (*mcpserver.MCPServer, error) {
	srv := mcpserver.NewMCPServer(name, version, mcpserver.WithToolCapabilities(false))
	if err := SyncExportTools(srv, cl); err != nil {
		return nil, err
	}
	return srv, nil
}

// SyncExportTools replaces the server's tool registry with the daemon's
// current roster. Call it again after a plugins reload to export the new set;
// until then the export serves the roster as of the last sync.
func SyncExportTools(srv *mcpserver.MCPServer, cl ExportClient) error {
	tools, err := cl.ListTools()
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	out := make([]mcpserver.ServerTool, 0, len(tools))
	for _, t := range tools {
		name := t.Name
		tool := mcptypes.NewToolWithRawSchema(t.Name, t.Description, t.InputSchema)
		out = append(out, mcpserver.ServerTool{Tool: tool, Handler: func(ctx context.Context, req mcptypes.CallToolRequest) (*mcptypes.CallToolResult, error) {
			raw, err := json.Marshal(req.GetArguments())
			if err != nil {
				return nil, fmt.Errorf("marshal arguments: %w", err)
			}
			res, err := cl.CallTool(name, raw, false)
			if err != nil {
				// In-band isError, the same failure shape the in-proc path gives
				// the model, so an exported call fails like a dispatched one.
				return mcptypes.NewToolResultErrorFromErr(err.Error(), err), nil
			}
			return mcptypes.NewToolResultText(res), nil
		}})
	}
	srv.SetTools(out...)
	return nil
}
