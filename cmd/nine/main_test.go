package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A client command never creates the log file; the daemon's log goes to
// nine.log beside the binary.
func TestOnlyTheDaemonWritesTheLogFile(t *testing.T) {
	t.Setenv("NINE_LOG_FILE", "")
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	// os.Executable() under `go test` is the test binary, in a writable temp
	// dir, so the log file opens.
	logPath := filepath.Join(filepath.Dir(exe), "nine.log")
	_ = os.Remove(logPath)

	setupLogger(false)
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Errorf("a client command created %s (err %v)", logPath, err)
	}

	setupLogger(true)
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected the daemon's log file at %s: %v", logPath, err)
	}
}
