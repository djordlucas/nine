package main

import (
	"os"
	"path/filepath"
	"testing"
)

// setupLogger's return value is what stops the TUI from painting log lines over
// its own frame, so the stderr cases have to be reported as such. The container
// sets NINE_LOG_FILE=off (see the Makefile), which is exactly this path.
func TestSetupLoggerReportsStderrWhenFileLoggingIsOff(t *testing.T) {
	t.Setenv("NINE_LOG_FILE", "off")

	if !setupLogger() {
		t.Error("setupLogger() = false with NINE_LOG_FILE=off, want true (logs go to stderr)")
	}
}

// The complement: with the log file in play, stderr is clear and the TUI has no
// reason to silence anything.
func TestSetupLoggerReportsFileWhenLogFileOpens(t *testing.T) {
	t.Setenv("NINE_LOG_FILE", "")

	if setupLogger() {
		// os.Executable() under `go test` is the test binary, in a writable
		// temp dir, so the log file opens.
		t.Error("setupLogger() = true with file logging available, want false")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	logPath := filepath.Join(filepath.Dir(exe), "nine.log")
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("expected a log file at %s: %v", logPath, err)
	}
}
