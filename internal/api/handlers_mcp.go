package api

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"

	"nine/internal/mcp"
)

// The MCP export endpoint (adr/modular_nine.md §6, P6): /api/v1/mcp serves the
// daemon's tool roster as MCP streamable HTTP — tools/list synced from the
// daemon, tools/call proxied to plugin_call. OpenAPI cannot describe the MCP
// protocol, so like the WebSocket route this one is mounted by hand, outside
// the document; it sits under /api/ so the auth middleware chain covers it
// exactly like every other API path.

const mcpPath = "/api/v1/mcp"

// apiExportClient dials the daemon per operation, the way every handler here
// does, and adapts the wire client to mcp.ExportClient.
type apiExportClient struct{ s *Server }

func (c apiExportClient) ListTools() ([]mcp.ExportTool, error) {
	cl, err := c.s.getDaemonClient()
	if err != nil {
		return nil, err
	}
	defer cl.Close() //nolint:errcheck
	tools, err := cl.ListTools()
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ExportTool, len(tools))
	for i, t := range tools {
		out[i] = mcp.ExportTool{Plugin: t.Plugin, Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
	}
	return out, nil
}

func (c apiExportClient) CallTool(tool string, args json.RawMessage, liveState bool) (string, error) {
	cl, err := c.s.getDaemonClient()
	if err != nil {
		return "", err
	}
	defer cl.Close() //nolint:errcheck
	return cl.CallTool(tool, args, liveState)
}

// mcpHandler returns the streamable-HTTP handler for the export server. The
// underlying MCP server is built once, without touching the daemon: the roster
// is synced on the first request and then on a ticker, so neither route
// registration nor an API boot with the daemon down makes a socket call, and a
// failed sync leaves the last roster in place.
func (s *Server) mcpHandler() http.Handler {
	s.mcpOnce.Do(func() {
		srv := mcpserver.NewMCPServer("nine", s.version, mcpserver.WithToolCapabilities(false))
		s.mcpSrv = srv
		go func() {
			t := time.NewTicker(10 * time.Second)
			defer t.Stop()
			for range t.C {
				_ = mcp.SyncExportTools(srv, apiExportClient{s})
			}
		}()
	})
	sh := mcpserver.NewStreamableHTTPServer(s.mcpSrv, mcpserver.WithStateLess(true))
	var synced sync.Once
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		synced.Do(func() { _ = mcp.SyncExportTools(s.mcpSrv, apiExportClient{s}) })
		sh.ServeHTTP(w, r)
	})
}
