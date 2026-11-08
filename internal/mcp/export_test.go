package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	mcptypes "github.com/mark3labs/mcp-go/mcp"
)

// fakeExportClient is the daemon surface the export proxies, scripted.
type fakeExportClient struct {
	tools   []ExportTool
	listErr error
	calls   []string
	args    []string
	out     string
	callErr error
}

func (f *fakeExportClient) ListTools() ([]ExportTool, error) { return f.tools, f.listErr }

func (f *fakeExportClient) CallTool(tool string, args json.RawMessage, _ bool) (string, error) {
	f.calls = append(f.calls, tool)
	f.args = append(f.args, string(args))
	if f.callErr != nil {
		return "", f.callErr
	}
	return f.out, nil
}

type messageHandler interface {
	HandleMessage(context.Context, json.RawMessage) mcptypes.JSONRPCMessage
}

func call(t *testing.T, srv messageHandler, method, params string) mcptypes.JSONRPCMessage {
	t.Helper()
	msg := `{"jsonrpc":"2.0","id":1,"method":"` + method + `"` + params + `}`
	return srv.HandleMessage(context.Background(), json.RawMessage(msg))
}

// The gate from adr/modular_nine.md §10: tools/list equals the daemon's
// list_tools, and tools/call proxies plugin_call.
func TestExportServerListsAndProxies(t *testing.T) {
	cl := &fakeExportClient{
		tools: []ExportTool{
			{Plugin: "core", Name: "read_file", Description: "read", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Plugin: "shell", Name: "shell", Description: "run"},
		},
		out: "wrote /work/x",
	}
	srv, err := ExportServer("nine", "test", cl)
	if err != nil {
		t.Fatal(err)
	}

	resp := call(t, srv, "tools/list", "")
	result, ok := resp.(mcptypes.JSONRPCResponse)
	if !ok {
		t.Fatalf("tools/list reply is %T, want a response", resp)
	}
	raw, _ := json.Marshal(result.Result)
	var list struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 2 || list.Tools[0].Name != "read_file" || list.Tools[1].Name != "shell" {
		t.Fatalf("tools/list = %s", raw)
	}
	if list.Tools[0].Description != "read" || string(list.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Errorf("read_file entry = %+v, schema carried through unchanged", list.Tools[0])
	}

	resp = call(t, srv, "tools/call", `,"params":{"name":"shell","arguments":{"cmd":"echo hi"}}`)
	result, ok = resp.(mcptypes.JSONRPCResponse)
	if !ok {
		t.Fatalf("tools/call reply is %T, want a response", resp)
	}
	raw, _ = json.Marshal(result.Result)
	if !strings.Contains(string(raw), "wrote /work/x") {
		t.Errorf("tools/call result = %s, want the proxied output", raw)
	}
	if len(cl.calls) != 1 || cl.calls[0] != "shell" || cl.args[0] != `{"cmd":"echo hi"}` {
		t.Errorf("proxied call = %v %v, want shell with the raw arguments", cl.calls, cl.args)
	}
}

// A tool error comes back in-band (isError), the same failure shape the
// dispatched path gives the model.
func TestExportServerCallErrorIsInBand(t *testing.T) {
	cl := &fakeExportClient{
		tools:   []ExportTool{{Name: "shell"}},
		callErr: errors.New("unknown tool: nope"),
	}
	srv, err := ExportServer("nine", "test", cl)
	if err != nil {
		t.Fatal(err)
	}
	resp := call(t, srv, "tools/call", `,"params":{"name":"shell","arguments":{}}`)
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), `"isError":true`) || !strings.Contains(string(raw), "unknown tool: nope") {
		t.Errorf("error reply = %s, want isError with the daemon's message", raw)
	}
}

// Sampling and elicitation are refused: the capability is never advertised,
// so the request fails as an error.
func TestExportServerRefusesSamplingAndElicitation(t *testing.T) {
	cl := &fakeExportClient{tools: []ExportTool{{Name: "shell"}}}
	srv, err := ExportServer("nine", "test", cl)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{"sampling/createMessage", "elicitation/create"} {
		resp := call(t, srv, m, `,"params":{}`)
		errResp, ok := resp.(mcptypes.JSONRPCError)
		if !ok {
			t.Errorf("%s: reply is %T, want a JSON-RPC error", m, resp)
			continue
		}
		if errResp.Error.Code == 0 {
			t.Errorf("%s: error code 0, want a refusal", m)
		}
	}
}

// A resync replaces the registry: a tool added on the daemon appears, one
// removed disappears.
func TestExportSyncReplacesRoster(t *testing.T) {
	cl := &fakeExportClient{tools: []ExportTool{{Name: "a"}}}
	srv, err := ExportServer("nine", "test", cl)
	if err != nil {
		t.Fatal(err)
	}
	cl.tools = []ExportTool{{Name: "b"}, {Name: "c"}}
	if err := SyncExportTools(srv, cl); err != nil {
		t.Fatal(err)
	}
	got := srv.ListTools()
	if len(got) != 2 {
		t.Fatalf("after sync: %d tools, want 2", len(got))
	}
	if _, ok := got["a"]; ok {
		t.Error("tool a still registered after sync")
	}
	if _, ok := got["b"]; !ok {
		t.Error("tool b missing after sync")
	}

	cl.listErr = errors.New("daemon gone")
	if err := SyncExportTools(srv, cl); err == nil {
		t.Error("sync with a failing ListTools: nil error, want the failure")
	}
}
