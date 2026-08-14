package builtins_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"nine/internal/plugin"
)

// The streamable-HTTP transport, driven end to end: a real bridge process, in
// its own plugin instance, talking to a stand-in MCP server over HTTP. The
// server here answers initialize with a plain JSON body and tools/call as an
// SSE stream, because a real server chooses per request and a client that only
// handles one shape works until it doesn't.

// fakeMCPHTTP is a minimal streamable-HTTP MCP server. It records the headers it
// saw so the session and auth plumbing can be asserted.
type fakeMCPHTTP struct {
	mu       sync.Mutex
	sessions []string // Mcp-Session-Id seen per request
	auth     []string // Authorization seen per request
	versions []string // MCP-Protocol-Version seen per request
}

func (f *fakeMCPHTTP) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int            `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		f.mu.Lock()
		f.sessions = append(f.sessions, r.Header.Get("Mcp-Session-Id"))
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.versions = append(f.versions, r.Header.Get("MCP-Protocol-Version"))
		f.mu.Unlock()

		// Notifications carry no id and are acknowledged with 202 and no body.
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}

		switch req.Method {
		case "initialize":
			// Issue a session the client must echo on every later request.
			w.Header().Set("Mcp-Session-Id", "session-abc")
			writeJSONRPC(w, *req.ID, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fake", "version": "1"},
			})

		case "tools/list":
			writeJSONRPC(w, *req.ID, map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "echo back the message",
					"inputSchema": map[string]any{"type": "object"},
				}},
			})

		case "tools/call":
			// SSE this time: a keep-alive comment, an unrelated notification, and
			// then the actual reply — the client has to walk past the first two.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, ": keep-alive\n\n")
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			body, _ := json.Marshal(map[string]any{
				"jsonrpc": "2.0",
				"id":      *req.ID,
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": "echoed over SSE"}},
				},
			})
			fmt.Fprintf(w, "data: %s\n\n", body)

		default:
			writeJSONRPCError(w, *req.ID, -32601, "method not found: "+req.Method)
		}
	}
}

func writeJSONRPC(w http.ResponseWriter, id int, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
		"jsonrpc": "2.0", "id": id, "result": result,
	})
}

func writeJSONRPCError(w http.ResponseWriter, id, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck // test server
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
}

func startHTTPBridge(t *testing.T, name string) (*plugin.Plugin, *plugin.Manager, *fakeMCPHTTP) {
	t.Helper()

	fake := &fakeMCPHTTP{}
	srv := httptest.NewServer(fake.handler(t))
	t.Cleanup(srv.Close)

	spec, err := json.Marshal(map[string]any{
		"name":    name,
		"url":     srv.URL,
		"headers": map[string]string{"Authorization": "Bearer test-token"},
	})
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}

	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartBuiltinInstance("mcp", plugin.MCPInstanceName(name), "NINE_MCP_SERVER="+string(spec))
	if err != nil {
		t.Fatalf("start http mcp bridge: %v", err)
	}
	t.Cleanup(func() { m.Stop(p) }) //nolint:errcheck // test cleanup
	return p, m, fake
}

// A hosted server is a plugin exactly like a spawned one: same instance name,
// same prefixing, same contract.
func TestMCPHTTPBridgeDescribe(t *testing.T) {
	p, _, _ := startHTTPBridge(t, "hosted")

	if p.Name != "mcp:hosted" {
		t.Errorf("plugin name = %q, want mcp:hosted", p.Name)
	}
	if len(p.Tools) != 1 || p.Tools[0].Name != "hosted__echo" {
		t.Fatalf("tools = %+v, want [hosted__echo]", p.Tools)
	}
}

// tools/call comes back as SSE here, with a keep-alive and an unrelated
// notification ahead of the answer. Reading the first data frame, or treating a
// notification as the reply, would both produce the wrong result.
func TestMCPHTTPBridgeCallOverSSE(t *testing.T) {
	p, m, _ := startHTTPBridge(t, "hosted")

	res, err := m.Call(context.Background(), p, "hosted__echo", json.RawMessage(`{"message":"hi"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Output != "echoed over SSE" {
		t.Errorf("output = %q, want %q", res.Output, "echoed over SSE")
	}
}

// The session a server issues on initialize has to ride every later request, or
// a stateful server rejects them.
func TestMCPHTTPBridgeEchoesSession(t *testing.T) {
	p, m, fake := startHTTPBridge(t, "hosted")

	if _, err := m.Call(context.Background(), p, "hosted__echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.sessions) < 2 {
		t.Fatalf("expected several requests, saw %d", len(fake.sessions))
	}
	// The first request is initialize, before any session exists.
	if fake.sessions[0] != "" {
		t.Errorf("initialize carried a session id %q; none had been issued yet", fake.sessions[0])
	}
	for i, got := range fake.sessions[1:] {
		if got != "session-abc" {
			t.Errorf("request %d carried session %q, want session-abc", i+1, got)
		}
	}
}

// Operator headers are what authenticate a hosted server, so they must be on
// every request, not just the handshake.
func TestMCPHTTPBridgeSendsHeaders(t *testing.T) {
	p, m, fake := startHTTPBridge(t, "hosted")

	if _, err := m.Call(context.Background(), p, "hosted__echo", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Call: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	for i, got := range fake.auth {
		if got != "Bearer test-token" {
			t.Errorf("request %d Authorization = %q, want the configured bearer", i, got)
		}
	}
	for i, got := range fake.versions {
		if !strings.HasPrefix(got, "20") {
			t.Errorf("request %d MCP-Protocol-Version = %q, want a protocol revision", i, got)
		}
	}
}

// An unreachable URL must fail the bridge rather than yield a plugin with no
// tools, the same as an unspawnable command.
func TestMCPHTTPBridgeUnreachableFails(t *testing.T) {
	// Port 0 is never listening.
	spec, _ := json.Marshal(map[string]any{"name": "down", "url": "http://127.0.0.1:0/rpc"})

	m := plugin.NewManager("")
	m.SetBuiltinBinary(nineBin)
	p, err := m.StartBuiltinInstance("mcp", plugin.MCPInstanceName("down"), "NINE_MCP_SERVER="+string(spec))
	if err == nil {
		m.Stop(p) //nolint:errcheck // cleanup on unexpected success
		t.Fatal("bridge started against an unreachable URL; want failure")
	}
}
