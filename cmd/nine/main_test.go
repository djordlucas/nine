package main

import (
	"os"
	"path/filepath"
	"testing"
)

// With file logging in play, the log goes to nine.log beside the binary.
func TestSetupLoggerWritesTheLogFile(t *testing.T) {
	t.Setenv("NINE_LOG_FILE", "")

	// os.Executable() under `go test` is the test binary, in a writable temp
	// dir, so the log file opens.
	setupLogger()

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	logPath := filepath.Join(filepath.Dir(exe), "nine.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected a log file at %s: %v", logPath, err)
	}
}
