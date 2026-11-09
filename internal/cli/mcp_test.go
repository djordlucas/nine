package cli_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nine/internal/cli"
	"nine/internal/protocol"
)

// scriptedWireDaemon listens on sock and answers the wire messages the export
// proxy sends: list_tools with a fixed roster, tool_call with "pong".
func scriptedWireDaemon(t *testing.T, sock string) {
	t.Helper()
	os.Remove(sock) //nolint:errcheck
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })      //nolint:errcheck
	t.Cleanup(func() { os.Remove(sock) }) //nolint:errcheck
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close() //nolint:errcheck
				sc := bufio.NewScanner(conn)
				enc := json.NewEncoder(conn)
				for sc.Scan() {
					var m protocol.Msg
					if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
						continue
					}
					switch m.Type {
					case protocol.TypeListTools:
						data, _ := json.Marshal([]protocol.ToolSummary{
							{Plugin: "core", Name: "read_file", Description: "read"},
							{Plugin: "shell", Name: "shell", Description: "run"},
						})
						_ = enc.Encode(protocol.NewTextMsg(protocol.TypeListTools, string(data)))
					case protocol.TypeToolCall:
						_ = enc.Encode(protocol.NewTextMsg(protocol.TypeToolCall, "pong"))
					default:
						_ = enc.Encode(protocol.NewErrorMsg("unexpected " + string(m.Type)))
					}
				}
			}()
		}
	}()
}

// `nine mcp export` serves the daemon's roster over stdio: an external MCP
// client's tools/list is the daemon's list_tools and its tools/call proxies
// plugin_call (adr/modular_nine.md §6, P6).
func TestMCPExportServesDaemonRosterOverStdio(t *testing.T) {
	sock := filepath.Join("/tmp", fmt.Sprintf("nine-mcpexport-test-%d.sock", os.Getpid()))
	scriptedWireDaemon(t, sock)

	dir := t.TempDir()
	cfg, _ := cfgAt(t, dir)
	cfg.Daemon.SocketPath = sock

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &cli.CLI{Out: outW, Err: io.Discard, In: inR, Binary: "nine"}

	done := make(chan error, 1)
	go func() { done <- c.MCPExport(cfg) }()

	// Drive it like an MCP client: newline-delimited JSON-RPC on the pipes.
	write := func(s string) {
		t.Helper()
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	readResp := func() map[string]any {
		t.Helper()
		lineCh := make(chan string, 1)
		go func() {
			line, err := bufio.NewReader(outR).ReadString('\n')
			if err == nil {
				lineCh <- line
			}
		}()
		select {
		case line := <-lineCh:
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("response is not JSON: %q", line)
			}
			return m
		case <-time.After(10 * time.Second):
			t.Fatal("no response from the export server")
			return nil
		}
	}

	write(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	if r := readResp(); r["result"] == nil {
		t.Fatalf("initialize reply = %v", r)
	}
	write(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
	raw, _ := json.Marshal(readResp())
	if !strings.Contains(string(raw), `"name":"read_file"`) || !strings.Contains(string(raw), `"name":"shell"`) {
		t.Errorf("tools/list = %s, want the daemon roster", raw)
	}
	write(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shell","arguments":{"cmd":"echo hi"}}}`)
	raw, _ = json.Marshal(readResp())
	if !strings.Contains(string(raw), "pong") {
		t.Errorf("tools/call = %s, want the proxied output", raw)
	}
	write(`{"jsonrpc":"2.0","id":4,"method":"sampling/createMessage","params":{}}`)
	if r := readResp(); r["error"] == nil {
		t.Errorf("sampling reply = %v, want a refusal", r)
	}

	inW.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("export server did not stop when stdin closed")
	}
}
