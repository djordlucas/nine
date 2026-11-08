package mcp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/mcp"
)

// echoTool is a test tool: it echoes its arguments back as text.
func echoTool() (mcp.ServerTool, mcp.Handler) {
	schema := json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}}}`)
	return mcp.ServerTool{Name: "echo", Description: "Echoes arguments", InputSchema: schema},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			return string(args), nil
		}
}

// failTool always fails; the failure must reach the client as a Go error, the
// same way the legacy envelope reports a handler error.
func failTool() (mcp.ServerTool, mcp.Handler) {
	schema := json.RawMessage(`{"type":"object"}`)
	return mcp.ServerTool{Name: "fail", Description: "Always fails", InputSchema: schema},
		func(ctx context.Context, args json.RawMessage) (string, error) {
			return "", errors.New("boom: deliberate failure")
		}
}

// The server side serves tools over streamable HTTP on a Unix socket, and the
// hand-rolled client handshakes it: tools/list carries the raw schema through,
// tools/call round-trips, and a handler error comes back as an error.
func TestServeHTTPOverUnixSocket(t *testing.T) {
	echo, echoH := echoTool()
	fail, failH := failTool()
	srv := mcp.NewServer("test", "0.1.0", []mcp.ServerTool{echo, fail},
		map[string]mcp.Handler{"echo": echoH, "fail": failH})

	// Short path: macOS caps socket paths at 104 bytes, so not t.TempDir().
	dir, err := os.MkdirTemp("/tmp", "ninemcp")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck
	sock := filepath.Join(dir, "s.sock")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go mcp.ServeHTTP(ln, srv) //nolint:errcheck // test server; closed via ln.Close below
	defer ln.Close()          //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, tools, err := mcp.ConnectSocket(ctx, sock, "test-client")
	if err != nil {
		t.Fatalf("ConnectSocket: %v", err)
	}
	defer conn.Stop() //nolint:errcheck

	if len(tools) != 2 {
		t.Fatalf("tools = %+v, want echo and fail", tools)
	}
	var echoT *mcp.Tool
	for i := range tools {
		if tools[i].Name == "echo" {
			echoT = &tools[i]
		}
	}
	if echoT == nil {
		t.Fatal("echo tool missing from tools/list")
	}
	if string(echoT.InputSchema) != string(echo.InputSchema) {
		t.Errorf("input schema = %s, want carried through unchanged: %s", echoT.InputSchema, echo.InputSchema)
	}

	args := json.RawMessage(`{"message":"hi"}`)
	out, err := mcp.CallTool(ctx, conn, "echo", args)
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if out != string(args) {
		t.Errorf("echo output = %q, want %q", out, args)
	}

	if _, err := mcp.CallTool(ctx, conn, "fail", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("CallTool fail err = %v, want it to carry the handler's error", err)
	}
}

// The stdio transport serves the same server over pipes; the frames are plain
// newline-delimited JSON-RPC.
func TestServeStdioOverPipes(t *testing.T) {
	echo, echoH := echoTool()
	srv := mcp.NewServer("test", "0.1.0", []mcp.ServerTool{echo}, map[string]mcp.Handler{"echo": echoH})

	clientInR, clientInW, err := os.Pipe() // server reads requests here
	if err != nil {
		t.Fatal(err)
	}
	serverOutR, serverOutW, err := os.Pipe() // client reads responses here
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		mcp.ServeStdio(ctx, clientInR, serverOutW, srv) //nolint:errcheck // ends with ctx
	}()

	send := func(id int, method string, params any) {
		t.Helper()
		req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		fmt.Fprintf(clientInW, "%s\n", req)
	}
	readResult := func(wantID int) map[string]json.RawMessage {
		t.Helper()
		sc := bufio.NewScanner(serverOutR)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if !sc.Scan() {
				t.Fatalf("no response frame: %v", sc.Err())
			}
			var msg map[string]json.RawMessage
			if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
				continue
			}
			var id int
			if err := json.Unmarshal(msg["id"], &id); err != nil || id != wantID {
				continue
			}
			return msg
		}
		t.Fatalf("no response to id %d within budget", wantID)
		return nil
	}

	send(1, "initialize", map[string]any{
		"protocolVersion": mcp.ProtocolVersionStdio,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if msg := readResult(1); msg["result"] == nil {
		t.Fatalf("initialize response = %v, want a result", msg)
	}

	// notifications/initialized has no id; the server answers 202-equivalent (nothing).
	req, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	fmt.Fprintf(clientInW, "%s\n", req)

	send(2, "tools/list", map[string]any{})
	msg := readResult(2)
	var listed struct {
		Result struct {
			Tools []mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(msg["result"], &listed.Result); err != nil {
		t.Fatalf("parse tools/list: %v", err)
	}
	if len(listed.Result.Tools) != 1 || listed.Result.Tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, want [echo]", listed.Result.Tools)
	}

	send(3, "tools/call", map[string]any{"name": "echo", "arguments": map[string]any{"message": "hi"}})
	msg = readResult(3)
	var callRes struct {
		Result struct {
			Content []mcp.Content `json:"content"`
			IsError bool          `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(msg["result"], &callRes.Result); err != nil {
		t.Fatalf("parse tools/call: %v", err)
	}
	if callRes.Result.IsError || len(callRes.Result.Content) != 1 || callRes.Result.Content[0].Text != `{"message":"hi"}` {
		t.Errorf("tools/call = %+v, want one text part echoing the arguments", callRes.Result)
	}
}
