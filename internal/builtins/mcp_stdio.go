package builtins

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
)

// This is the JSON-RPC 2.0 stdio client for talking to an MCP server. It used
// to live in internal/plugin as a second transport the manager had to know
// about, alongside the HTTP one every native plugin uses. It belongs here
// instead: MCP is now reached through a plugin like anything else, so stdio is
// one bridge's private implementation detail rather than a fork in the core
// (spec/contracts/plugin.md R-PLUG.15).
//
// Reads are serialized under a mutex because stdio has no per-message framing —
// there is no way to match an arbitrary response to a request without owning
// the stream for the round trip. That is why the bridge advertises
// max_concurrent: 1; the serialization is inherent to the transport, not a
// choice, and now it is declared where the daemon can see it.

// mcpScannerMaxBytes bounds a single JSON-RPC line. MCP responses carry tool
// output inline, which can be large.
const mcpScannerMaxBytes = 4 * 1024 * 1024

type mcpStdio struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
	nextID int
	closed bool

	// done closes when the server process has exited and been reaped; waitErr
	// holds why. Exactly one goroutine calls cmd.Wait (started in dialMCP), so
	// stop and the crash watchdog can both learn the outcome without racing on
	// a second Wait — and a server that dies is reaped rather than left a zombie.
	done    chan struct{}
	waitErr error
}

// Exited returns a channel closed when the MCP server process exits, for any
// reason. See mcpStdio.done.
func (c *mcpStdio) Exited() <-chan struct{} { return c.done }

// StoppedDeliberately reports whether the exit followed a stop() rather than a
// crash, so a watchdog can tell shutdown from failure.
func (c *mcpStdio) StoppedDeliberately() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// dialMCP spawns the MCP server and sets up its stdio pipes. env is the extra
// environment for the server process, on top of this bridge's own — which the
// daemon already sanitized, so a server sees Nine's secrets no more than any
// other plugin does.
func dialMCP(command string, args []string, env []string) (*mcpStdio, error) {
	cmd := exec.Command(command, args...) //nolint:gosec // operator-configured [[mcp.server]] command
	cmd.Env = append(os.Environ(), env...)
	// The server's own stderr is worth keeping: it is where an MCP server
	// explains why it refused to start, and it lands in the daemon's log.
	cmd.Stderr = os.Stderr

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start MCP server %q: %w", command, err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, mcpScannerMaxBytes), mcpScannerMaxBytes)

	c := &mcpStdio{cmd: cmd, stdin: stdinPipe, stdout: scanner, done: make(chan struct{})}
	go func() {
		c.waitErr = cmd.Wait()
		close(c.done)
	}()
	return c, nil
}

// call sends a JSON-RPC request and returns the raw result, skipping any
// notifications (messages with no id) that arrive before the response.
func (c *mcpStdio) call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, fmt.Errorf("MCP server stopped")
	}

	c.nextID++
	req := struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", ID: c.nextID, Method: method, Params: params}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	for {
		if !c.stdout.Scan() {
			if err := c.stdout.Err(); err != nil {
				return nil, fmt.Errorf("read response: %w", err)
			}
			return nil, fmt.Errorf("MCP server closed stdout")
		}
		var raw struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *mcpRPCError    `json:"error,omitempty"`
		}
		if err := json.Unmarshal(c.stdout.Bytes(), &raw); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		if len(raw.ID) == 0 || string(raw.ID) == "null" {
			continue // a notification, not our reply
		}
		if raw.Error != nil {
			return nil, fmt.Errorf("MCP error %d: %s", raw.Error.Code, raw.Error.Message)
		}
		return raw.Result, nil
	}
}

// notify sends a JSON-RPC notification: no id, no response expected. The MCP
// handshake requires exactly one (notifications/initialized).
func (c *mcpStdio) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("MCP server stopped")
	}
	notif := struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}{JSONRPC: "2.0", Method: method, Params: params}
	data, err := json.Marshal(notif)
	if err != nil {
		return err
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

// stop closes stdin and waits for the server to exit.
func (c *mcpStdio) stop() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.done
		return c.waitErr
	}
	c.closed = true
	c.stdin.Close() //nolint:errcheck // best-effort; closing stdin is how an MCP server is asked to exit
	c.mu.Unlock()

	<-c.done
	return c.waitErr
}
