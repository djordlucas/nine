package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"

	"nine/internal/mcp"
)

// mcpClient reaches a plugin process that speaks MCP streamable HTTP on its
// Unix socket (adr/modular_nine.md, R-PLUG.16): the manager handshakes MCP
// first and only falls back to the legacy envelope. It maps the plugin
// contract's methods onto MCP: plugin.call becomes tools/call with the tool's
// bare name (first-party tools are never prefixed, §9.9). Jobs are not part of
// MCP's core, so an MCP plugin advertises async_jobs: false and the job methods
// fail closed, exactly as the old bridge declared.
type mcpClient struct {
	name       string // plugin name, for log correlation
	cmd        *exec.Cmd
	watch      *procWatch // the single reaper for cmd; see procWatch
	socketPath string
	conn       mcp.Conn
	stopped    atomic.Bool
}

func newMCPClient(name, socketPath string, cmd *exec.Cmd, watch *procWatch, conn mcp.Conn) *mcpClient {
	return &mcpClient{name: name, cmd: cmd, watch: watch, socketPath: socketPath, conn: conn}
}

// call implements pluginClient.
func (c *mcpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.stopped.Load() {
		return nil, fmt.Errorf("plugin stopped")
	}
	switch method {
	case "plugin.call":
		req, ok := params.(CallRequest)
		if !ok {
			return nil, fmt.Errorf("plugin.call: unexpected params type %T", params)
		}
		slog.Debug("plugin call", "plugin", c.name, "method", "tools/call", "tool", req.Tool)
		// The daemon's ctx carries task_timeout, and a call bounded by nothing
		// is how one hung server wedges every later call to it.
		ctx, cancel := context.WithTimeout(ctx, mcp.CallTimeout)
		defer cancel()
		out, err := mcp.CallTool(ctx, c.conn, req.Tool, req.Args)
		if err != nil {
			return nil, err
		}
		return json.Marshal(CallResult{Output: out})
	case "plugin.job_status", "plugin.job_cancel":
		return nil, fmt.Errorf("plugin does not support jobs")
	default:
		return nil, fmt.Errorf("method not found: %s", method)
	}
}

// stop tears down the MCP connection and then the plugin process, mirroring
// httpClient.stop. Idempotent.
func (c *mcpClient) stop() error {
	if c.stopped.Swap(true) {
		return nil
	}
	c.conn.Stop() //nolint:errcheck // best-effort; the SIGTERM below is the real stop

	if c.cmd.Process != nil {
		c.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort; fall through to Wait/Kill
	}

	done := make(chan error, 1)
	go func() { done <- c.watch.wait() }()

	var err error
	select {
	case err = <-done:
	case <-time.After(stopGrace):
		c.cmd.Process.Kill() //nolint:errcheck // best-effort
		err = <-done
	}
	os.Remove(c.socketPath) //nolint:errcheck // best-effort socket cleanup

	// We asked the process to stop, so an exit caused by our own SIGTERM/SIGKILL
	// is a clean stop, not a failure (a plugin need not trap SIGTERM).
	if isStopSignalErr(err) {
		return nil
	}
	return err
}
