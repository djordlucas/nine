package plugin

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"syscall"
	"time"
)

// Process supervision helpers for spawned plugin children. They moved here from
// httpclient.go when the legacy envelope client was deleted at P4 — the socket
// start path still needs the single-reap watch and the socket-ready poll.

// stopGrace is how long stop waits for a SIGTERM'd plugin to exit before SIGKILL.
const stopGrace = 2 * time.Second

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
