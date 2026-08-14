package builtins

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
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

	// callMu serializes a whole request/response round trip: it owns the stream.
	// nextID rides with it because ids are only meaningful within one round trip.
	callMu sync.Mutex
	nextID int

	// stateMu guards closed, and is deliberately *not* callMu. A single mutex
	// deadlocks teardown: call() holds it across a blocking Scan, so stop() —
	// which is how a hung server is supposed to be interrupted — would wait on
	// the very read it is meant to unblock. The handshake timeout could then
	// never fire, and a server that accepts stdin without ever replying would
	// hang plugin.describe, and with it daemon boot, forever.
	stateMu sync.Mutex
	closed  bool

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
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.closed
}

// isClosed reports whether stop() has run.
func (c *mcpStdio) isClosed() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
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
	// Put the server in its own process group so the whole tree can be signalled
	// as one. `npx foo` is sh → npm → node: killing the direct child leaves the
	// node process running, and it accumulates one orphan per daemon restart.
	// Verified by finding exactly those orphans still alive after teardown.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
// call sends a JSON-RPC request and returns the raw result.
//
// ctx bounds the round trip, but stdio cannot abandon one in-flight request and
// resynchronize: the reply has no framing to skip past, so a request given up on
// would leave the next call reading someone else's answer. Cancellation
// therefore tears the connection down — the server is killed and the bridge
// follows it out (exitWhenServerDies) — which is a loud, correct failure rather
// than a stream silently out of step. HTTP has real per-request cancellation and
// does not need this.
func (c *mcpStdio) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	type result struct {
		raw json.RawMessage
		err error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := c.roundTrip(method, params)
		done <- result{raw, err}
	}()

	select {
	case r := <-done:
		return r.raw, r.err
	case <-ctx.Done():
		// stop() takes only stateMu, so it can interrupt a read blocked under
		// callMu — the whole reason those are separate locks.
		c.stop() //nolint:errcheck // best-effort teardown of a hung server
		return nil, fmt.Errorf("MCP %s: %w", method, ctx.Err())
	}
}

func (c *mcpStdio) roundTrip(method string, params any) (json.RawMessage, error) {
	c.callMu.Lock()
	defer c.callMu.Unlock()

	if c.isClosed() {
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
			Method string          `json:"method,omitempty"`
		}
		if err := json.Unmarshal(c.stdout.Bytes(), &raw); err != nil {
			return nil, fmt.Errorf("unmarshal response: %w", err)
		}
		// Skip anything that is not a reply to us. A notification has no id; a
		// server→client *request* (sampling, roots) has both an id and a method,
		// and without the method check it would be mistaken for our response and
		// return a nil result. We advertise no capabilities, so a well-behaved
		// server sends neither — but a reply frame is the wrong place to be
		// trusting.
		if raw.Method != "" {
			continue
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
	c.callMu.Lock()
	defer c.callMu.Unlock()

	if c.isClosed() {
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
	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		<-c.done
		return c.waitErr
	}
	c.closed = true
	c.stdin.Close() //nolint:errcheck // best-effort; closing stdin is how an MCP server is asked to exit
	c.stateMu.Unlock()

	// Closing stdin is a request, not a guarantee. An `npx` server is a shell
	// wrapping npm wrapping node, and that tree does not reliably exit when its
	// stdin goes away — leaving one orphaned process per daemon restart, which
	// is exactly what accumulated in testing. Kill after a grace period, the same
	// escalation the plugin manager applies to plugins themselves.
	select {
	case <-c.done:
	case <-time.After(mcpStopGrace):
		c.killGroup()
		<-c.done
	}
	// The direct child has been reaped, but a grandchild in the same group can
	// still be alive — `npx` exits while node keeps running. Sweep the group
	// either way, including on the clean path.
	c.killGroup()
	return c.waitErr
}

// killGroup signals the server's whole process group. Negating the pid is what
// makes it a group signal; it is a no-op once everything has exited.
func (c *mcpStdio) killGroup() {
	if c.cmd.Process == nil {
		return
	}
	syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL) //nolint:errcheck // best-effort; the group may already be gone
}

// mcpStopGrace is how long an MCP server gets to exit on its own after stdin
// closes, before it is killed.
const mcpStopGrace = 3 * time.Second
