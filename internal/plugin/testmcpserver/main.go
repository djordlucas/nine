// testmcpserver is a minimal MCP server used by internal/plugin tests.
// It implements the MCP stdio transport with one tool: "mcp_echo".
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)

	for scanner.Scan() {
		var msg struct {
			JSONRPC string           `json:"jsonrpc"`
			ID      *json.RawMessage `json:"id"`
			Method  string           `json:"method"`
			Params  json.RawMessage  `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		// Skip notifications (no id).
		if msg.ID == nil {
			continue
		}

		var result any
		var rpcErrVal *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}

		switch msg.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "test-mcp-server", "version": "0.1.0"},
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
			rpcErrVal = &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -32601, Message: "method not found: " + msg.Method}
		}

		resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErrVal != nil {
			resp["error"] = rpcErrVal
		} else {
			resp["result"] = result
		}
		data, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stdout, "%s\n", data)
	}
}
