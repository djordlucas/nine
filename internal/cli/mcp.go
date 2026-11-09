package cli

import (
	"context"
	"encoding/json"
	"os/signal"
	"syscall"
	"time"

	"nine/internal/config"
	"nine/internal/mcp"
	"nine/internal/protocol"
)

// `nine mcp export` — the export direction of adr/modular_nine.md §6 (P6):
// Nine as an MCP server over stdio, so an external MCP client can use the
// daemon's tools. tools/list is the daemon's roster (resynced on a ticker, so
// a plugins reload is picked up), tools/call proxies plugin_call. Sampling and
// elicitation are refused by the server (the capability is never advertised).

// mcpExportClient adapts the wire client to mcp.ExportClient.
type mcpExportClient struct{ cl *protocol.Client }

func (e mcpExportClient) ListTools() ([]mcp.ExportTool, error) {
	tools, err := e.cl.ListTools()
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ExportTool, len(tools))
	for i, t := range tools {
		out[i] = mcp.ExportTool{Plugin: t.Plugin, Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return out, nil
}

func (e mcpExportClient) CallTool(tool string, args json.RawMessage, liveState bool) (string, error) {
	return e.cl.CallTool(tool, args, liveState)
}

// MCPExport serves the daemon's tool roster as an MCP stdio server.
func (c *CLI) MCPExport(cfg *config.Config) error {
	cl, err := c.connect(cfg)
	if err != nil {
		return err
	}
	defer cl.Close() //nolint:errcheck

	ec := mcpExportClient{cl}
	srv, err := mcp.ExportServer("nine", c.Version, ec)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Keep the exported roster in step with the daemon: a `nine plugins reload`
	// after the export started shows up on the next sync.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := mcp.SyncExportTools(srv, ec); err != nil {
					// A daemon restart mid-export leaves the last roster in
					// place; the next successful sync replaces it.
					continue
				}
			}
		}
	}()

	return mcp.ServeStdio(ctx, c.In, c.Out, srv)
}
