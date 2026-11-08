// testplugin is a minimal MCP server used by internal/plugin tests. It speaks
// MCP on whichever transport the manager hands it: streamable HTTP on
// NINE_PLUGIN_SOCKET when set (the default), stdio otherwise. Tools: "echo"
// (echoes its arguments back as text), "sleep" (fixed 150ms delay, for
// concurrency and cancellation tests), "crash" (exits without replying), and
// "traceid" (returns the X-Nine-Request-ID header, socket transport only).
//
// Knobs: NINE_TEST_ENV_DUMP writes the process environment to that path before
// serving; NINE_TEST_STARTUP_DELAY sleeps before serving. It is hand-rolled
// rather than built on the mcp-go server so the client and the fixture cannot
// share a server-side bug.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const sleepDur = 150 * time.Millisecond

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params"`
	Header  http.Header      `json:"-"` // socket transport only
}

func main() {
	if dump := os.Getenv("NINE_TEST_ENV_DUMP"); dump != "" {
		os.WriteFile(dump, []byte(strings.Join(os.Environ(), "\n")), 0o600) //nolint:errcheck
	}
	if d := os.Getenv("NINE_TEST_STARTUP_DELAY"); d != "" {
		if wait, err := time.ParseDuration(d); err == nil {
			time.Sleep(wait)
		}
	}

	if sock := os.Getenv("NINE_PLUGIN_SOCKET"); sock != "" {
		serveSocket(sock)
		return
	}
	serveStdio()
}

var tools = []map[string]any{
	{"name": "echo", "description": "Echoes its arguments back as text",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"message": map[string]any{"type": "string"}}}},
	{"name": "sleep", "description": "sleeps a fixed duration", "inputSchema": map[string]any{"type": "object"}},
	{"name": "crash", "description": "exits the process without replying", "inputSchema": map[string]any{"type": "object"}},
	{"name": "traceid", "description": "returns the request ID header", "inputSchema": map[string]any{"type": "object"}},
}

// rpcError is a JSON-RPC error object. A named pointer type so a nil check in
// reply is not defeated by a typed-nil interface.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func handle(msg message) (result any, rpcErr *rpcError) {
	switch msg.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "testplugin", "version": "0.1.0"},
		}
	case "tools/list":
		result = map[string]any{"tools": tools}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(msg.Params, &params) //nolint:errcheck
		switch params.Name {
		case "echo":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": string(params.Arguments)}}, "isError": false}
		case "sleep":
			time.Sleep(sleepDur)
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}, "isError": false}
		case "crash":
			os.Exit(1)
		case "traceid":
			id := ""
			if msg.Header != nil {
				id = msg.Header.Get("X-Nine-Request-ID")
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": id}}, "isError": false}
		default:
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "unknown tool"}}, "isError": true}
		}
	default:
		rpcErr = &rpcError{Code: -32601, Message: "method not found: " + msg.Method}
	}
	return result, rpcErr
}

// reply encodes one JSON-RPC response frame (or nothing for a notification).
func reply(w io.Writer, msg message, result any, rpcErr *rpcError) {
	if msg.ID == nil {
		return // notification: no reply
	}
	resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	data, _ := json.Marshal(resp)
	fmt.Fprintf(w, "%s\n", data)
}

func serveStdio() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4*1024*1024), 4*1024*1024)
	for scanner.Scan() {
		var msg message
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		result, rpcErr := handle(msg)
		reply(os.Stdout, msg, result, rpcErr)
	}
}

func serveSocket(socketPath string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var msg message
		if err := json.Unmarshal(body, &msg); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		msg.Header = r.Header
		if msg.ID == nil {
			w.WriteHeader(http.StatusAccepted) // notification
			return
		}
		result, rpcErr := handle(msg)
		resp := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	})

	os.Remove(socketPath) //nolint:errcheck
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testplugin: listen: %v\n", err)
		os.Exit(1)
	}
	if err := http.Serve(ln, mux); err != nil { //nolint:gosec // G114: test-only local socket server
		fmt.Fprintf(os.Stderr, "testplugin: serve: %v\n", err)
	}
}
