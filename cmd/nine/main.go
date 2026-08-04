package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"nine/internal/cli"
	"nine/internal/config"
	"nine/internal/tui"
)

// Version is the release version. It is overridden at build time via
// -ldflags "-X main.Version=$(VERSION)" (see the Makefile); the default below
// applies to plain `go build` / `go run` invocations.
var Version = "dev"

func main() {
	logsToStderr := setupLogger()

	cfg := config.LoadDefault()

	c := cli.New(os.Args[0])
	c.Version = Version
	c.StartDaemon = runDaemon
	c.StartTUI = func(attachID string) error {
		// The TUI renders a full-screen UI over this terminal, so stderr is a
		// display surface here, not a log sink — a single slog line painted
		// into the chat area corrupts the frame. When that is where logs are
		// going (NINE_LOG_FILE=off, or the log file could not be opened), drop
		// them for the life of the TUI. Nothing is lost that matters: the
		// client side logs almost nothing, and the daemon is a separate
		// process with its own destination.
		if logsToStderr {
			slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		}
		return tui.Run(cfg.SocketPath(), os.Args[0], cfg, attachID, Version)
	}

	if err := c.Run(os.Args[1:], cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// setupLogger configures the global slog logger and reports whether it ended up
// writing to stderr — which callers that own the terminal (the TUI) must know,
// since for them stderr is the screen.
//
// Log destination (in priority order):
//  1. NINE_LOG_FILE=off  → stderr only (disables file logging)
//  2. default             → <binary-dir>/nine.log (appended)
//
// The file is a best-effort default: if it cannot be opened — an installed
// binary in a directory the user cannot write to is the common case — this
// silently falls back to stderr, so the stderr case is not only the explicit
// opt-out.
//
// Other env vars:
//
//	NINE_LOG_LEVEL=debug   verbose output
//	NINE_LOG_FORMAT=json   machine-readable JSON
func setupLogger() (logsToStderr bool) {
	level := slog.LevelInfo
	if v := os.Getenv("NINE_LOG_LEVEL"); v != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(v)); err == nil {
			level = l
		}
	}

	var w io.Writer = os.Stderr
	logsToStderr = true

	if os.Getenv("NINE_LOG_FILE") != "off" {
		if exePath, err := os.Executable(); err == nil {
			logPath := filepath.Join(filepath.Dir(exePath), "nine.log")
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				w = f // file stays open for process lifetime
				logsToStderr = false
			}
		}
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if os.Getenv("NINE_LOG_FORMAT") == "json" {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	slog.SetDefault(slog.New(h))
	slog.Info("nine", "version", Version)
	return logsToStderr
}
