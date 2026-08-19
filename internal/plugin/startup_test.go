package plugin

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildHelper compiles a tiny Go program to a temp path and returns it.
func buildHelper(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module helper\n\ngo 1.26\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "helper")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build helper: %v\n%s", err, out)
	}
	return bin
}

// A plugin that exits before listening must be reported as dead immediately,
// naming the exit — not as "socket not ready" after the full budget elapses.
//
// This is the defect the flake investigation turned up: the old wait polled
// blindly, so every startup failure looked like slowness and cost the whole
// timeout to discover.
func TestWaitForSocketDetectsProcessDeath(t *testing.T) {
	bin := buildHelper(t, `package main

import "os"

func main() { os.Exit(3) }
`)
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	watch := watchProcess(cmd)

	sock := filepath.Join(t.TempDir(), "never.sock")
	start := time.Now()
	err := waitForSocket(sock, 30*time.Second, watch)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForSocket succeeded against a process that exited without listening")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("error = %v, want it to report the process exit", err)
	}
	// The whole point: it must not sit out the budget.
	if elapsed > 5*time.Second {
		t.Errorf("took %v to notice a dead process; the budget is 30s and it should not be waited out", elapsed)
	}
}

// The budget still applies to a process that is alive but never listens —
// otherwise a hung plugin would block startup forever.
func TestWaitForSocketTimesOutOnLiveButSilentProcess(t *testing.T) {
	bin := buildHelper(t, `package main

import "time"

func main() { time.Sleep(time.Minute) }
`)
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	watch := watchProcess(cmd)
	t.Cleanup(func() { cmd.Process.Kill(); watch.wait() }) //nolint:errcheck

	sock := filepath.Join(t.TempDir(), "never.sock")
	err := waitForSocket(sock, 300*time.Millisecond, watch)
	if err == nil {
		t.Fatal("waitForSocket succeeded against a process that never listened")
	}
	if !strings.Contains(err.Error(), "not ready after") {
		t.Errorf("error = %v, want the timeout message", err)
	}
	if strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("error = %v, but the process was still alive", err)
	}
}

// procWatch owns the single Wait, so both startup and stop can read the exit
// status. Calling exec.Cmd.Wait twice is an error, which is what forced this.
func TestProcWatchFansOutOneWait(t *testing.T) {
	bin := buildHelper(t, `package main

import "os"

func main() { os.Exit(7) }
`)
	cmd := exec.Command(bin)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	watch := watchProcess(cmd)

	// Every reader sees the same result, however many times it is asked.
	first := watch.wait()
	for i := range 3 {
		if got := watch.wait(); got == nil || got.Error() != first.Error() {
			t.Fatalf("wait #%d = %v, want the same result as the first (%v)", i+2, got, first)
		}
	}
	if first == nil || !strings.Contains(first.Error(), "exit status 7") {
		t.Errorf("wait = %v, want it to carry exit status 7", first)
	}
	if !watch.exited() {
		t.Error("exited() = false after the process exited")
	}
}
