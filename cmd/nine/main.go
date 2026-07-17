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
	setupLogger()

	cfg := config.LoadDefault()

	c := cli.New(os.Args[0])
	c.Version = Version
	c.StartDaemon = runDaemon
	c.StartTUI = func(attachID string) error {
		return tui.Run(cfg.SocketPath(), os.Args[0], cfg, attachID)
	}

	if err := c.Run(os.Args[1:], cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// setupLogger configures the global slog logger.
//
// Log destination (in priority order):
//  1. NINE_LOG_FILE=off  → stderr only (disables file logging)
//  2. default             → <binary-dir>/nine.log (appended)
//
// Other env vars:
//
//	NINE_LOG_LEVEL=debug   verbose output
//	NINE_LOG_FORMAT=json   machine-readable JSON
func setupLogger() {
	level := slog.LevelInfo
	if v := os.Getenv("NINE_LOG_LEVEL"); v != "" {
		var l slog.Level
		if err := l.UnmarshalText([]byte(v)); err == nil {
			level = l
		}
	}

	var w io.Writer = os.Stderr

	if os.Getenv("NINE_LOG_FILE") != "off" {
		if exePath, err := os.Executable(); err == nil {
			logPath := filepath.Join(filepath.Dir(exePath), "nine.log")
			if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600); err == nil {
				w = f // file stays open for process lifetime
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
}
