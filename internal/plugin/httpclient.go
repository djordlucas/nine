package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"time"
)

// rpcURL is a fixed pseudo-URL; the transport's DialContext ignores the host and
// always dials the plugin's Unix socket, so "unix" is just a placeholder.
const rpcURL = "http://unix/rpc"

// stopGrace is how long stop waits for a SIGTERM'd plugin to exit before SIGKILL.
const stopGrace = 2 * time.Second

// defaultIdleConns keeps a warm idle pool for unbounded (max_concurrent=0) plugins
// so high-fan-out call bursts don't churn connections.
const defaultIdleConns = 64

// httpClient speaks the compact RPC envelope over HTTP on
// a per-plugin Unix socket. It is concurrency-safe by construction: http.Client
// pools connections, so there is no mutex, reader goroutine, or pending map.
type httpClient struct {
	name       string // plugin name, for log correlation
	cmd        *exec.Cmd
	watch      *procWatch // the single reaper for cmd; see procWatch
	socketPath string
	hc         *http.Client
	stopped    atomic.Bool
}

// procWatch owns the one permitted Wait on a spawned plugin process.
//
// exec.Cmd allows Wait to be called exactly once, which used to make the exit
// status unavailable to anyone but the caller who reaped it. Startup needs it
// (a plugin that dies before listening must be reported as dead, not as slow)
// and so does stop. Reaping once here and fanning the result out over a closed
// channel gives both, without either racing the other for the single Wait.
type procWatch struct {
	done chan struct{} // closed once the process has exited and been reaped
	err  error         // the Wait result; safe to read only after done is closed
}

// watchProcess reaps cmd in the background. Call it immediately after Start.
func watchProcess(cmd *exec.Cmd) *procWatch {
	w := &procWatch{done: make(chan struct{})}
	go func() {
		w.err = cmd.Wait()
		close(w.done) // publishes w.err to every later reader
	}()
	return w
}

// exited reports whether the process is already gone, without blocking.
func (w *procWatch) exited() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// wait blocks until the process exits and returns the Wait result.
func (w *procWatch) wait() error {
	<-w.done
	return w.err
}

// dialUnix returns a DialContext that always dials socketPath over AF_UNIX.
func dialUnix(socketPath string) func(context.Context, string, string) (net.Conn, error) {
	var d net.Dialer
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return d.DialContext(ctx, "unix", socketPath)
	}
}

// newHTTPClient builds the real per-plugin client. maxConcurrent maps onto
// MaxConnsPerHost (0 = unbounded).
func newHTTPClient(name, socketPath string, cmd *exec.Cmd, watch *procWatch, maxConcurrent int) *httpClient {
	idle := maxConcurrent
	if idle == 0 {
		idle = defaultIdleConns
	}
	return &httpClient{
		name:       name,
		cmd:        cmd,
		watch:      watch,
		socketPath: socketPath,
		hc: &http.Client{
			Transport: &http.Transport{
				DialContext:         dialUnix(socketPath),
				MaxConnsPerHost:     maxConcurrent,
				MaxIdleConnsPerHost: idle,
			},
		},
	}
}

func (c *httpClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if c.stopped.Load() {
		return nil, fmt.Errorf("plugin stopped")
	}
	// Ensure a trace ID on the ctx and log it daemon-side; the plugin logs the
	// same ID from the request header (see HeaderRequestID), so one ID greps
	// across both processes' logs.
	id := RequestIDFromContext(ctx)
	if id == "" {
		id = newRequestID()
		ctx = ContextWithRequestID(ctx, id)
	}
	slog.Debug("plugin call", "plugin", c.name, "method", method, "request_id", id)
	return postRPC(ctx, c.hc, method, params)
}

// stop SIGTERMs the plugin, waits with a kill-after-grace, closes idle
// connections, and unlinks the socket. Idempotent.
func (c *httpClient) stop() error {
	if c.stopped.Swap(true) {
		return nil
	}
	c.hc.CloseIdleConnections()

	if c.cmd.Process != nil {
		c.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort; fall through to Wait/Kill
	}

	// The process is reaped by its procWatch, not here: exec.Cmd permits only one
	// Wait, and startup already claimed it.
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

// isStopSignalErr reports whether err is a process exit caused by the SIGTERM or
// SIGKILL that stop sent.
func isStopSignalErr(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && (ws.Signal() == syscall.SIGTERM || ws.Signal() == syscall.SIGKILL)
}

// postRPC sends one request envelope to /rpc and decodes the reply envelope.
// Shared by httpClient.call and the throwaway describe client in the manager.
func postRPC(ctx context.Context, hc *http.Client, method string, params any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rpcURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if id := RequestIDFromContext(ctx); id != "" {
		req.Header.Set(HeaderRequestID, id)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	var reply struct {
		Result json.RawMessage `json:"result,omitempty"`
		Error  *rpcError       `json:"error,omitempty"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("plugin error %d: %s", reply.Error.Code, reply.Error.Message)
	}
	return reply.Result, nil
}

// waitForSocket dials the plugin socket until it accepts a connection, the
// process dies, or the budget expires.
//
// Watching for death matters as much as the timeout. A plugin that exits during
// startup — a missing shared library, a bad argument, an immediate panic — used
// to be reported only after the full budget elapsed, and reported as "socket not
// ready", which describes the symptom and hides the cause. Now it fails at once
// and says the process exited.
//
// The budget therefore only governs the remaining case: alive, but not listening
// yet. Waiting longer there costs nothing when things are healthy, because this
// returns as soon as the dial succeeds — it is a poll, not a sleep.
func waitForSocket(socketPath string, timeout time.Duration, watch *procWatch) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.Dial("unix", socketPath)
		if err == nil {
			conn.Close() //nolint:errcheck
			return nil
		}
		if watch != nil && watch.exited() {
			return fmt.Errorf("plugin exited during startup before listening on %s: %w",
				socketPath, watch.wait())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("plugin socket %s not ready after %s: %w", socketPath, timeout, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
