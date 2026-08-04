package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

const scannerMaxBytes = 4 * 1024 * 1024 // 4MB per response line

// pluginClient is the interface implemented by both the native client and the
// MCP adapter client. Plugin holds one of these; Manager operates through it.
//
// call takes a context: the native httpClient ties it to the HTTP request for
// real per-call cancellation/deadlines. The stdio client (MCP) can only honor it
// best-effort — a read already blocked under the mutex cannot be interrupted —
// so it checks ctx before sending.
type pluginClient interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	stop() error
}

// client is a JSON-RPC 2.0 client for a single plugin process.
type client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Scanner
	mu     sync.Mutex
	nextID int
	closed bool
}

// newClient spawns the binary with args and the given env vars appended to the
// current process environment, then sets up stdin/stdout pipes for JSON-RPC.
func newClient(binaryPath string, args []string, extraEnv []string) (*client, error) {
	cmd := exec.Command(binaryPath, args...)
	cmd.Env = append(sanitizedHostEnv(), extraEnv...)

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start plugin: %w", err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, scannerMaxBytes), scannerMaxBytes)

	return &client{
		cmd:    cmd,
		stdin:  stdinPipe,
		stdout: scanner,
	}, nil
}

// call sends a JSON-RPC request and returns the raw result, skipping any
// incoming notifications (messages without an id) before the response arrives.
func (c *client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Best-effort: honor an already-cancelled ctx. Once the read below blocks it
	// cannot be interrupted (stdio, no per-message framing), so this is the only
	// cancellation point. Native plugins use httpClient for real cancellation.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.closed {
		return nil, fmt.Errorf("plugin stopped")
	}

	c.nextID++
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      c.nextID,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write request: %w", err)
	}

	// Read lines until we get one with a non-null id (skip notifications).
	for {
		if !c.stdout.Scan() {
			if err := c.stdout.Err(); err != nil {
				return nil, fmt.Errorf("read response: %w", err)
			}
			return nil, fmt.Errorf("plugin closed stdout")
		}

		var raw struct {
			ID     json.RawMessage `json:"id"`
			Result json.RawMessage `json:"result,omitempty"`
			Error  *rpcError       `json:"error,omitempty"`
		}
		if err := json.Unmarshal(c.stdout.Bytes(), &raw); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		// Notifications have no id or null id — skip them.
		if len(raw.ID) == 0 || string(raw.ID) == "null" {
			continue
		}
		if raw.Error != nil {
			return nil, fmt.Errorf("plugin error %d: %s", raw.Error.Code, raw.Error.Message)
		}
		return raw.Result, nil
	}
}

// sendNotification sends a JSON-RPC notification (no id, no response expected).
func (c *client) sendNotification(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return fmt.Errorf("plugin stopped")
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
	data = append(data, '\n')
	_, err = c.stdin.Write(data)
	return err
}

// stop closes stdin and waits for the process to exit.
func (c *client) stop() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.stdin.Close()
	c.mu.Unlock()

	return c.cmd.Wait()
}
