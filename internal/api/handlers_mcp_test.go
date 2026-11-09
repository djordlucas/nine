package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nine/internal/config"
	"nine/internal/protocol"
)

// The MCP export endpoint (adr/modular_nine.md §6, P6): /api/v1/mcp serves the
// daemon's roster as MCP streamable HTTP. The daemon here is scripted: it
// answers list_tools and tool_call, and the test drives the endpoint with
// JSON-RPC over HTTP the way an external MCP client would.

func mcpServer(t *testing.T, sock string, authToken string) http.Handler {
	t.Helper()
	return NewServer(Config{
		APIConfig:  config.APIConfig{AuthToken: authToken},
		SocketPath: sock,
		Version:    "test",
		StartTime:  time.Now(),
	}).createHandler()
}

// mcpPost sends one JSON-RPC request to the MCP endpoint and returns the
// response body. Stateless mode answers either plain JSON or an SSE frame;
// both carry the response as JSON.
func mcpPost(t *testing.T, h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, mcpPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// rpcBody extracts the JSON-RPC response object from a plain-JSON or SSE reply.
func rpcBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	body := w.Body.String()
	if strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(line, "data: ") {
				body = strings.TrimPrefix(line, "data: ")
				break
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("reply is not JSON (%s): %s", w.Header().Get("Content-Type"), body)
	}
	return out
}

func TestMCPExportEndpoint(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		switch m.Type {
		case protocol.TypeListTools:
			return textReply(t, protocol.TypeListTools, []protocol.ToolSummary{
				{Plugin: "core", Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
				{Plugin: "shell", Name: "shell", Description: "run"},
			})
		case protocol.TypeToolCall:
			if m.ToolName != "shell" {
				return errReply("unknown tool: " + m.ToolName)
			}
			return textReply(t, protocol.TypeToolCall, "pong")
		}
		return errReply("unexpected " + string(m.Type))
	})
	h := mcpServer(t, sock, "")

	// initialize
	w := mcpPost(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("initialize: %d %s", w.Code, w.Body.String())
	}
	if resp := rpcBody(t, w); resp["result"] == nil {
		t.Fatalf("initialize reply has no result: %v", resp)
	}

	// tools/list equals the daemon's list_tools
	w = mcpPost(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/list: %d %s", w.Code, w.Body.String())
	}
	raw, _ := json.Marshal(rpcBody(t, w))
	if !strings.Contains(string(raw), `"name":"read_file"`) || !strings.Contains(string(raw), `"name":"shell"`) {
		t.Errorf("tools/list = %s, want the daemon roster", raw)
	}

	// tools/call proxies plugin_call
	w = mcpPost(t, h, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shell","arguments":{"cmd":"echo hi"}}}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("tools/call: %d %s", w.Code, w.Body.String())
	}
	raw, _ = json.Marshal(rpcBody(t, w))
	if !strings.Contains(string(raw), "pong") {
		t.Errorf("tools/call = %s, want the proxied output", raw)
	}

	// sampling is refused
	w = mcpPost(t, h, `{"jsonrpc":"2.0","id":4,"method":"sampling/createMessage","params":{}}`, nil)
	resp := rpcBody(t, w)
	if resp["error"] == nil {
		t.Errorf("sampling/createMessage reply = %v, want a JSON-RPC error", resp)
	}
}

// The exported surface authenticates like the rest of the API: with a token
// configured, a request without it is refused before it reaches the MCP server.
func TestMCPExportEndpointRequiresAuth(t *testing.T) {
	sock := scriptedDaemon(t, func(m protocol.Msg) []protocol.Msg {
		return errReply("unexpected " + string(m.Type))
	})
	h := mcpServer(t, sock, "secret")

	w := mcpPost(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`, nil)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", w.Code)
	}

	w = mcpPost(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		map[string]string{"Authorization": "Bearer secret"})
	if w.Code != http.StatusOK {
		t.Errorf("with token: status %d, want 200 (%s)", w.Code, w.Body.String())
	}
}
