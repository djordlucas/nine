// testmcpserve is a minimal MCP server used by internal/plugin tests. It speaks
// the MCP streamable-HTTP transport (stateless) on NINE_PLUGIN_SOCKET at /mcp,
// with one tool: "mcp_echo", which echoes its arguments back as text. It exists
// so the manager's MCP-first handshake (R-PLUG.16, adr/modular_nine.md) can be
// tested without building the nine binary.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
)

func main() {
	socketPath := os.Getenv("NINE_PLUGIN_SOCKET")
	if socketPath == "" {
		fmt.Fprintln(os.Stderr, "testmcpserve: NINE_PLUGIN_SOCKET not set")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", handle)

	os.Remove(socketPath) //nolint:errcheck // clear a stale socket
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testmcpserve: listen: %v\n", err)
		os.Exit(1)
	}
	if err := http.Serve(ln, mux); err != nil { //nolint:gosec // G114: local Unix-socket test server
		fmt.Fprintf(os.Stderr, "testmcpserve: serve: %v\n", err)
	}
}

func handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var msg struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      *json.RawMessage `json:"id"`
		Method  string           `json:"method"`
		Params  json.RawMessage  `json:"params"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// A notification (no id) is acknowledged with 202 and no body.
	if msg.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var result any
	var rpcErr *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}

	switch msg.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "testmcpserve", "version": "0.1.0"},
		}
	case "tools/list":
		result = map[string]any{
			"tools": []map[string]any{
				{
					"name":        "mcp_echo",
					"description": "Echoes its arguments back as text",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"message": map[string]any{"type": "string"},
						},
					},
				},
			},
		}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(msg.Params, &params) //nolint:errcheck
		result = map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": string(params.Arguments)},
			},
			"isError": false,
		}
	default:
		rpcErr = &struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}{Code: -32601, Message: "method not found: " + msg.Method}
	}

	resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp) //nolint:errcheck
}
