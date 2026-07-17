// testplugin is a minimal plugin used by internal/plugin tests. It implements
// the wire contract by hand (no pluginutil) over the HTTP/Unix-socket transport:
// plugin.describe (one tool: "echo") and plugin.call (echoes args as output).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
)

func main() {
	socketPath := os.Getenv("NINE_PLUGIN_SOCKET")
	if socketPath == "" {
		fmt.Fprintln(os.Stderr, "testplugin: NINE_PLUGIN_SOCKET not set")
		os.Exit(1)
	}

	// Advertised protocol version. Defaults to the current contract (1); tests
	// override it via NINE_TEST_PROTOCOL_VERSION to exercise the daemon's
	// version-mismatch rejection.
	protocolVersion := 1
	if v := os.Getenv("NINE_TEST_PROTOCOL_VERSION"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "testplugin: bad NINE_TEST_PROTOCOL_VERSION %q: %v\n", v, err)
			os.Exit(1)
		}
		protocolVersion = n
	}
	os.Remove(socketPath) //nolint:errcheck
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testplugin: listen: %v\n", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/rpc", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		json.Unmarshal(body, &req) //nolint:errcheck

		var resp map[string]any
		switch req.Method {
		case "plugin.describe":
			resp = map[string]any{"result": map[string]any{
				"protocol_version": protocolVersion,
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echoes its arguments back as output",
					"input_schema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"message": map[string]any{"type": "string"}},
					},
				}},
			}}
		case "plugin.call":
			var p struct {
				Tool string          `json:"tool"`
				Args json.RawMessage `json:"args"`
			}
			json.Unmarshal(req.Params, &p) //nolint:errcheck
			resp = map[string]any{"result": map[string]any{"output": string(p.Args)}}
		default:
			resp = map[string]any{"error": map[string]any{"code": -32601, "message": "method not found"}}
		}
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})

	http.Serve(ln, mux) //nolint:errcheck
}
